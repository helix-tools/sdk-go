// Package producer provides functionality for uploading datasets to the Helix Connect Platform.
//
// It handles the entire lifecycle of dataset production, including authentication,
// encrypting, compressing, uploading datasets, and notifying subscribers via SNS.
//
// TODO: Use thalesfsp/sypl logger, and set log levels to `debug`.
package producer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Producer handles uploading and managing datasets on Helix Connect platform.
type Producer struct {
	APIEndpoint string
	BucketName  string
	CustomerID  string
	KMSKeyID    string
	Region      string

	awsConfig  aws.Config
	httpClient *http.Client
	kmsClient  *kms.Client
	s3Client   *s3.Client
}

// APIError represents an error returned by the Helix API with status code.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Body)
}

// IsConflict returns true if the error is a 409 Conflict (duplicate resource).
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}

// UploadOptions contains options for uploading datasets.
//
// NOTE: Use NewUploadOptions() to get sane defaults.
// NOTE: Encryption and compression are required.
type UploadOptions struct {
	Category         string
	Compress         bool
	CompressionLevel int // Default: 6 (gzip compression level 1-9)
	DataFreshness    types.DataFreshness
	DatasetName      string
	Description      string
	Encrypt          bool
	Metadata         map[string]any
	DatasetOverrides map[string]any
}

// NewUploadOptions creates UploadOptions with sane defaults.
//
// NOTE: This is the recommended way to create upload options.
func NewUploadOptions(datasetName string) UploadOptions {
	return UploadOptions{
		DatasetName:      datasetName,
		Category:         "general",
		DataFreshness:    types.DataFreshnessDaily,
		Encrypt:          true,
		Compress:         true,
		CompressionLevel: 6,
	}
}

// NewProducer creates a new Producer instance.
//
// TODO: Allow to pass context for better control.
func NewProducer(cfg types.Config) (*Producer, error) {
	// Basic validation.
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

	// Load AWS config.
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

	// Validate credentials.
	stsClient := sts.NewFromConfig(awsCfg)
	if _, err = stsClient.GetCallerIdentity(
		context.Background(),
		&sts.GetCallerIdentityInput{},
	); err != nil {
		return nil, fmt.Errorf("invalid AWS credentials: %w", err)
	}

	// Get producer-specific resources from SSM.
	ssmClient := ssm.NewFromConfig(awsCfg)

	// Get S3 bucket name.
	bucketParamCandidates := ssmParamCandidates(cfg.CustomerID, "s3_bucket")
	bucketValue, err := getSSMParameterValue(context.Background(), ssmClient, bucketParamCandidates)
	if err != nil {
		return nil, fmt.Errorf("S3 bucket not found for producer %s: %w", cfg.CustomerID, err)
	}

	// Get KMS key ID.
	kmsKeyID := ""
	kmsParamCandidates := ssmParamCandidates(cfg.CustomerID, "kms_key_id")
	kmsValue, err := getSSMParameterValue(context.Background(), ssmClient, kmsParamCandidates)
	if err != nil {
		fmt.Printf("Warning: KMS key not found, encryption will be disabled: %v\n", err)
	} else {
		kmsKeyID = kmsValue
	}

	return &Producer{
		APIEndpoint: cfg.APIEndpoint,
		BucketName:  bucketValue,
		CustomerID:  cfg.CustomerID,
		KMSKeyID:    kmsKeyID,
		Region:      cfg.Region,

		awsConfig:  awsCfg,
		httpClient: &http.Client{},
		kmsClient:  kms.NewFromConfig(awsCfg),
		s3Client:   s3.NewFromConfig(awsCfg),
	}, nil
}

func ssmParamCandidates(customerID, paramName string) []string {
	if customerID == "" || paramName == "" {
		return nil
	}

	env := os.Getenv("HELIX_ENVIRONMENT")
	if env == "" {
		env = os.Getenv("ENVIRONMENT")
	}
	if env == "" {
		env = "production"
	}

	prefixes := []string{}
	if prefix := strings.TrimRight(os.Getenv("HELIX_SSM_CUSTOMER_PREFIX"), "/"); prefix != "" {
		prefixes = append(prefixes, prefix)
	}
	prefixes = append(prefixes,
		fmt.Sprintf("/helix-tools/%s/customers", env),
		fmt.Sprintf("/helix/%s/customers", env),
		"/helix/customers",
	)

	seen := map[string]struct{}{}
	candidates := []string{}
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		candidates = append(candidates, fmt.Sprintf("%s/%s/%s", prefix, customerID, paramName))
	}
	return candidates
}

func getSSMParameterValue(ctx context.Context, client *ssm.Client, names []string) (string, error) {
	var lastErr error
	for _, name := range names {
		resp, err := client.GetParameter(ctx, &ssm.GetParameterInput{
			Name:           aws.String(name),
			WithDecryption: aws.Bool(true),
		})
		if err == nil && resp.Parameter != nil && resp.Parameter.Value != nil {
			return aws.ToString(resp.Parameter.Value), nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("empty SSM parameter value for %s", name)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("SSM parameter not found")
	}
	return "", lastErr
}

// compressData compresses data using gzip.
func (p *Producer) compressData(data []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	gzWriter, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip writer: %w", err)
	}

	if _, err := gzWriter.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write to gzip: %w", err)
	}

	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return buf.Bytes(), nil
}

// encryptData encrypts data using envelope encryption
// Process:
// 1. Generate random data key (32 bytes for AES-256)
// 2. Encrypt data with the data key
// 3. Encrypt the data key with KMS
// 4. Return: [key_length][encrypted_key][iv][tag][encrypted_data]
func (p *Producer) encryptData(ctx context.Context, data []byte) ([]byte, error) {
	if p.KMSKeyID == "" {
		return nil, fmt.Errorf("KMS key not configured, cannot encrypt data")
	}

	// Generate random data key and IV.
	dataKey := make([]byte, 32) // 256-bit key for AES-256.
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("failed to generate data key: %w", err)
	}

	iv := make([]byte, 16) // 128-bit IV for GCM mode (16 bytes, matches Python SDK).
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("failed to generate IV: %w", err)
	}

	// Encrypt data with data key using AES-256-GCM.
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Use 16-byte nonce to match Python's os.urandom(16).
	aesGCM, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Encrypt the data (auth tag is automatically appended).
	encryptedData := aesGCM.Seal(nil, iv, data, nil)

	// Split encrypted data and auth tag
	authTagSize := aesGCM.Overhead()
	actualEncryptedData := encryptedData[:len(encryptedData)-authTagSize]
	authTag := encryptedData[len(encryptedData)-authTagSize:]

	// Encrypt the data key with KMS.
	encryptOutput, err := p.kmsClient.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(p.KMSKeyID),
		Plaintext: dataKey,
	})
	if err != nil {
		return nil, fmt.Errorf("KMS encryption failed: %w", err)
	}

	// Package: [4 bytes: key length][encrypted key][16 bytes: IV][16 bytes: tag][encrypted data].
	var result bytes.Buffer

	// Write encrypted key length (4 bytes, big-endian).
	if err := binary.Write(&result, binary.BigEndian, uint32(len(encryptOutput.CiphertextBlob))); err != nil {
		return nil, fmt.Errorf("failed to write key length: %w", err)
	}

	// Write encrypted key.
	result.Write(encryptOutput.CiphertextBlob)

	// Write IV (16 bytes).
	result.Write(iv)

	// Write auth tag (16 bytes).
	result.Write(authTag)

	// Write encrypted data.
	result.Write(actualEncryptedData)

	return result.Bytes(), nil
}

// CreateDatasetResponse represents the API response when creating a dataset record.
type CreateDatasetResponse struct {
	ID        string `json:"id"`
	UploadURL string `json:"upload_url"`
	S3Key     string `json:"s3_key"`
}

// ProcessedFileData contains the processed (encrypted/compressed) file data and metadata.
type ProcessedFileData struct {
	Data         []byte
	OriginalSize int64
	Sizes        map[string]any
	Analysis     *AnalysisResult
}

// createDatasetRecord creates a dataset record in the catalog and retrieves presigned URL.
// This is step 1 of the new POST-first upload flow.
func (p *Producer) createDatasetRecord(ctx context.Context, filePath string, opts UploadOptions) (*CreateDatasetResponse, error) {
	// Analyze data before compression/encryption (memory-efficient streaming).
	var analysis *AnalysisResult
	analysisResult, err := p.analyzeData(filePath, DefaultAnalysisOptions())
	if err != nil {
		fmt.Printf("⚠️  Warning: Data analysis failed, continuing without analysis: %v\n", err)
	} else {
		analysis = analysisResult
	}

	// Build initial metadata (sizes will be updated after processing)
	metadata := make(map[string]any)
	maps.Copy(metadata, opts.Metadata)
	
	// Add analysis results to metadata if available
	if analysis != nil {
		metadata["schema"] = analysis.Schema
		metadata["field_emptiness"] = analysis.FieldEmptiness
		metadata["record_count"] = analysis.RecordCount
		if analysis.AnalysisErrors > 0 {
			metadata["analysis_errors"] = analysis.AnalysisErrors
		}
	}

	// Build dataset payload (without S3 key and size, which will be set after upload)
	payload := map[string]any{
		"name":           opts.DatasetName,
		"description":    opts.Description,
		"category":       opts.Category,
		"data_freshness": string(opts.DataFreshness),
		"producer_id":    p.CustomerID,
		"metadata":       metadata,
	}

	// Merge dataset overrides
	if opts.DatasetOverrides != nil {
		maps.Copy(payload, opts.DatasetOverrides)
	}

	// POST to /v1/datasets to create record and get presigned URL
	var response CreateDatasetResponse
	err = p.makeAPIRequest(ctx, "POST", "/v1/datasets", payload, &response)
	if err != nil {
		return nil, fmt.Errorf("failed to create dataset record: %w", err)
	}

	return &response, nil
}

// processFile reads, compresses, and encrypts the file data.
// This is step 2 of the new POST-first upload flow.
func (p *Producer) processFile(ctx context.Context, filePath string, opts UploadOptions) (*ProcessedFileData, error) {
	// Validate encryption/compression requirements
	if !opts.Encrypt {
		return nil, fmt.Errorf("encryption is required for dataset uploads")
	}

	if !opts.Compress {
		return nil, fmt.Errorf("compression is required for dataset uploads")
	}

	if opts.Encrypt && p.KMSKeyID == "" {
		return nil, fmt.Errorf("encryption requested but KMS key not found")
	}

	// Read original file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	originalSize := int64(len(data))

	// Validate file is not empty
	if originalSize == 0 {
		return nil, fmt.Errorf("file is empty: %s (no data to upload)", filePath)
	}

	// Track sizes for metadata
	sizes := map[string]any{
		"original_size_bytes":   originalSize,
		"compressed_size_bytes": originalSize,
		"encrypted_size_bytes":  originalSize,
		"encryption_enabled":    opts.Encrypt,
		"compression_enabled":   opts.Compress,
	}

	// Step 1: Compress FIRST
	if opts.Compress {
		fmt.Printf("📦 Compressing %d bytes with gzip (level %d)...\n", len(data), opts.CompressionLevel)

		compressed, err := p.compressData(data, opts.CompressionLevel)
		if err != nil {
			return nil, fmt.Errorf("compression failed: %w", err)
		}

		data = compressed
		sizes["compressed_size_bytes"] = int64(len(data))

		compressionRatio := (1 - float64(len(data))/float64(originalSize)) * 100
		fmt.Printf("Compressed: %d bytes (%.1f%% reduction)\n", len(data), compressionRatio)
	}

	// Step 2: Encrypt SECOND
	if opts.Encrypt {
		fmt.Printf("🔒 Encrypting %d bytes with KMS key...\n", len(data))

		encrypted, err := p.encryptData(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("encryption failed: %w", err)
		}

		data = encrypted
		sizes["encrypted_size_bytes"] = int64(len(data))

		fmt.Printf("Encrypted: %d bytes\n", len(data))
	}

	return &ProcessedFileData{
		Data:         data,
		OriginalSize: originalSize,
		Sizes:        sizes,
	}, nil
}

// uploadToPresignedURL uploads the processed data to the presigned URL.
// This is step 3 of the new POST-first upload flow.
func (p *Producer) uploadToPresignedURL(ctx context.Context, uploadURL string, data []byte) error {
	fmt.Printf("📤 Uploading %d bytes to presigned URL...\n", len(data))

	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create upload request: %w", err)
	}

	// Set content type for binary data
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload to presigned URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	fmt.Printf("✅ Upload successful\n")
	return nil
}

// UploadDataset uploads a dataset with optional encryption and compression.
// NEW FLOW (POST-first to prevent race conditions):
// 1. POST to /v1/datasets to create record and get presigned URL
// 2. Process file (compress + encrypt)
// 3. PUT to presigned URL
// 4. Return dataset
//
// NOTE: Use NewUploadOptions() to get sane defaults.
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*types.Dataset, error) {
	// Set defaults for fields not specified
	if opts.Category == "" {
		opts.Category = "general"
	}

	if opts.DataFreshness == "" {
		opts.DataFreshness = types.DataFreshnessDaily
	}

	if opts.CompressionLevel == 0 {
		opts.CompressionLevel = 6
	}

	// Validate encryption capability
	if !opts.Encrypt {
		return nil, fmt.Errorf("encryption is required for dataset uploads")
	}

	if !opts.Compress {
		return nil, fmt.Errorf("compression is required for dataset uploads")
	}

	if opts.Encrypt && p.KMSKeyID == "" {
		return nil, fmt.Errorf("encryption requested but KMS key not found")
	}

	// Step 1: Create dataset record and get presigned URL
	createResp, err := p.createDatasetRecord(ctx, filePath, opts)
	if err != nil {
		return nil, err
	}

	fmt.Printf("✅ Dataset record created: %s\n", createResp.ID)

	// Step 2: Process file (encrypt/compress)
	processedData, err := p.processFile(ctx, filePath, opts)
	if err != nil {
		return nil, err
	}

	// Step 3: Upload to presigned URL
	if err := p.uploadToPresignedURL(ctx, createResp.UploadURL, processedData.Data); err != nil {
		return nil, fmt.Errorf("dataset record created but upload failed: %w", err)
	}

	// Step 4: Return dataset (fetch updated record from API)
	dataset := &types.Dataset{}
	err = p.makeAPIRequest(ctx, "GET", fmt.Sprintf("/v1/datasets/%s", url.PathEscape(createResp.ID)), nil, dataset)
	if err != nil {
		// If GET fails, construct a basic dataset response
		fmt.Printf("⚠️  Warning: Failed to fetch dataset details: %v\n", err)
		return &types.Dataset{
			ID:           createResp.ID,
			IDAlias:      createResp.ID,
			Name:         opts.DatasetName,
			Description:  opts.Description,
			ProducerID:   p.CustomerID,
			Category:     opts.Category,
			S3Key:        createResp.S3Key,
			S3BucketName: p.BucketName,
			S3Bucket:     p.BucketName,
		}, nil
	}

	return dataset, nil
}

// updateDataset updates an existing dataset via PATCH /v1/datasets/:id.
// This is used internally when UploadDataset encounters a 409 conflict.
func (p *Producer) updateDataset(ctx context.Context, datasetID string, payload map[string]any) (*types.Dataset, error) {
	// Build update request with only the fields that can be updated
	updatePayload := map[string]any{}

	// Copy allowed update fields from the full payload
	updateableFields := []string{
		"name", "description", "schema", "metadata", "status",
		"visibility", "category", "access_tier", "tags",
		"size_bytes", "record_count", "s3_key", "s3_bucket_name",
		"data_freshness", "version", "version_notes", "last_updated",
		"updated_at", "updated_by",
	}

	for _, field := range updateableFields {
		if val, ok := payload[field]; ok {
			updatePayload[field] = val
		}
	}

	// Make PATCH request (ignore response body - API returns different field names)
	path := fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID))

	if err := p.makeAPIRequest(ctx, "PATCH", path, updatePayload, nil); err != nil {
		return nil, fmt.Errorf("failed to update dataset: %w", err)
	}

	// Construct return value from the payload since PATCH succeeded
	// (API response has different field names like 'id' vs '_id', 'total_size_bytes' vs 'size_bytes')
	sizeBytes, _ := payload["size_bytes"].(int64)
	recordCount, _ := payload["record_count"].(int)
	metadata, _ := payload["metadata"].(map[string]any)
	schema, _ := payload["schema"].(map[string]any)
	tags, _ := payload["tags"].([]string)

	return &types.Dataset{
		ID:            datasetID,
		IDAlias:       datasetID,
		Name:          getStringField(payload, "name"),
		Description:   getStringField(payload, "description"),
		ProducerID:    p.CustomerID,
		Category:      getStringField(payload, "category"),
		DataFreshness: types.DataFreshness(getStringField(payload, "data_freshness")),
		Visibility:    getStringField(payload, "visibility"),
		Status:        getStringField(payload, "status"),
		AccessTier:    getStringField(payload, "access_tier"),
		S3Key:         getStringField(payload, "s3_key"),
		S3BucketName:  p.BucketName,
		S3Bucket:      p.BucketName,
		SizeBytes:     sizeBytes,
		RecordCount:   recordCount,
		Version:       getStringField(payload, "version"),
		Metadata:      metadata,
		Schema:        schema,
		Tags:          tags,
		LastUpdated:   getStringField(payload, "last_updated"),
		UpdatedAt:     getStringField(payload, "updated_at"),
		UpdatedBy:     getStringField(payload, "updated_by"),
	}, nil
}

// getStringField safely extracts a string field from a map
func getStringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// makeAPIRequest makes an authenticated API request.
func (p *Producer) makeAPIRequest(ctx context.Context, method, path string, body, response any) error {
	apiURL, err := url.Parse(p.APIEndpoint + path)
	if err != nil {
		return fmt.Errorf("invalid API URL: %w", err)
	}

	var (
		reqBody  io.Reader
		jsonData []byte
	)

	if body != nil {
		var err error
		jsonData, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}

		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiURL.String(), reqBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Sign request with AWS SigV4.
	creds, err := p.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	// Calculate payload hash for SigV4.
	var payloadHash string

	if body != nil {
		// Hash the actual JSON body.
		h := crypto.SHA256.New()

		h.Write(jsonData)

		payloadHash = fmt.Sprintf("%x", h.Sum(nil))
	} else {
		// Empty payload hash for GET requests.
		payloadHash = types.EmptyPayloadHash
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "execute-api", p.Region, time.Now()); err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	// Execute request.
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)

		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(bodyBytes),
		}
	}

	if response != nil {
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// ListMyDatasets lists all datasets uploaded by this producer
func (p *Producer) ListMyDatasets(ctx context.Context) ([]types.Dataset, error) {
	var datasets []types.Dataset

	path := fmt.Sprintf("/v1/datasets?producer_id=%s", url.QueryEscape(p.CustomerID))

	if err := p.makeAPIRequest(ctx, "GET", path, nil, &datasets); err != nil {
		return nil, err
	}

	return datasets, nil
}

// GetDatasetSubscribers lists all subscribers for a specific dataset.
func (p *Producer) GetDatasetSubscribers(ctx context.Context, datasetID string) ([]types.Subscription, error) {
	var response struct {
		Subscriptions []types.Subscription `json:"subscriptions"`
		Count         int                  `json:"count"`
	}

	path := fmt.Sprintf("/v1/subscriptions?dataset_id=%s", url.QueryEscape(datasetID))

	if err := p.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}

	return response.Subscriptions, nil
}

// RevokeSubscription revokes a subscription.
func (p *Producer) RevokeSubscription(ctx context.Context, subscriptionID string) error {
	path := fmt.Sprintf("/v1/subscriptions/%s/revoke", url.PathEscape(subscriptionID))

	// PUT request with empty body
	if err := p.makeAPIRequest(ctx, http.MethodPut, path, map[string]string{}, nil); err != nil {
		return err
	}

	return nil
}

// ListSubscriptionRequests lists incoming subscription requests for this producer.
// Returns requests that match the specified status filter.
//
// Parameters:
//   - status: Filter by request status. Valid values: "pending", "approved", "rejected".
//     If empty, defaults to "pending".
//
// Returns a slice of subscription requests matching the filter.
func (p *Producer) ListSubscriptionRequests(ctx context.Context, status string) ([]types.SubscriptionRequest, error) {
	// Default to pending if not specified
	if status == "" {
		status = "pending"
	}

	path := fmt.Sprintf("/v1/producers/subscription-requests?status=%s", url.QueryEscape(status))

	var response types.SubscriptionRequestsResponse
	if err := p.makeAPIRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}

	return response.Requests, nil
}

// ApproveSubscriptionRequest approves a subscription request from a consumer.
// This creates the necessary resources (SQS queue, SNS subscription, KMS grants)
// for the consumer to access the producer's datasets.
//
// Parameters:
//   - requestID: The subscription request ID to approve.
//   - opts: Optional parameters for approval:
//   - Notes: Optional internal notes about the approval.
//   - DatasetID: Optional specific dataset ID to grant access to
//     (if not provided, uses the dataset from the original request).
//
// Returns the updated subscription request with status "approved".
func (p *Producer) ApproveSubscriptionRequest(ctx context.Context, requestID string, opts *types.ApproveSubscriptionRequestOptions) (*types.SubscriptionRequest, error) {
	path := fmt.Sprintf("/v1/subscription-requests/%s", url.PathEscape(requestID))

	payload := types.ApproveRejectPayload{
		Action: "approve",
	}

	if opts != nil {
		payload.Notes = opts.Notes
		// Add dataset_id to payload if provided
		// Note: The ApproveRejectPayload doesn't have DatasetID, so we need to use a map
	}

	// Use a map to include optional dataset_id field
	payloadMap := map[string]any{
		"action": "approve",
	}
	if opts != nil {
		if opts.Notes != nil {
			payloadMap["notes"] = *opts.Notes
		}
		if opts.DatasetID != nil {
			payloadMap["dataset_id"] = *opts.DatasetID
		}
	}

	var result types.SubscriptionRequest
	if err := p.makeAPIRequest(ctx, http.MethodPost, path, payloadMap, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// UpdateDataset updates an existing dataset's metadata.
// This is a public API for producers to update dataset information without re-uploading data.
//
// Parameters:
//   - datasetID: The dataset ID to update.
//   - input: DatasetUpdateInput containing fields to update (nil fields are ignored).
//
// Returns the updated dataset.
func (p *Producer) UpdateDataset(ctx context.Context, datasetID string, input types.DatasetUpdateInput) (*types.Dataset, error) {
	path := fmt.Sprintf("/v1/datasets/%s", url.PathEscape(datasetID))

	var result types.Dataset
	if err := p.makeAPIRequest(ctx, http.MethodPatch, path, input, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListSubscribers lists all active subscribers across all of the producer's datasets.
// Provides an aggregated view of who has access to the producer's data.
//
// Returns a SubscribersResponse containing all subscribers with their subscription details.
func (p *Producer) ListSubscribers(ctx context.Context) (*types.SubscribersResponse, error) {
	var response types.SubscribersResponse
	if err := p.makeAPIRequest(ctx, http.MethodGet, "/v1/producers/subscribers", nil, &response); err != nil {
		return nil, err
	}

	return &response, nil
}

// RejectSubscriptionRequest rejects a subscription request from a consumer.
//
// Parameters:
//   - requestID: The subscription request ID to reject.
//   - reason: Optional reason for rejection (will be visible to the consumer).
//
// Returns the updated subscription request with status "rejected".
func (p *Producer) RejectSubscriptionRequest(ctx context.Context, requestID string, reason string) (*types.SubscriptionRequest, error) {
	path := fmt.Sprintf("/v1/subscription-requests/%s", url.PathEscape(requestID))

	payloadMap := map[string]any{
		"action": "reject",
	}
	if reason != "" {
		payloadMap["reason"] = reason
	}

	var result types.SubscriptionRequest
	if err := p.makeAPIRequest(ctx, http.MethodPost, path, payloadMap, &result); err != nil {
		return nil, err
	}

	return &result, nil
}
