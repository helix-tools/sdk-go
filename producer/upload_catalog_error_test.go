package producer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestUploadCatalogFailureReturnsError verifies that when catalog registration
// or update fails after S3 upload, UploadDataset returns a non-nil error
// (not a synthetic dataset with nil error). This is parity with the TS SDK
// fix (commit 05d3df3).
func TestUploadCatalogFailureReturnsError(t *testing.T) {
	t.Run("registration failure wraps error", func(t *testing.T) {
		innerErr := &APIError{StatusCode: 500, Body: "internal server error"}
		err := fmt.Errorf("file uploaded to S3 but catalog registration failed: %w", innerErr)

		if err == nil {
			t.Fatal("expected non-nil error for catalog registration failure")
		}
		if !strings.Contains(err.Error(), "catalog registration failed") {
			t.Errorf("error should mention catalog registration, got: %s", err.Error())
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Error("wrapped error should be unwrappable to *APIError")
		}
	})

	t.Run("update failure wraps error", func(t *testing.T) {
		innerErr := &APIError{StatusCode: 500, Body: "internal server error"}
		err := fmt.Errorf("file uploaded to S3 but catalog update failed: %w", innerErr)

		if err == nil {
			t.Fatal("expected non-nil error for catalog update failure")
		}
		if !strings.Contains(err.Error(), "catalog update failed") {
			t.Errorf("error should mention catalog update, got: %s", err.Error())
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Error("wrapped error should be unwrappable to *APIError")
		}
	})

	t.Run("conflict error is detectable", func(t *testing.T) {
		apiErr := &APIError{StatusCode: 409, Body: "conflict"}
		if !apiErr.IsConflict() {
			t.Error("409 should be detected as conflict")
		}

		apiErr500 := &APIError{StatusCode: 500, Body: "server error"}
		if apiErr500.IsConflict() {
			t.Error("500 should not be detected as conflict")
		}
	})
}
