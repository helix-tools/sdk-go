// Package consumer provides functionality for consuming datasets from the Helix Connect Platform.
//
// It handles the entire lifecycle of dataset consumption, including authentication,
// downloading, decrypting, and decompressing datasets. It also provides mechanisms
// to poll and acknowledge dataset upload notifications via SQS.
//
// TODO: Use thalesfsp/sypl logger, and set log levels to `debug`.
package consumer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/helix-tools/sdk-go/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// emptyPayloadHash is the SHA256 hash of an empty payload.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Consumer handles downloading and managing datasets from Helix Connect platform.
type Consumer struct {
	APIEndpoint string
	CustomerID  string
	Region      string

	awsConfig  aws.Config
	httpClient *http.Client
	kmsClient  *kms.Client
	queueURL   *string // Cache for per-consumer queue URL.
	sqsClient  *sqs.Client
	ssmClient  *ssm.Client
}

// DownloadURLInfo contains information about a dataset download URL.
// Note: Go API returns file_name, file_size instead of nested dataset object.
type DownloadURLInfo struct {
	DownloadURL string `json:"download_url"`
	ExpiresAt   string `json:"expires_at"`
	FileName    string `json:"file_name,omitempty"`
	FileSize    int64  `json:"file_size,omitempty"`
	// Legacy nested dataset object (for backward compatibility).
	Dataset *struct {
		ID        string `json:"_id"`
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"dataset,omitempty"`
}

// Dataset represents a dataset in the catalog.
type Dataset struct {
	ID       string `json:"_id"`
	Name     string `json:"name"`
	Metadata struct {
		CompressionEnabled bool `json:"compression_enabled"`
		EncryptionEnabled  bool `json:"encryption_enabled"`
	} `json:"metadata"`
}

// Notification represents a dataset upload notification received from SQS.
type Notification struct {
	DatasetID      string `json:"dataset_id"`
	DatasetName    string `json:"dataset_name,omitempty"`
	EventType      string `json:"event_type"`
	MessageID      string `json:"message_id"`
	ProducerID     string `json:"producer_id"`
	RawMessage     string `json:"raw_message"`
	ReceiptHandle  string `json:"receipt_handle"`
	S3Bucket       string `json:"s3_bucket"`
	S3Key          string `json:"s3_key"`
	SizeBytes      int64  `json:"size_bytes"`
	SubscriberID   string `json:"subscriber_id"`
	SubscriptionID string `json:"subscription_id"`
	Timestamp      string `json:"timestamp"`
}

// Subscription is an alias for types.Subscription for backward compatibility.
// Use types.Subscription directly for new code.
type Subscription = types.Subscription

// PollNotificationsOptions contains options for polling notifications from SQS.
type PollNotificationsOptions struct {
	AutoAcknowledge *bool    // Automatically acknowledge (delete) messages after receiving (default: true)
	MaxMessages     int32    // Maximum number of messages to retrieve (1-10, default: 10)
	SubscriptionIDs []string // Optional list of subscription IDs to filter notifications

	// Long polling wait time (0-20 seconds, default: 20)
	//
	// TODO: Get pattern from AWS SSM.
	WaitTimeSeconds int32
}

// NewConsumer creates a new Consumer instance.
//
// TODO: Allow to pass context for better control.
func NewConsumer(cfg types.Config) (*Consumer, error) {
	// Basic validation.
	//
	if cfg.APIEndpoint == "" {
		// TODO: Get this from AWS SSM.
		envEndpoint := strings.TrimSpace(os.Getenv("HELIX_API_ENDPOINT"))
		if envEndpoint != "" {
			cfg.APIEndpoint = envEndpoint
		} else {
			cfg.APIEndpoint = "https://api-go.helix.tools"
		}
	}

	if cfg.Region == "" {
		// TODO: Get this from AWS SSM.
		cfg.Region = "us-east-1"
	}

	awsHTTPClient := &http.Client{
		Timeout: 25 * time.Second,
	}

	// Load AWS config.
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AWSAccessKeyID,
			cfg.AWSSecretAccessKey,
			"",
		)),
		config.WithHTTPClient(awsHTTPClient),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Validate credentials.
	stsClient := sts.NewFromConfig(awsCfg)
	if _, err = stsClient.GetCallerIdentity(
		context.Background(),
		&sts.GetCallerIdentityInput{},
	); err != nil {
		return nil, fmt.Errorf("invalid AWS credentials: %w", err)
	}

	return &Consumer{
		APIEndpoint: cfg.APIEndpoint,
		CustomerID:  cfg.CustomerID,
		Region:      cfg.Region,

		awsConfig:  awsCfg,
		httpClient: &http.Client{},
		kmsClient:  kms.NewFromConfig(awsCfg),
		sqsClient:  sqs.NewFromConfig(awsCfg),
		ssmClient:  ssm.NewFromConfig(awsCfg),
	}, nil
}

// GetDataset retrieves metadata for a specific dataset.
func (c *Consumer) GetDataset(ctx context.Context, datasetID string) (*types.Dataset, error) {
	path := fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID))

	var dataset types.Dataset
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &dataset); err != nil {
		return nil, err
	}

	return &dataset, nil
}

// GetDownloadURL retrieves a presigned download URL for a dataset.
func (c *Consumer) GetDownloadURL(ctx context.Context, datasetID string) (*DownloadURLInfo, error) {
	path := fmt.Sprintf("/v1/datasets/%s/download", url.PathEscape(datasetID))

	var urlInfo DownloadURLInfo
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &urlInfo); err != nil {
		return nil, err
	}

	return &urlInfo, nil
}

// DownloadDataset downloads and processes a dataset to a local file.
func (c *Consumer) DownloadDataset(ctx context.Context, datasetID, outputPath string) error {
	fmt.Printf("Downloading dataset %s...\n", datasetID)

	// Get download URL.
	urlInfo, err := c.GetDownloadURL(ctx, datasetID)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %w", err)
	}

	// Get dataset metadata to check encryption/compression settings.
	dataset, err := c.GetDataset(ctx, datasetID)
	if err != nil {
		return fmt.Errorf("failed to get dataset metadata: %w", err)
	}

	// Safely extract encryption/compression settings with defaults.
	isEncrypted := false
	isCompressed := false
	if dataset.Metadata != nil {
		if enc, ok := dataset.Metadata["encryption_enabled"].(bool); ok {
			isEncrypted = enc
		}
		if comp, ok := dataset.Metadata["compression_enabled"].(bool); ok {
			isCompressed = comp
		}
	}

	fmt.Printf("   Compressed: %v\n", isCompressed)
	fmt.Printf("   Encrypted: %v\n", isEncrypted)

	// Download file.
	resp, err := http.Get(urlInfo.DownloadURL)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Get content length.
	contentLength := resp.ContentLength

	largeFileThreshold := int64(100 * 1024 * 1024) // 100MB

	// For large files, always stream to temporary file first to avoid memory issues.
	if contentLength > largeFileThreshold {
		// Create temporary file
		tempFile, err := os.CreateTemp("", "helix-dataset-*")
		if err != nil {
			return fmt.Errorf("failed to create temp file: %w", err)
		}

		defer os.Remove(tempFile.Name()) // Clean up temp file.

		defer tempFile.Close()

		// Stream download to temp file.
		sizeGB := float64(contentLength) / (1024 * 1024 * 1024)

		fmt.Printf("Streaming %.2f GB to temporary file...\n", sizeGB)

		written, err := io.Copy(tempFile, resp.Body)
		if err != nil {
			return fmt.Errorf("failed to stream to temp file: %w", err)
		}

		tempFile.Close() // Close before reading.

		fmt.Printf("Downloaded %d bytes to temp file\n", written)

		// For processed files, read from temp file, process, and write to final location.
		fmt.Printf("Processing temp file...\n")

		data, err := os.ReadFile(tempFile.Name())
		if err != nil {
			return fmt.Errorf("failed to read temp file: %w", err)
		}

		// Step 1: Decrypt FIRST (if encrypted).
		if isEncrypted {
			fmt.Printf("Decrypting %d bytes with KMS...\n", len(data))

			data, err = c.decryptData(ctx, data)
			if err != nil {
				return fmt.Errorf("decryption failed: %w", err)
			}

			fmt.Printf("Decrypted to %d bytes\n", len(data))
		}

		// Step 2: Decompress SECOND (if compressed).
		if isCompressed {
			fmt.Printf("Decompressing %d bytes...\n", len(data))

			data, err = c.decompressData(data)
			if err != nil {
				return fmt.Errorf("decompression failed: %w", err)
			}

			decompressedGB := float64(len(data)) / (1024 * 1024 * 1024)
			if decompressedGB > 1 {
				fmt.Printf("Decompressed to %.2f GB\n", decompressedGB)
			} else {
				fmt.Printf("Decompressed to %d bytes\n", len(data))
			}
		}

		// Write processed data to final file.
		if err := os.WriteFile(outputPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}

		fmt.Printf("Saved to %s\n", outputPath)

		return nil
	}

	// For smaller files, process in memory as before.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	fmt.Printf("Downloaded %d bytes\n", len(data))

	// Step 1: Decrypt FIRST (if encrypted).
	if isEncrypted {
		fmt.Printf("Decrypting %d bytes with KMS...\n", len(data))

		data, err = c.decryptData(ctx, data)
		if err != nil {
			return fmt.Errorf("decryption failed: %w", err)
		}

		fmt.Printf("Decrypted to %d bytes\n", len(data))
	}

	// Step 2: Decompress SECOND (if compressed).
	if isCompressed {
		fmt.Printf("Decompressing %d bytes...\n", len(data))

		data, err = c.decompressData(data)
		if err != nil {
			return fmt.Errorf("decompression failed: %w", err)
		}

		decompressedGB := float64(len(data)) / (1024 * 1024 * 1024)

		if decompressedGB > 1 {
			fmt.Printf("Decompressed to %.2f GB\n", decompressedGB)
		} else {
			fmt.Printf("Decompressed to %d bytes\n", len(data))
		}
	}

	// Write to file.
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	fmt.Printf("Saved to %s\n", outputPath)

	return nil
}

// decryptData decrypts data using envelope decryption.
func (c *Consumer) decryptData(ctx context.Context, data []byte) ([]byte, error) {
	buf := bytes.NewReader(data)

	// Read encrypted key length.
	var keyLen uint32
	if err := binary.Read(buf, binary.BigEndian, &keyLen); err != nil {
		return nil, err
	}

	// Read encrypted data key.
	encryptedKey := make([]byte, keyLen)
	if _, err := buf.Read(encryptedKey); err != nil {
		return nil, err
	}

	// Read IV (16 bytes).
	iv := make([]byte, 16)
	if _, err := buf.Read(iv); err != nil {
		return nil, err
	}

	// Read auth tag (16 bytes).
	authTag := make([]byte, 16)
	if _, err := buf.Read(authTag); err != nil {
		return nil, err
	}

	// Remaining bytes are encrypted data.
	encryptedData, err := io.ReadAll(buf)
	if err != nil {
		return nil, err
	}

	// Decrypt data key with KMS.
	decryptOut, err := c.kmsClient.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: encryptedKey,
	})
	if err != nil {
		return nil, fmt.Errorf("KMS decrypt failed: %w", err)
	}

	// Decrypt data with AES-256-GCM.
	block, err := aes.NewCipher(decryptOut.Plaintext)
	if err != nil {
		return nil, err
	}

	// Use 16-byte nonce (Python uses os.urandom(16) for IV).
	aesGCM, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return nil, err
	}

	// Append auth tag to encrypted data for GCM.
	ciphertext := append(encryptedData, authTag...)

	plaintext, err := aesGCM.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM decrypt failed: %w", err)
	}

	return plaintext, nil
}

// decompressData decompresses data using gzip.
func (c *Consumer) decompressData(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	defer gr.Close()

	return io.ReadAll(gr)
}

// ListDatasets lists all available datasets.
func (c *Consumer) ListDatasets(ctx context.Context) ([]Dataset, error) {
	type DatasetsResponse struct {
		Datasets []Dataset `json:"datasets"`
		Count    int       `json:"count"`
	}

	var response DatasetsResponse
	if err := c.makeAPIRequest(ctx, http.MethodGet, "/v1/datasets", nil, &response); err != nil {
		return nil, err
	}

	return response.Datasets, nil
}

// ListSubscriptions lists all active subscriptions for this consumer.
func (c *Consumer) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	type SubscriptionsResponse struct {
		Subscriptions []Subscription `json:"subscriptions"`
		Count         int            `json:"count"`
	}

	var response SubscriptionsResponse
	if err := c.makeAPIRequest(ctx, http.MethodGet, "/v1/subscriptions", nil, &response); err != nil {
		return nil, err
	}

	return response.Subscriptions, nil
}

// CreateSubscriptionRequest creates a subscription request to access a producer's datasets.
// The producer must approve the request before the consumer gains access.
//
// Parameters:
//   - input.ProducerID: Required. The ID of the producer to request access from.
//   - input.DatasetID: Optional. Specific dataset ID (nil for all-datasets access).
//   - input.Tier: Optional. Subscription tier (defaults to "basic").
//   - input.Message: Optional. Message to the producer explaining the request.
//
// Returns the created subscription request with status "pending".
func (c *Consumer) CreateSubscriptionRequest(ctx context.Context, input types.CreateSubscriptionRequestInput) (*types.SubscriptionRequest, error) {
	// Build request payload
	payload := types.CreateSubscriptionRequestPayload{
		ProducerID: input.ProducerID,
		DatasetID:  input.DatasetID,
		Tier:       input.Tier,
		Message:    input.Message,
	}

	// Default tier to "basic" if not specified
	if payload.Tier == "" {
		payload.Tier = "basic"
	}

	var result types.SubscriptionRequest
	if err := c.makeAPIRequest(ctx, http.MethodPost, "/v1/subscription-requests", payload, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// makeAPIRequest makes an authenticated API request.
func (c *Consumer) makeAPIRequest(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	reqURL := c.APIEndpoint + path

	var (
		reqBody  io.Reader
		jsonData []byte
	)

	if body != nil {
		var err error
		jsonData, err = json.Marshal(body)
		if err != nil {
			return err
		}

		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	// Sign request with AWS SigV4
	creds, err := c.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return err
	}

	// Calculate payload hash for SigV4
	payloadHash := emptyPayloadHash
	if body != nil {
		// Hash the actual JSON body
		h := sha256.New()
		h.Write(jsonData)
		payloadHash = fmt.Sprintf("%x", h.Sum(nil))
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "execute-api", c.Region, time.Now()); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("API request failed: %d - %s", resp.StatusCode, string(bodyBytes))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return err
		}
	}

	return nil
}

// PollNotifications polls the per-consumer SQS queue for dataset upload notifications.
//
// IMPORTANT: This uses a DEDICATED queue for this consumer. SNS filter policies
// ensure only relevant notifications reach this queue. You can optionally filter
// by subscription IDs for advanced use cases.
//
// Messages are automatically acknowledged (deleted) by default after retrieval.
// This prevents duplicate processing and simplifies the developer experience.
// Set opts.AutoAcknowledge to false if you need manual control over message deletion.
func (c *Consumer) PollNotifications(ctx context.Context, opts PollNotificationsOptions) ([]Notification, error) {
	// Apply defaults
	if opts.MaxMessages == 0 {
		opts.MaxMessages = 10
	}

	if opts.MaxMessages > 10 {
		opts.MaxMessages = 10 // AWS limit.
	}

	if opts.WaitTimeSeconds == 0 {
		opts.WaitTimeSeconds = 20
	}

	if opts.WaitTimeSeconds > 20 {
		opts.WaitTimeSeconds = 20 // AWS limit.
	}

	// Default AutoAcknowledge to true.
	autoAcknowledge := true

	if opts.AutoAcknowledge != nil {
		autoAcknowledge = *opts.AutoAcknowledge
	}

	// Get per-consumer queue URL from first active subscription.
	if c.queueURL == nil {
		subscriptions, err := c.ListSubscriptions(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get subscriptions: %w", err)
		}

		if len(subscriptions) == 0 {
			return nil, fmt.Errorf("no active subscriptions found. Create a subscription first using CreateSubscriptionRequest()")
		}

		// Get queue URL from first subscription (all subscriptions for same consumer share same queue).
		var queueURL *string

		for _, sub := range subscriptions {
			if sub.SQSQueueURL != nil {
				queueURL = sub.SQSQueueURL

				break
			}
		}

		if queueURL == nil {
			return nil, fmt.Errorf("per-consumer queue not provisioned. This may be a legacy subscription. " +
				"Please contact support or create a new subscription to get a dedicated queue.")
		}

		c.queueURL = queueURL
	}

	queueURL := aws.ToString(c.queueURL)

	// Poll SQS for messages.
	receiveOutput, err := c.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		MaxNumberOfMessages:   opts.MaxMessages,
		MessageAttributeNames: []string{"All"},
		QueueUrl:              aws.String(queueURL),
		VisibilityTimeout:     300,
		WaitTimeSeconds:       opts.WaitTimeSeconds,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to poll SQS queue: %w", err)
	}

	var notifications []Notification

	for _, message := range receiveOutput.Messages {
		// Parse message body - handle both SNS-wrapped and raw message formats.
		// SNS-wrapped messages have a "Message" field containing the stringified notification.
		// Raw messages contain the notification fields directly (event_type, producer_id, etc.).

		messageBody := aws.ToString(message.Body)

		// First, try to parse as a generic JSON to determine format.
		var parsedBody map[string]any
		if err := json.Unmarshal([]byte(messageBody), &parsedBody); err != nil {
			fmt.Printf("Warning: Failed to parse message body: %v\n", err)
			continue
		}

		// Notification data structure.
		var notificationData struct {
			DatasetID      string `json:"dataset_id"`
			DatasetName    string `json:"dataset_name"`
			EventType      string `json:"event_type"`
			ProducerID     string `json:"producer_id"`
			S3Bucket       string `json:"s3_bucket"`
			S3Key          string `json:"s3_key"`
			SizeBytes      int64  `json:"size_bytes"`
			SubscriberID   string `json:"subscriber_id"`
			SubscriptionID string `json:"subscription_id"`
			Timestamp      string `json:"timestamp"`
		}

		// Determine message format and parse notification data.
		if snsMessage, hasSNSWrapper := parsedBody["Message"].(string); hasSNSWrapper {
			// SNS-wrapped format: { "Type": "Notification", "Message": "{...}", ... }
			if err := json.Unmarshal([]byte(snsMessage), &notificationData); err != nil {
				fmt.Printf("Warning: Failed to parse notification payload from SNS wrapper: %v\n", err)
				continue
			}
		} else if _, hasEventType := parsedBody["event_type"]; hasEventType {
			// Raw notification payload format (raw_message_delivery = true or direct SQS).
			if err := json.Unmarshal([]byte(messageBody), &notificationData); err != nil {
				fmt.Printf("Warning: Failed to parse raw notification payload: %v\n", err)
				continue
			}
		} else {
			fmt.Printf("Warning: Unknown message format for %s, skipping\n", aws.ToString(message.MessageId))
			continue
		}

		// SNS filter policy ensures only messages for this consumer reach this queue
		// No need for subscriber_id filtering - it's already guaranteed by SNS.

		// Optional filter by subscription IDs if provided (advanced use case).
		if len(opts.SubscriptionIDs) > 0 {
			found := false

			for _, subID := range opts.SubscriptionIDs {
				if subID == notificationData.SubscriptionID {
					found = true

					break
				}
			}
			if !found {
				continue // Skip this notification - doesn't match our subscriptions.
			}
		}

		notification := Notification{
			DatasetID:      notificationData.DatasetID,
			DatasetName:    notificationData.DatasetName,
			EventType:      notificationData.EventType,
			MessageID:      aws.ToString(message.MessageId),
			ProducerID:     notificationData.ProducerID,
			RawMessage:     aws.ToString(message.Body),
			ReceiptHandle:  aws.ToString(message.ReceiptHandle),
			S3Bucket:       notificationData.S3Bucket,
			S3Key:          notificationData.S3Key,
			SizeBytes:      notificationData.SizeBytes,
			SubscriberID:   notificationData.SubscriberID,
			SubscriptionID: notificationData.SubscriptionID,
			Timestamp:      notificationData.Timestamp,
		}

		notifications = append(notifications, notification)

		// Auto-acknowledge (delete) message by default.
		if autoAcknowledge {
			if err := c.DeleteNotification(ctx, notification.ReceiptHandle); err != nil {
				fmt.Printf("Warning: Failed to auto-acknowledge notification %s: %v\n", notification.MessageID, err)
			}
		}
	}

	return notifications, nil
}

// DeleteNotification deletes a notification message from the SQS queue after processing.
func (c *Consumer) DeleteNotification(ctx context.Context, receiptHandle string) error {
	if c.queueURL == nil {
		return fmt.Errorf("queue URL not available. Call PollNotifications() first to initialize the queue URL")
	}

	queueURL := aws.ToString(c.queueURL)

	// Delete message.
	if _, err := c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	}); err != nil {
		return fmt.Errorf("failed to delete notification: %w", err)
	}

	return nil
}

// ListMySubscriptionRequests lists the consumer's own subscription requests.
// Allows consumers to track the status of their pending, approved, or rejected requests.
//
// Parameters:
//   - status: Filter by request status. Valid values: "pending", "approved", "rejected", "cancelled".
//     If empty, returns all requests regardless of status.
//
// Returns a slice of subscription requests matching the filter.
func (c *Consumer) ListMySubscriptionRequests(ctx context.Context, status string) ([]types.SubscriptionRequest, error) {
	path := "/v1/subscription-requests"
	if status != "" {
		path = fmt.Sprintf("/v1/subscription-requests?status=%s", url.QueryEscape(status))
	}

	var response types.SubscriptionRequestsResponse
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}

	return response.Requests, nil
}

// GetSubscriptionRequest retrieves details of a specific subscription request by ID.
//
// Parameters:
//   - requestID: The subscription request ID (e.g., "req-abc123-def456").
//
// Returns the subscription request with all details.
func (c *Consumer) GetSubscriptionRequest(ctx context.Context, requestID string) (*types.SubscriptionRequest, error) {
	path := fmt.Sprintf("/v1/subscription-requests/%s", url.PathEscape(requestID))

	var result types.SubscriptionRequest
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ClearQueue clears all messages from the consumer's notification queue.
//
// This permanently deletes all messages in the queue. Use with caution.
//
// IMPORTANT: AWS limits PurgeQueue to once every 60 seconds per queue.
// Calling this method more frequently will result in an error.
func (c *Consumer) ClearQueue(ctx context.Context) error {
	// Initialize queue URL if not already set.
	if c.queueURL == nil {
		subscriptions, err := c.ListSubscriptions(ctx)
		if err != nil {
			return fmt.Errorf("failed to get subscriptions: %w", err)
		}

		if len(subscriptions) == 0 {
			return fmt.Errorf("no active subscriptions found. Cannot determine queue URL")
		}

		// Get queue URL from first subscription with a queue.
		var queueURL *string

		for _, sub := range subscriptions {
			if sub.SQSQueueURL != nil {
				queueURL = sub.SQSQueueURL

				break
			}
		}

		if queueURL == nil {
			return fmt.Errorf("per-consumer queue not provisioned. This may be a legacy subscription. " +
				"Please contact support or create a new subscription to get a dedicated queue.")
		}

		c.queueURL = queueURL
	}

	queueURL := aws.ToString(c.queueURL)

	// Purge queue.
	if _, err := c.sqsClient.PurgeQueue(ctx, &sqs.PurgeQueueInput{
		QueueUrl: aws.String(queueURL),
	}); err != nil {
		// Check for PurgeQueueInProgress error.
		if strings.Contains(err.Error(), "PurgeQueueInProgress") {
			return fmt.Errorf("queue purge already in progress. AWS limits PurgeQueue to once every 60 seconds per queue")
		}

		return fmt.Errorf("failed to clear queue: %w", err)
	}

	return nil
}
