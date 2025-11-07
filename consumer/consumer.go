package consumer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type Consumer struct {
	CustomerID        string
	APIEndpoint       string
	Region            string
	awsConfig         aws.Config
	kmsClient         *kms.Client
	sqsClient         *sqs.Client
	ssmClient         *ssm.Client
	httpClient        *http.Client
	queueURL          *string // Cache for per-consumer queue URL
}

type Config struct {
	CustomerID          string
	AWSAccessKeyID      string
	AWSSecretAccessKey  string
	APIEndpoint         string
	Region              string
}

type DownloadURLInfo struct {
	DownloadURL string `json:"download_url"`
	ExpiresAt   string `json:"expires_at"`
	Dataset     struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"dataset"`
}

type Dataset struct {
	ID       string `json:"_id"`
	Name     string `json:"name"`
	Metadata struct {
		CompressionEnabled bool `json:"compression_enabled"`
		EncryptionEnabled  bool `json:"encryption_enabled"`
	} `json:"metadata"`
}

type Notification struct {
	MessageID      string `json:"message_id"`
	ReceiptHandle  string `json:"receipt_handle"`
	EventType      string `json:"event_type"`
	ProducerID     string `json:"producer_id"`
	DatasetID      string `json:"dataset_id"`
	DatasetName    string `json:"dataset_name,omitempty"`
	S3Bucket       string `json:"s3_bucket"`
	S3Key          string `json:"s3_key"`
	SizeBytes      int64  `json:"size_bytes"`
	Timestamp      string `json:"timestamp"`
	SubscriberID   string `json:"subscriber_id"`
	SubscriptionID string `json:"subscription_id"`
	RawMessage     string `json:"raw_message"`
}

type Subscription struct {
	ID           string  `json:"_id"`
	ProducerID   string  `json:"producer_id"`
	ConsumerID   string  `json:"consumer_id"`
	Status       string  `json:"status"`
	SQSQueueURL  *string `json:"sqs_queue_url,omitempty"`
	SQSQueueARN  *string `json:"sqs_queue_arn,omitempty"`
	SNSSubARN    *string `json:"sns_subscription_arn,omitempty"`
}

// PollNotificationsOptions contains options for polling notifications from SQS.
type PollNotificationsOptions struct {
	MaxMessages      int32    // Maximum number of messages to retrieve (1-10, default: 10)
	WaitTimeSeconds  int32    // Long polling wait time (0-20 seconds, default: 20)
	AutoAcknowledge  *bool    // Automatically acknowledge (delete) messages after receiving (default: true)
	SubscriptionIDs  []string // Optional list of subscription IDs to filter notifications
}

func NewConsumer(cfg Config) (*Consumer, error) {
	if cfg.APIEndpoint == "" {
		cfg.APIEndpoint = "https://api.helix.tools"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	
	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(cfg.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AWSAccessKeyID,
			cfg.AWSSecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	
	// Validate credentials
	stsClient := sts.NewFromConfig(awsCfg)
	_, err = stsClient.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("invalid AWS credentials: %w", err)
	}
	
	return &Consumer{
		CustomerID:  cfg.CustomerID,
		APIEndpoint: cfg.APIEndpoint,
		Region:      cfg.Region,
		awsConfig:   awsCfg,
		kmsClient:   kms.NewFromConfig(awsCfg),
		sqsClient:   sqs.NewFromConfig(awsCfg),
		ssmClient:   ssm.NewFromConfig(awsCfg),
		httpClient:  &http.Client{},
	}, nil
}

func (c *Consumer) GetDataset(ctx context.Context, datasetID string) (*Dataset, error) {
	path := fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID))
	
	var dataset Dataset
	if err := c.makeAPIRequest(ctx, "GET", path, nil, &dataset); err != nil {
		return nil, err
	}
	
	return &dataset, nil
}

func (c *Consumer) GetDownloadURL(ctx context.Context, datasetID string) (*DownloadURLInfo, error) {
	path := fmt.Sprintf("/v1/datasets/%s/download", url.PathEscape(datasetID))
	
	var urlInfo DownloadURLInfo
	if err := c.makeAPIRequest(ctx, "GET", path, nil, &urlInfo); err != nil {
		return nil, err
	}
	
	return &urlInfo, nil
}

func (c *Consumer) DownloadDataset(ctx context.Context, datasetID, outputPath string) error {
	fmt.Printf("Downloading dataset %s...\n", datasetID)
	
	// Get dataset metadata
	dataset, err := c.GetDataset(ctx, datasetID)
	if err != nil {
		return fmt.Errorf("failed to get dataset: %w", err)
	}
	
	isCompressed := dataset.Metadata.CompressionEnabled
	isEncrypted := dataset.Metadata.EncryptionEnabled
	
	fmt.Printf("   Compressed: %v\n", isCompressed)
	fmt.Printf("   Encrypted: %v\n", isEncrypted)
	
	// Get download URL
	urlInfo, err := c.GetDownloadURL(ctx, datasetID)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %w", err)
	}
	
	// Download file
	resp, err := http.Get(urlInfo.DownloadURL)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	
	fmt.Printf("Downloaded %d bytes\n", len(data))
	
	// Step 1: Decrypt (if encrypted)
	if isEncrypted {
		fmt.Printf("Decrypting %d bytes with KMS...\n", len(data))
		data, err = c.decryptData(ctx, data)
		if err != nil {
			return fmt.Errorf("decryption failed: %w", err)
		}
		fmt.Printf("Decrypted to %d bytes\n", len(data))
	}
	
	// Step 2: Decompress (if compressed)
	if isCompressed {
		fmt.Printf("Decompressing %d bytes...\n", len(data))
		data, err = c.decompressData(data)
		if err != nil {
			return fmt.Errorf("decompression failed: %w", err)
		}
		fmt.Printf("Decompressed to %d bytes\n", len(data))
	}
	
	// Write to file
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	
	fmt.Printf("Saved to %s\n", outputPath)
	return nil
}

func (c *Consumer) decryptData(ctx context.Context, data []byte) ([]byte, error) {
	buf := bytes.NewReader(data)
	
	// Read encrypted key length
	var keyLen uint32
	if err := binary.Read(buf, binary.BigEndian, &keyLen); err != nil {
		return nil, err
	}
	
	// Read encrypted data key
	encryptedKey := make([]byte, keyLen)
	if _, err := buf.Read(encryptedKey); err != nil {
		return nil, err
	}
	
	// Read IV (16 bytes)
	iv := make([]byte, 16)
	if _, err := buf.Read(iv); err != nil {
		return nil, err
	}
	
	// Read auth tag (16 bytes)
	authTag := make([]byte, 16)
	if _, err := buf.Read(authTag); err != nil {
		return nil, err
	}
	
	// Remaining bytes are encrypted data
	encryptedData, err := io.ReadAll(buf)
	if err != nil {
		return nil, err
	}
	
	// Decrypt data key with KMS
	decryptOut, err := c.kmsClient.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: encryptedKey,
	})
	if err != nil {
		return nil, fmt.Errorf("KMS decrypt failed: %w", err)
	}
	
	// Decrypt data with AES-256-GCM
	block, err := aes.NewCipher(decryptOut.Plaintext)
	if err != nil {
		return nil, err
	}

	// Use 16-byte nonce (Python uses os.urandom(16) for IV)
	aesGCM, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return nil, err
	}

	// Append auth tag to encrypted data for GCM
	ciphertext := append(encryptedData, authTag...)

	plaintext, err := aesGCM.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("AES-GCM decrypt failed: %w", err)
	}

	return plaintext, nil
}

func (c *Consumer) decompressData(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	
	return io.ReadAll(gr)
}

func (c *Consumer) ListDatasets(ctx context.Context) ([]Dataset, error) {
	type DatasetsResponse struct {
		Datasets []Dataset `json:"datasets"`
		Count    int       `json:"count"`
	}

	var response DatasetsResponse
	if err := c.makeAPIRequest(ctx, "GET", "/v1/datasets", nil, &response); err != nil {
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
	if err := c.makeAPIRequest(ctx, "GET", "/v1/subscriptions", nil, &response); err != nil {
		return nil, err
	}

	return response.Subscriptions, nil
}

func (c *Consumer) makeAPIRequest(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	reqURL := c.APIEndpoint + path

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
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

	signer := v4.NewSigner()
	payloadHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // empty body hash
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
//
// Example (simple usage - messages auto-deleted after poll):
//
//	opts := consumer.PollNotificationsOptions{
//		MaxMessages:     10,
//		WaitTimeSeconds: 20,
//		SubscriptionIDs: []string{"sub-123"},
//	}
//	notifications, err := consumer.PollNotifications(ctx, opts)
//	for _, notif := range notifications {
//		fmt.Printf("New dataset: %s\n", notif.DatasetName)
//		// Download dataset...
//		// No need to delete - already handled automatically!
//	}
//
// Example (manual acknowledgment for custom retry logic):
//
//	autoAck := false
//	opts := consumer.PollNotificationsOptions{
//		MaxMessages:     10,
//		WaitTimeSeconds: 20,
//		AutoAcknowledge: &autoAck,
//	}
//	notifications, err := consumer.PollNotifications(ctx, opts)
//	for _, notif := range notifications {
//		if err := processDataset(notif); err != nil {
//			fmt.Printf("Processing failed, message will retry: %v\n", err)
//			// Message not deleted, will become visible again after visibility timeout
//			continue
//		}
//		// Manually acknowledge only after successful processing
//		consumer.DeleteNotification(ctx, notif.ReceiptHandle)
//	}
func (c *Consumer) PollNotifications(ctx context.Context, opts PollNotificationsOptions) ([]Notification, error) {
	// Apply defaults
	if opts.MaxMessages == 0 {
		opts.MaxMessages = 10
	}
	if opts.MaxMessages > 10 {
		opts.MaxMessages = 10 // AWS limit
	}
	if opts.WaitTimeSeconds == 0 {
		opts.WaitTimeSeconds = 20
	}
	if opts.WaitTimeSeconds > 20 {
		opts.WaitTimeSeconds = 20 // AWS limit
	}
	// Default AutoAcknowledge to true
	autoAcknowledge := true
	if opts.AutoAcknowledge != nil {
		autoAcknowledge = *opts.AutoAcknowledge
	}

	// Get per-consumer queue URL from first active subscription
	if c.queueURL == nil {
		subscriptions, err := c.ListSubscriptions(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get subscriptions: %w", err)
		}

		if len(subscriptions) == 0 {
			return nil, fmt.Errorf("no active subscriptions found. Create a subscription first using CreateSubscriptionRequest()")
		}

		// Get queue URL from first subscription (all subscriptions for same consumer share same queue)
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

	// Poll SQS for messages
	receiveOutput, err := c.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: opts.MaxMessages,
		WaitTimeSeconds:     opts.WaitTimeSeconds,
		VisibilityTimeout:   300,
		MessageAttributeNames: []string{"All"},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to poll SQS queue: %w", err)
	}

	var notifications []Notification

	for _, message := range receiveOutput.Messages {
		// Parse SNS message
		var snsMessage struct {
			Message string `json:"Message"`
		}
		if err := json.Unmarshal([]byte(aws.ToString(message.Body)), &snsMessage); err != nil {
			fmt.Printf("Warning: Failed to parse SNS message: %v\n", err)
			continue
		}

		// Parse custom notification payload
		var notificationData struct {
			EventType      string `json:"event_type"`
			ProducerID     string `json:"producer_id"`
			DatasetID      string `json:"dataset_id"`
			DatasetName    string `json:"dataset_name"`
			S3Bucket       string `json:"s3_bucket"`
			S3Key          string `json:"s3_key"`
			SizeBytes      int64  `json:"size_bytes"`
			Timestamp      string `json:"timestamp"`
			SubscriberID   string `json:"subscriber_id"`
			SubscriptionID string `json:"subscription_id"`
		}
		if err := json.Unmarshal([]byte(snsMessage.Message), &notificationData); err != nil {
			fmt.Printf("Warning: Failed to parse notification payload: %v\n", err)
			continue
		}

		// SNS filter policy ensures only messages for this consumer reach this queue
		// No need for subscriber_id filtering - it's already guaranteed by SNS

		// Optional filter by subscription IDs if provided (advanced use case)
		if len(opts.SubscriptionIDs) > 0 {
			found := false
			for _, subID := range opts.SubscriptionIDs {
				if subID == notificationData.SubscriptionID {
					found = true
					break
				}
			}
			if !found {
				continue // Skip this notification - doesn't match our subscriptions
			}
		}

		notification := Notification{
			MessageID:      aws.ToString(message.MessageId),
			ReceiptHandle:  aws.ToString(message.ReceiptHandle),
			EventType:      notificationData.EventType,
			ProducerID:     notificationData.ProducerID,
			DatasetID:      notificationData.DatasetID,
			DatasetName:    notificationData.DatasetName,
			S3Bucket:       notificationData.S3Bucket,
			S3Key:          notificationData.S3Key,
			SizeBytes:      notificationData.SizeBytes,
			Timestamp:      notificationData.Timestamp,
			SubscriberID:   notificationData.SubscriberID,
			SubscriptionID: notificationData.SubscriptionID,
			RawMessage:     aws.ToString(message.Body),
		}
		notifications = append(notifications, notification)

		// Auto-acknowledge (delete) message by default
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

	// Delete message
	_, err := c.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(queueURL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return fmt.Errorf("failed to delete notification: %w", err)
	}

	return nil
}

// extractProducerID extracts producer ID from S3 key path.
// S3 keys follow the pattern: datasets/{dataset_name}/{date}/{file}
func extractProducerID(s3Key string) string {
	// Example key: datasets/company-123-producer-Dataset Name/2025-11-01/file.json.gz
	parts := strings.Split(s3Key, "/")
	if len(parts) >= 2 {
		datasetName := parts[1]
		// Extract company ID from dataset name
		if strings.Contains(datasetName, "company-") {
			nameParts := strings.Split(datasetName, "-")
			if len(nameParts) >= 2 {
				return "company-" + nameParts[1]
			}
		}
	}
	return "unknown"
}
