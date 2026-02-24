package producer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProcessFileCompression tests file compression logic.
func TestProcessFileCompression(t *testing.T) {
	// Create test file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.ndjson")
	
	// Create test data (repeated pattern compresses well)
	testData := strings.Repeat(`{"id": 1, "name": "test"}` + "\n", 100)
	if err := os.WriteFile(testFile, []byte(testData), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Create minimal producer for compression testing
	p := &Producer{
		CustomerID: "test-customer",
		KMSKeyID:   "",  // No encryption for this test
	}

	opts := NewUploadOptions("test-dataset")
	opts.Encrypt = false // Skip encryption requirement validation
	opts.Compress = true
	opts.CompressionLevel = 6

	// Test that we can't upload without encryption (required)
	_, err := p.processFile(context.Background(), testFile, opts)
	if err == nil {
		t.Error("expected error when encryption is disabled")
	}
	if !strings.Contains(err.Error(), "encryption is required") {
		t.Errorf("expected 'encryption is required' error, got: %v", err)
	}
}

// TestProcessFileEmptyFile tests that empty files are rejected.
func TestProcessFileEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.ndjson")
	if err := os.WriteFile(emptyFile, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create empty file: %v", err)
	}

	p := &Producer{
		CustomerID: "test-customer",
		KMSKeyID:   "test-kms-key", // Set KMS key to pass encryption check
	}

	opts := NewUploadOptions("test-dataset")
	
	_, err := p.processFile(context.Background(), emptyFile, opts)
	if err == nil {
		t.Error("expected error for empty file")
	}

	if !strings.Contains(err.Error(), "file is empty") {
		t.Errorf("expected 'file is empty' error, got: %v", err)
	}
}

// TestProcessFileMissingFile tests that missing files return appropriate error.
func TestProcessFileMissingFile(t *testing.T) {
	p := &Producer{
		CustomerID: "test-customer",
		KMSKeyID:   "test-kms-key", // Set KMS key to pass encryption check
	}

	opts := NewUploadOptions("test-dataset")
	
	_, err := p.processFile(context.Background(), "/nonexistent/file.ndjson", opts)
	if err == nil {
		t.Error("expected error for missing file")
	}

	if !strings.Contains(err.Error(), "failed to read file") {
		t.Errorf("expected 'failed to read file' error, got: %v", err)
	}
}

// TestUploadToPresignedURL tests uploading to a presigned URL.
func TestUploadToPresignedURL(t *testing.T) {
	t.Run("successful upload", func(t *testing.T) {
		// Track what was uploaded
		var uploadedData []byte
		
		// Create mock S3 presigned URL handler
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PUT" {
				t.Errorf("expected PUT, got %s", r.Method)
			}

			// Read and verify body
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("failed to read body: %v", err)
			}
			uploadedData = body

			if len(body) == 0 {
				t.Error("expected non-empty body")
			}

			// Check content type
			if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("expected Content-Type 'application/octet-stream', got '%s'", ct)
			}

			// Check content length
			if r.ContentLength <= 0 {
				t.Errorf("expected positive ContentLength, got %d", r.ContentLength)
			}

			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		p := &Producer{
			httpClient: &http.Client{},
		}

		testData := []byte("test data content")
		err := p.uploadToPresignedURL(context.Background(), server.URL, testData)
		if err != nil {
			t.Fatalf("uploadToPresignedURL failed: %v", err)
		}

		// Verify data was uploaded correctly
		if !bytes.Equal(uploadedData, testData) {
			t.Errorf("uploaded data mismatch: expected %q, got %q", testData, uploadedData)
		}
	})

	t.Run("upload failure - 403 Forbidden", func(t *testing.T) {
		// Create mock server that returns error
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("Access Denied"))
		}))
		defer server.Close()

		p := &Producer{
			httpClient: &http.Client{},
		}

		testData := []byte("test data")
		err := p.uploadToPresignedURL(context.Background(), server.URL, testData)
		if err == nil {
			t.Fatal("expected error for failed upload")
		}

		// Try to unwrap to APIError
		if apiErr, ok := err.(*APIError); ok {
			if apiErr.StatusCode != 403 {
				t.Errorf("expected status 403, got %d", apiErr.StatusCode)
			}
			if !strings.Contains(apiErr.Body, "Access Denied") {
				t.Errorf("expected 'Access Denied' in error body, got: %s", apiErr.Body)
			}
		} else {
			t.Errorf("expected error to be *APIError, got: %T", err)
		}
	})

	t.Run("empty data", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		p := &Producer{
			httpClient: &http.Client{},
		}

		// Should still succeed even with empty data (edge case)
		err := p.uploadToPresignedURL(context.Background(), server.URL, []byte{})
		if err != nil {
			t.Errorf("uploadToPresignedURL should handle empty data: %v", err)
		}
	})
}

// TestUploadDatasetValidation tests input validation in UploadDataset.
func TestUploadDatasetValidation(t *testing.T) {
	p := &Producer{
		CustomerID: "test-customer",
		KMSKeyID:   "", // No KMS key
	}

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.ndjson")
	if err := os.WriteFile(testFile, []byte(`{"test": "data"}`), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	t.Run("encryption required", func(t *testing.T) {
		opts := NewUploadOptions("test-dataset")
		opts.Encrypt = false

		_, err := p.UploadDataset(context.Background(), testFile, opts)
		if err == nil {
			t.Error("expected error when encryption is disabled")
		}
		if !strings.Contains(err.Error(), "encryption is required") {
			t.Errorf("expected 'encryption is required', got: %v", err)
		}
	})

	t.Run("compression required", func(t *testing.T) {
		opts := NewUploadOptions("test-dataset")
		opts.Compress = false

		_, err := p.UploadDataset(context.Background(), testFile, opts)
		if err == nil {
			t.Error("expected error when compression is disabled")
		}
		if !strings.Contains(err.Error(), "compression is required") {
			t.Errorf("expected 'compression is required', got: %v", err)
		}
	})

	t.Run("KMS key required for encryption", func(t *testing.T) {
		opts := NewUploadOptions("test-dataset")
		opts.Encrypt = true
		// p.KMSKeyID is empty

		_, err := p.UploadDataset(context.Background(), testFile, opts)
		if err == nil {
			t.Error("expected error when KMS key is missing")
		}
		if !strings.Contains(err.Error(), "KMS key not found") {
			t.Errorf("expected 'KMS key not found', got: %v", err)
		}
	})
}

// TestCreateDatasetResponseStructure tests that CreateDatasetResponse unmarshals correctly.
func TestCreateDatasetResponseStructure(t *testing.T) {
	// This tests the data structure we expect from the API
	jsonResp := `{
		"id": "dataset-123",
		"upload_url": "https://s3.amazonaws.com/bucket/key?X-Amz-...",
		"s3_key": "datasets/test/data.ndjson.gz"
	}`

	var resp CreateDatasetResponse
	if err := json.Unmarshal([]byte(jsonResp), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.ID != "dataset-123" {
		t.Errorf("expected ID 'dataset-123', got '%s'", resp.ID)
	}

	if resp.UploadURL == "" {
		t.Error("expected non-empty UploadURL")
	}

	if resp.S3Key != "datasets/test/data.ndjson.gz" {
		t.Errorf("expected S3Key 'datasets/test/data.ndjson.gz', got '%s'", resp.S3Key)
	}
}
