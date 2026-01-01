// Package api provides integration test utilities for the Helix Connect API.
//
// It includes a test HTTP client with AWS SigV4 authentication, configuration
// loading, and cleanup utilities for managing test resources.
package api

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// DefaultAPIEndpoint is the default API endpoint for production.
const DefaultAPIEndpoint = "https://api-go.helix.tools"

// DefaultRegion is the default AWS region.
const DefaultRegion = "us-east-1"

// Credentials holds AWS credentials for a customer.
type Credentials struct {
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	CustomerID         string
}

// TestConfig holds configuration for integration tests.
type TestConfig struct {
	// BaseURL is the API endpoint (e.g., https://api-go.helix.tools or http://localhost:8080).
	BaseURL string

	// Region is the AWS region.
	Region string

	// ProducerCredentials contains credentials for producer operations.
	ProducerCredentials Credentials

	// ConsumerCredentials contains credentials for consumer operations.
	ConsumerCredentials Credentials

	// TestDatasetID is an optional known dataset ID for subscription tests.
	TestDatasetID string
}

// WithCredentials returns a copy of the config with the specified credentials.
func (c TestConfig) WithCredentials(creds Credentials) TestConfig {
	newConfig := c
	newConfig.ProducerCredentials = creds
	return newConfig
}

// LoadTestConfig loads test configuration from environment variables.
// Required variables depend on test type:
//   - HELIX_TEST_BASE_URL: API endpoint (default: https://api-go.helix.tools)
//   - HELIX_TEST_REGION: AWS region (default: us-east-1)
//   - HELIX_TEST_PRODUCER_ID, HELIX_TEST_PRODUCER_AWS_ACCESS_KEY_ID, HELIX_TEST_PRODUCER_AWS_SECRET_ACCESS_KEY
//   - HELIX_TEST_CONSUMER_ID, HELIX_TEST_CONSUMER_AWS_ACCESS_KEY_ID, HELIX_TEST_CONSUMER_AWS_SECRET_ACCESS_KEY
//   - HELIX_TEST_DATASET_ID: Optional dataset ID for subscription tests
func LoadTestConfig(t *testing.T) TestConfig {
	t.Helper()

	cfg := TestConfig{
		BaseURL: getEnvOrDefault("HELIX_TEST_BASE_URL", DefaultAPIEndpoint),
		Region:  getEnvOrDefault("HELIX_TEST_REGION", DefaultRegion),
	}

	// Producer credentials.
	cfg.ProducerCredentials = Credentials{
		CustomerID:         os.Getenv("HELIX_TEST_PRODUCER_ID"),
		AWSAccessKeyID:     os.Getenv("HELIX_TEST_PRODUCER_AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("HELIX_TEST_PRODUCER_AWS_SECRET_ACCESS_KEY"),
	}

	// Consumer credentials.
	cfg.ConsumerCredentials = Credentials{
		CustomerID:         os.Getenv("HELIX_TEST_CONSUMER_ID"),
		AWSAccessKeyID:     os.Getenv("HELIX_TEST_CONSUMER_AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("HELIX_TEST_CONSUMER_AWS_SECRET_ACCESS_KEY"),
	}

	// Optional test dataset ID.
	cfg.TestDatasetID = os.Getenv("HELIX_TEST_DATASET_ID")

	return cfg
}

// RequireProducerCredentials validates that producer credentials are set.
// If not set, it skips the test.
func (c TestConfig) RequireProducerCredentials(t *testing.T) {
	t.Helper()

	if c.ProducerCredentials.CustomerID == "" {
		t.Skip("HELIX_TEST_PRODUCER_ID not set")
	}

	if c.ProducerCredentials.AWSAccessKeyID == "" {
		t.Skip("HELIX_TEST_PRODUCER_AWS_ACCESS_KEY_ID not set")
	}

	if c.ProducerCredentials.AWSSecretAccessKey == "" {
		t.Skip("HELIX_TEST_PRODUCER_AWS_SECRET_ACCESS_KEY not set")
	}
}

// RequireConsumerCredentials validates that consumer credentials are set.
// If not set, it skips the test.
func (c TestConfig) RequireConsumerCredentials(t *testing.T) {
	t.Helper()

	if c.ConsumerCredentials.CustomerID == "" {
		t.Skip("HELIX_TEST_CONSUMER_ID not set")
	}

	if c.ConsumerCredentials.AWSAccessKeyID == "" {
		t.Skip("HELIX_TEST_CONSUMER_AWS_ACCESS_KEY_ID not set")
	}

	if c.ConsumerCredentials.AWSSecretAccessKey == "" {
		t.Skip("HELIX_TEST_CONSUMER_AWS_SECRET_ACCESS_KEY not set")
	}
}

// RequireTestDatasetID validates that a test dataset ID is set.
// If not set, it skips the test.
func (c TestConfig) RequireTestDatasetID(t *testing.T) {
	t.Helper()

	if c.TestDatasetID == "" {
		t.Skip("HELIX_TEST_DATASET_ID not set")
	}
}

// LoadCredentialsFromSSM loads customer credentials from AWS SSM Parameter Store.
// It uses the AWS helix profile and fetches:
//   - /helix/production/customers/{customerID}/aws_access_key_id
//   - /helix/production/customers/{customerID}/aws_secret_access_key
func LoadCredentialsFromSSM(ctx context.Context, customerID string) (Credentials, error) {
	// Load AWS config with helix profile.
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(DefaultRegion),
		config.WithSharedConfigProfile("helix"),
	)
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to load AWS config: %w", err)
	}

	ssmClient := ssm.NewFromConfig(awsCfg)

	// Get access key ID.
	accessKeyParam := fmt.Sprintf("/helix/production/customers/%s/aws_access_key_id", customerID)
	accessKeyResp, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(accessKeyParam),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to get access key from SSM: %w", err)
	}

	// Get secret access key.
	secretKeyParam := fmt.Sprintf("/helix/production/customers/%s/aws_secret_access_key", customerID)
	secretKeyResp, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(secretKeyParam),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("failed to get secret key from SSM: %w", err)
	}

	return Credentials{
		CustomerID:         customerID,
		AWSAccessKeyID:     *accessKeyResp.Parameter.Value,
		AWSSecretAccessKey: *secretKeyResp.Parameter.Value,
	}, nil
}

// NewAWSConfig creates an AWS config with static credentials.
func NewAWSConfig(ctx context.Context, creds Credentials, region string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			creds.AWSAccessKeyID,
			creds.AWSSecretAccessKey,
			"",
		)),
	)
}

// getEnvOrDefault returns the environment variable value or a default.
func getEnvOrDefault(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultValue
}
