package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// emptyPayloadHash is the SHA256 hash of an empty payload.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Client wraps HTTP client with AWS SigV4 authentication for API testing.
type Client struct {
	baseURL    string
	httpClient *http.Client
	awsConfig  aws.Config
	region     string
	customerID string
}

// APIError represents an error response from the API.
type APIError struct {
	StatusCode int
	Body       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Message)
	}

	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Body)
}

// NewClient creates a new API client with AWS SigV4 authentication.
func NewClient(ctx context.Context, baseURL string, creds Credentials, region string) (*Client, error) {
	awsCfg, err := NewAWSConfig(ctx, creds, region)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS config: %w", err)
	}

	return &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		awsConfig:  awsCfg,
		region:     region,
		customerID: creds.CustomerID,
	}, nil
}

// NewTestClient creates a new API client for testing, using the test configuration.
// It handles test skipping if credentials are not available.
func NewTestClient(t *testing.T, cfg TestConfig, creds Credentials) *Client {
	t.Helper()

	ctx := context.Background()

	client, err := NewClient(ctx, cfg.BaseURL, creds, cfg.Region)
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}

	return client
}

// CustomerID returns the customer ID associated with this client.
func (c *Client) CustomerID() string {
	return c.customerID
}

// BaseURL returns the base URL of the API.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Request makes an authenticated API request.
func (c *Client) Request(ctx context.Context, method, path string, body, result any) error {
	apiURL, err := url.Parse(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("invalid API URL: %w", err)
	}

	var (
		reqBody  io.Reader
		jsonData []byte
	)

	if body != nil {
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
	creds, err := c.awsConfig.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	// Calculate payload hash for SigV4.
	var payloadHash string

	if body != nil {
		h := sha256.New()
		h.Write(jsonData)
		payloadHash = fmt.Sprintf("%x", h.Sum(nil))
	} else {
		payloadHash = emptyPayloadHash
	}

	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, req, payloadHash, "execute-api", c.region, time.Now()); err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	// Execute request.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	defer resp.Body.Close()

	// Read response body.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for errors.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}

		// Try to extract error message from JSON response.
		var errResp struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}

		if json.Unmarshal(respBody, &errResp) == nil {
			if errResp.Error != "" {
				apiErr.Message = errResp.Error
			} else if errResp.Message != "" {
				apiErr.Message = errResp.Message
			}
		}

		return apiErr
	}

	// Decode response if expected.
	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return nil
}

// Get makes an authenticated GET request.
func (c *Client) Get(ctx context.Context, path string, result any) error {
	return c.Request(ctx, http.MethodGet, path, nil, result)
}

// Post makes an authenticated POST request.
func (c *Client) Post(ctx context.Context, path string, body, result any) error {
	return c.Request(ctx, http.MethodPost, path, body, result)
}

// Patch makes an authenticated PATCH request.
func (c *Client) Patch(ctx context.Context, path string, body, result any) error {
	return c.Request(ctx, http.MethodPatch, path, body, result)
}

// Put makes an authenticated PUT request.
func (c *Client) Put(ctx context.Context, path string, body, result any) error {
	return c.Request(ctx, http.MethodPut, path, body, result)
}

// Delete makes an authenticated DELETE request.
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.Request(ctx, http.MethodDelete, path, nil, nil)
}

// IsNotFoundError checks if an error is a 404 Not Found error.
func IsNotFoundError(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode == http.StatusNotFound
	}

	return false
}

// IsForbiddenError checks if an error is a 403 Forbidden error.
func IsForbiddenError(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode == http.StatusForbidden
	}

	return false
}

// IsConflictError checks if an error is a 409 Conflict error.
func IsConflictError(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode == http.StatusConflict
	}

	return false
}

// IsBadRequestError checks if an error is a 400 Bad Request error.
func IsBadRequestError(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.StatusCode == http.StatusBadRequest
	}

	return false
}
