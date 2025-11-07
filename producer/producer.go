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
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Producer handles uploading and managing datasets on Helix Connect
type Producer struct {
	CustomerID   string
	APIEndpoint  string
	Region       string
	BucketName   string
	KMSKeyID     string
	awsConfig    aws.Config
	kmsClient    *kms.Client
	s3Client     *s3.Client
	httpClient   *http.Client
}

// Config contains configuration for the Producer
type Config struct {
	CustomerID          string
	AWSAccessKeyID      string
	AWSSecretAccessKey  string
	APIEndpoint         string
	Region              string
}

// UploadOptions contains options for uploading datasets.
//
// Security and Performance Defaults (v2.0+):
//   - Encrypt: true (KMS envelope encryption enabled by default)
//   - Compress: true (gzip compression enabled by default)
//   - CompressionLevel: 6 (balanced speed/size ratio)
//
// Use NewUploadOptions() to get secure defaults:
//   opts := producer.NewUploadOptions("my-dataset")
//   opts.Description = "My data"
//   producer.UploadDataset(ctx, filePath, opts)
//
// To disable encryption or compression:
//   opts := producer.NewUploadOptions("my-dataset")
//   opts.Encrypt = false  // Disable encryption
//   opts.Compress = false // Disable compression
type UploadOptions struct {
	DatasetName      string
	Description      string
	Category         string
	DataFreshness    string
	Metadata         map[string]interface{}
	Encrypt          bool // Recommended: true for security (use NewUploadOptions for secure defaults)
	Compress         bool // Recommended: true for cost optimization (use NewUploadOptions for secure defaults)
	CompressionLevel int  // Recommended: 6 (gzip compression level 1-9)
}

// NewUploadOptions creates UploadOptions with secure defaults.
// This is the recommended way to create upload options (v2.0+).
//
// Defaults:
//   - Category: "general"
//   - DataFreshness: "daily"
//   - Encrypt: true (KMS envelope encryption)
//   - Compress: true (gzip compression)
//   - CompressionLevel: 6 (balanced speed/size)
//
// Example:
//   opts := producer.NewUploadOptions("my-dataset")
//   opts.Description = "My dataset description"
//   opts.Metadata = map[string]interface{}{"key": "value"}
//   dataset, err := producer.UploadDataset(ctx, "/path/to/file", opts)
func NewUploadOptions(datasetName string) UploadOptions {
	return UploadOptions{
		DatasetName:      datasetName,
		Category:         "general",
		DataFreshness:    "daily",
		Encrypt:          true,
		Compress:         true,
		CompressionLevel: 6,
	}
}

// Dataset represents a dataset in the catalog
type Dataset struct {
	ID            string                 `json:"_id,omitempty"`
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	ProducerID    string                 `json:"producer_id"`
	Category      string                 `json:"category"`
	DataFreshness string                 `json:"data_freshness"`
	S3Key         string                 `json:"s3_key"`
	SizeBytes     int64                  `json:"size_bytes"`
	Metadata      map[string]interface{} `json:"metadata"`
}

// NewProducer creates a new Producer instance
func NewProducer(cfg Config) (*Producer, error) {
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

	// Get producer-specific resources from SSM
	ssmClient := ssm.NewFromConfig(awsCfg)

	// Get S3 bucket name
	bucketParam := fmt.Sprintf("/helix/customers/%s/s3_bucket", cfg.CustomerID)
	bucketResp, err := ssmClient.GetParameter(context.Background(), &ssm.GetParameterInput{
		Name: aws.String(bucketParam),
	})
	if err != nil {
		return nil, fmt.Errorf("S3 bucket not found for producer %s: %w", cfg.CustomerID, err)
	}

	// Get KMS key ID (optional for producers)
	kmsKeyID := ""
	kmsParam := fmt.Sprintf("/helix/customers/%s/kms_key_id", cfg.CustomerID)
	kmsResp, err := ssmClient.GetParameter(context.Background(), &ssm.GetParameterInput{
		Name: aws.String(kmsParam),
	})
	if err != nil {
		fmt.Printf("Warning: KMS key not found, encryption will be disabled: %v\n", err)
	} else {
		kmsKeyID = *kmsResp.Parameter.Value
	}

	return &Producer{
		CustomerID:  cfg.CustomerID,
		APIEndpoint: cfg.APIEndpoint,
		Region:      cfg.Region,
		BucketName:  *bucketResp.Parameter.Value,
		KMSKeyID:    kmsKeyID,
		awsConfig:   awsCfg,
		kmsClient:   kms.NewFromConfig(awsCfg),
		s3Client:    s3.NewFromConfig(awsCfg),
		httpClient:  &http.Client{},
	}, nil
}

// compressData compresses data using gzip
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

	// Generate random data key and IV
	dataKey := make([]byte, 32) // 256-bit key for AES-256
	if _, err := rand.Read(dataKey); err != nil {
		return nil, fmt.Errorf("failed to generate data key: %w", err)
	}

	iv := make([]byte, 16) // 128-bit IV for GCM mode (16 bytes to match Python)
	if _, err := rand.Read(iv); err != nil {
		return nil, fmt.Errorf("failed to generate IV: %w", err)
	}

	// Encrypt data with data key using AES-256-GCM
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Use 16-byte nonce to match Python's os.urandom(16)
	aesGCM, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Encrypt the data (auth tag is automatically appended)
	encryptedData := aesGCM.Seal(nil, iv, data, nil)

	// Split encrypted data and auth tag
	authTagSize := aesGCM.Overhead()
	actualEncryptedData := encryptedData[:len(encryptedData)-authTagSize]
	authTag := encryptedData[len(encryptedData)-authTagSize:]

	// Encrypt the data key with KMS
	encryptOutput, err := p.kmsClient.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(p.KMSKeyID),
		Plaintext: dataKey,
	})
	if err != nil {
		return nil, fmt.Errorf("KMS encryption failed: %w", err)
	}

	// Package: [4 bytes: key length][encrypted key][16 bytes: IV][16 bytes: tag][encrypted data]
	var result bytes.Buffer

	// Write encrypted key length (4 bytes, big-endian)
	if err := binary.Write(&result, binary.BigEndian, uint32(len(encryptOutput.CiphertextBlob))); err != nil {
		return nil, fmt.Errorf("failed to write key length: %w", err)
	}

	// Write encrypted key
	result.Write(encryptOutput.CiphertextBlob)

	// Write IV (16 bytes)
	result.Write(iv)

	// Write auth tag (16 bytes)
	result.Write(authTag)

	// Write encrypted data
	result.Write(actualEncryptedData)

	return result.Bytes(), nil
}

// UploadDataset uploads a dataset with optional encryption and compression.
//
// Recommended usage with secure defaults (v2.0+):
//   opts := producer.NewUploadOptions("my-dataset")
//   opts.Description = "My dataset description"
//   dataset, err := producer.UploadDataset(ctx, "/path/to/file", opts)
//
// This method supports encryption (KMS envelope encryption) and compression (gzip).
// Use NewUploadOptions() to get secure defaults with both enabled.
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*Dataset, error) {
	// Set defaults for fields not specified
	if opts.Category == "" {
		opts.Category = "general"
	}
	if opts.DataFreshness == "" {
		opts.DataFreshness = "daily"
	}
	if opts.CompressionLevel == 0 {
		opts.CompressionLevel = 6
	}

	// Validate encryption capability
	if opts.Encrypt && p.KMSKeyID == "" {
		fmt.Println("Warning: Encryption requested but KMS key not found. Skipping encryption.")
		opts.Encrypt = false
	}

	// Read original file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	originalSize := int64(len(data))

	// Track sizes for metadata
	sizes := map[string]interface{}{
		"original_size_bytes":   originalSize,
		"compressed_size_bytes": originalSize,
		"encrypted_size_bytes":  originalSize,
		"encryption_enabled":    opts.Encrypt,
		"compression_enabled":   opts.Compress,
	}

	// Step 1: Compress FIRST (if enabled)
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

	// Step 2: Encrypt SECOND (if enabled)
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

	// Generate S3 key with consistent filename (no date, no randomness)
	// This enables in-place updates - same dataset name = same S3 key = file overwrite
	fileName := "data.ndjson"
	if opts.Compress {
		fileName += ".gz"
	}
	s3Key := fmt.Sprintf("datasets/%s/%s", opts.DatasetName, fileName)

	// Build S3 object tags for cost tracking
	// Format: CustomerID=value&Component=storage&Purpose=dataset-storage&DatasetName=value
	tags := fmt.Sprintf("CustomerID=%s&Component=%s&Purpose=%s&DatasetName=%s",
		url.QueryEscape(p.CustomerID),
		url.QueryEscape("storage"),
		url.QueryEscape("dataset-storage"),
		url.QueryEscape(opts.DatasetName),
	)

	// Upload to S3
	fmt.Printf("📤 Uploading %d bytes to S3...\n", len(data))
	_, err = p.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:  aws.String(p.BucketName),
		Key:     aws.String(s3Key),
		Body:    bytes.NewReader(data),
		Tagging: aws.String(tags),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to upload to S3: %w", err)
	}

	fmt.Printf("✅ Uploaded to s3://%s/%s (tagged: CustomerID=%s)\n", p.BucketName, s3Key, p.CustomerID)

	// Merge metadata
	finalMetadata := make(map[string]interface{})
	for k, v := range opts.Metadata {
		finalMetadata[k] = v
	}
	for k, v := range sizes {
		finalMetadata[k] = v
	}

	// Register dataset in catalog via API
	dataset := &Dataset{
		Name:          opts.DatasetName,
		Description:   opts.Description,
		ProducerID:    p.CustomerID,
		Category:      opts.Category,
		DataFreshness: opts.DataFreshness,
		S3Key:         s3Key,
		SizeBytes:     sizes["compressed_size_bytes"].(int64),
		Metadata:      finalMetadata,
	}

	// Make API request to register dataset
	err = p.makeAPIRequest(ctx, "POST", "/v1/datasets", dataset, dataset)
	if err != nil {
		fmt.Printf("⚠️  Warning: File uploaded but catalog registration failed: %v\n", err)
		return &Dataset{
			S3Key:    s3Key,
			Metadata: map[string]interface{}{"status": "uploaded_unregistered", "error": err.Error()},
		}, nil
	}

	return dataset, nil
}

// makeAPIRequest makes an authenticated API request
func (p *Producer) makeAPIRequest(ctx context.Context, method, path string, body, response interface{}) error {
	apiURL, err := url.Parse(p.APIEndpoint + path)
	if err != nil {
		return fmt.Errorf("invalid API URL: %w", err)
	}

	var reqBody io.Reader
	var jsonData []byte
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

	// Sign request with AWS SigV4
	creds, err := p.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	// Calculate payload hash for SigV4
	var payloadHash string
	if body != nil {
		// Hash the actual JSON body
		h := crypto.SHA256.New()
		h.Write(jsonData)
		payloadHash = fmt.Sprintf("%x", h.Sum(nil))
	} else {
		// Empty payload hash for GET requests
		payloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "execute-api", p.Region, time.Now()); err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	// Execute request
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if response != nil {
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// ListMyDatasets lists all datasets uploaded by this producer
func (p *Producer) ListMyDatasets(ctx context.Context) ([]Dataset, error) {
	var datasets []Dataset
	path := fmt.Sprintf("/v1/datasets?producer_id=%s", url.QueryEscape(p.CustomerID))
	err := p.makeAPIRequest(ctx, "GET", path, nil, &datasets)
	if err != nil {
		return nil, err
	}
	return datasets, nil
}
