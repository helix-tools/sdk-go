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
		cfg.APIEndpoint = "https://z94fkomfij.execute-api.us-east-1.amazonaws.com"
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
	//
	// TODO: Get pattern from AWS SSM.
	bucketParam := fmt.Sprintf("/helix/customers/%s/s3_bucket", cfg.CustomerID)
	bucketResp, err := ssmClient.GetParameter(context.Background(), &ssm.GetParameterInput{
		Name: aws.String(bucketParam),
	})
	if err != nil {
		return nil, fmt.Errorf("S3 bucket not found for producer %s: %w", cfg.CustomerID, err)
	}

	// Get KMS key ID.
	//
	// TODO: Get pattern from AWS SSM.
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
		APIEndpoint: cfg.APIEndpoint,
		BucketName:  *bucketResp.Parameter.Value,
		CustomerID:  cfg.CustomerID,
		KMSKeyID:    kmsKeyID,
		Region:      cfg.Region,

		awsConfig:  awsCfg,
		httpClient: &http.Client{},
		kmsClient:  kms.NewFromConfig(awsCfg),
		s3Client:   s3.NewFromConfig(awsCfg),
	}, nil
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

// UploadDataset uploads a dataset with optional encryption and compression.
//
// NOTE: Use NewUploadOptions() to get sane defaults.
func (p *Producer) UploadDataset(ctx context.Context, filePath string, opts UploadOptions) (*types.Dataset, error) {
	// Set defaults for fields not specified.
	if opts.Category == "" {
		opts.Category = "general"
	}

	if opts.DataFreshness == "" {
		opts.DataFreshness = types.DataFreshnessDaily
	}

	if opts.CompressionLevel == 0 {
		opts.CompressionLevel = 6
	}

	// Validate encryption capability.
	if !opts.Encrypt {
		return nil, fmt.Errorf("encryption is required for dataset uploads")
	}

	if !opts.Compress {
		return nil, fmt.Errorf("compression is required for dataset uploads")
	}

	if opts.Encrypt && p.KMSKeyID == "" {
		return nil, fmt.Errorf("encryption requested but KMS key not found")

	}

	// Read original file.
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	originalSize := int64(len(data))

	// Track sizes for metadata.
	sizes := map[string]any{
		"original_size_bytes":   originalSize,
		"compressed_size_bytes": originalSize,
		"encrypted_size_bytes":  originalSize,
		"encryption_enabled":    opts.Encrypt,
		"compression_enabled":   opts.Compress,
	}

	// Step 1: Compress FIRST.
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

	// Step 2: Encrypt SECOND.
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

	// Generate S3 key with consistent filename. This enables in-place updates.
	fileName := "data.ndjson"

	if opts.Compress {
		fileName += ".gz"
	}

	// TODO: Get this from AWS SSM.
	s3Key := fmt.Sprintf("datasets/%s/%s", opts.DatasetName, fileName)

	// Build S3 object tags for cost tracking.
	//
	// Format: CustomerID=value&Component=storage&Purpose=dataset-storage&DatasetName=value
	tags := fmt.Sprintf("CustomerID=%s&Component=%s&Purpose=%s&DatasetName=%s",
		url.QueryEscape(p.CustomerID),
		url.QueryEscape("storage"),
		url.QueryEscape("dataset-storage"),
		url.QueryEscape(opts.DatasetName),
	)

	// Upload to S3.
	fmt.Printf("📤 Uploading %d bytes to S3...\n", len(data))

	if _, err = p.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:  aws.String(p.BucketName),
		Key:     aws.String(s3Key),
		Body:    bytes.NewReader(data),
		Tagging: aws.String(tags),
	}); err != nil {
		return nil, fmt.Errorf("failed to upload to S3: %w", err)
	}

	fmt.Printf("✅ Uploaded to s3://%s/%s (tagged: CustomerID=%s)\n", p.BucketName, s3Key, p.CustomerID)

	// Merge metadata.
	finalMetadata := make(map[string]any)

	// Copy user-provided metadata first.
	maps.Copy(finalMetadata, opts.Metadata)

	// Copy sizes next (overwrites any user-provided size fields).
	maps.Copy(finalMetadata, sizes)

	// Register dataset in catalog via API
	dataset := &types.Dataset{
		Category:      opts.Category,
		DataFreshness: opts.DataFreshness,
		Description:   opts.Description,
		Metadata:      finalMetadata,
		Name:          opts.DatasetName,
		ProducerID:    p.CustomerID,
		S3Key:         s3Key,
		SizeBytes:     sizes["compressed_size_bytes"].(int64),
	}

	// Make API request to register dataset.
	err = p.makeAPIRequest(ctx, "POST", "/v1/datasets", dataset, dataset)
	if err != nil {
		fmt.Printf("⚠️  Warning: File uploaded but catalog registration failed: %v\n", err)

		return &types.Dataset{
			S3Key:    s3Key,
			Metadata: map[string]any{"status": "uploaded_unregistered", "error": err.Error()},
		}, nil
	}

	return dataset, nil
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
func (p *Producer) ListMyDatasets(ctx context.Context) ([]types.Dataset, error) {
	var datasets []types.Dataset

	path := fmt.Sprintf("/v1/datasets?producer_id=%s", url.QueryEscape(p.CustomerID))

	if err := p.makeAPIRequest(ctx, "GET", path, nil, &datasets); err != nil {
		return nil, err
	}

	return datasets, nil
}
