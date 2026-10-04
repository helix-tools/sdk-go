package producer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// testDescription is a description the API accepts (at least 10 characters).
const testDescription = "Upload suite fixture dataset"

// testUploadOptions is NewUploadOptions plus the Description every upload
// needs. The zero-value UploadOptions is not a valid upload on its own.
func testUploadOptions(datasetName string) UploadOptions {
	opts := NewUploadOptions(datasetName)
	opts.Description = testDescription

	return opts
}

// requireDescriptionRefusal asserts err is the local description refusal.
func requireDescriptionRefusal(t *testing.T, err error) {
	t.Helper()

	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "description" {
		t.Fatalf("error = %v, want a *ValidationError on description", err)
	}
}

// TestUploadDataset_DescriptionMinimum: the API refuses a description shorter
// than 10 characters once surrounding spaces are trimmed. UploadDataset must
// refuse the same inputs locally, before any request is sent.
func TestUploadDataset_DescriptionMinimum(t *testing.T) {
	cases := []struct {
		name        string
		description string
		wantErr     bool
	}{
		{"empty", "", true},
		{"nine characters", "123456789", true},
		{"ten characters", "1234567890", false},
		{"ten spaces (trims to empty)", "          ", true},
		{"short text padded past ten bytes", "   short   ", true},
		{"ten characters padded with spaces", "  0123456789  ", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUploadFixture(t)
			opts := testUploadOptions("description-minimum")
			opts.Description = tc.description

			dataFile := writeNDJSON(t, 3)

			_, err := f.p.UploadDataset(context.Background(), dataFile, opts)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("UploadDataset: %v", err)
				}

				return
			}

			requireDescriptionRefusal(t, err)

			if len(f.order) != 0 {
				t.Fatalf("requests reached the API before the refusal: %v; want none", f.order)
			}
		})
	}
}

// TestUploadDataset_DescriptionRefusedBeforeReadingFile: the refusal runs
// before the file is opened, so a missing file reports the description.
func TestUploadDataset_DescriptionRefusedBeforeReadingFile(t *testing.T) {
	f := newUploadFixture(t)
	opts := testUploadOptions("description-first")
	opts.Description = "short"

	missing := filepath.Join(t.TempDir(), "missing.ndjson")

	_, err := f.p.UploadDataset(context.Background(), missing, opts)
	requireDescriptionRefusal(t, err)
}

// TestUploadDataset_DescriptionOverrideIsTheSentValue: DatasetOverrides
// replaces the description that is sent, so a short Description with a valid
// override is not refused locally; the override is what the API checks.
func TestUploadDataset_DescriptionOverrideIsTheSentValue(t *testing.T) {
	f := newUploadFixture(t)
	opts := testUploadOptions("description-override")
	opts.Description = "short"
	opts.DatasetOverrides = map[string]any{"description": "Long enough from the override"}

	if _, err := f.p.UploadDataset(context.Background(), writeNDJSON(t, 3), opts); err != nil {
		t.Fatalf("UploadDataset: %v", err)
	}

	if got := f.postBody["description"]; got != "Long enough from the override" {
		t.Fatalf("POST body description = %v, want the override", got)
	}
}
