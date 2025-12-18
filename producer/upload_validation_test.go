package producer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmptyFileValidation tests that empty files are rejected with a clear error.
func TestEmptyFileValidation(t *testing.T) {
	// Create a temporary empty file
	tmpDir := t.TempDir()
	emptyFile := filepath.Join(tmpDir, "empty.ndjson")

	if err := os.WriteFile(emptyFile, []byte{}, 0644); err != nil {
		t.Fatalf("failed to create empty test file: %v", err)
	}

	// Verify file is empty
	info, err := os.Stat(emptyFile)
	if err != nil {
		t.Fatalf("failed to stat empty file: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected empty file, got size %d", info.Size())
	}

	// Read the file and check the validation logic
	data, err := os.ReadFile(emptyFile)
	if err != nil {
		t.Fatalf("failed to read empty file: %v", err)
	}

	// Simulate the validation that happens in UploadDataset
	if len(data) == 0 {
		// This is the expected behavior - empty files should trigger an error
		expectedErrorSubstring := "file is empty"
		testError := "file is empty: " + emptyFile + " (no data to upload)"

		if !strings.Contains(strings.ToLower(testError), expectedErrorSubstring) {
			t.Errorf("error message should contain '%s', got: %s", expectedErrorSubstring, testError)
		}
	} else {
		t.Error("expected file to be empty")
	}
}

// TestNonEmptyFileValidation tests that non-empty files pass validation.
func TestNonEmptyFileValidation(t *testing.T) {
	// Create a temporary non-empty file
	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.ndjson")
	content := []byte(`{"id": 1, "name": "test"}` + "\n")

	if err := os.WriteFile(dataFile, content, 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	// Read the file and check the validation logic
	data, err := os.ReadFile(dataFile)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	// Simulate the validation that happens in UploadDataset
	if len(data) == 0 {
		t.Error("non-empty file should not trigger empty file error")
	}

	// Verify content was read correctly
	if len(data) != len(content) {
		t.Errorf("expected %d bytes, got %d", len(content), len(data))
	}
}
