package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestDataset_StorageUsageFieldsRoundTrip pins the wire field names of the
// Dataset storage-usage stats to dataset.schema.json (sdk-schemas): without a
// matching struct field, json.Unmarshal silently drops unknown keys — this
// test proves each field actually survives a decode, not just that the type
// compiles.
func TestDataset_StorageUsageFieldsRoundTrip(t *testing.T) {
	raw := `{
		"_id": "ds-1",
		"name": "test dataset",
		"producer_id": "prod-1",
		"category": "finance",
		"data_freshness": "daily",
		"visibility": "public",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"created_by": "user-1",
		"file_formats": ["csv", "parquet"],
		"total_files": 42,
		"total_size_bytes": 104857600,
		"original_size": 209715200,
		"compressed_size": 104857600,
		"access_count": 7,
		"last_updated_data": "2026-08-01T12:00:00Z"
	}`

	var ds Dataset
	if err := json.Unmarshal([]byte(raw), &ds); err != nil {
		t.Fatalf("unmarshal Dataset: %v", err)
	}

	if got, want := ds.FileFormats, []string{"csv", "parquet"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("FileFormats = %v, want %v", got, want)
	}
	if ds.TotalFiles != 42 {
		t.Errorf("TotalFiles = %d, want 42", ds.TotalFiles)
	}
	if ds.TotalSizeBytes != 104857600 {
		t.Errorf("TotalSizeBytes = %d, want 104857600", ds.TotalSizeBytes)
	}
	if ds.OriginalSize != 209715200 {
		t.Errorf("OriginalSize = %d, want 209715200", ds.OriginalSize)
	}
	if ds.CompressedSize != 104857600 {
		t.Errorf("CompressedSize = %d, want 104857600", ds.CompressedSize)
	}
	if ds.AccessCount != 7 {
		t.Errorf("AccessCount = %d, want 7", ds.AccessCount)
	}
	if ds.LastUpdatedData == nil || *ds.LastUpdatedData != "2026-08-01T12:00:00Z" {
		t.Errorf("LastUpdatedData = %v, want 2026-08-01T12:00:00Z", ds.LastUpdatedData)
	}
}

// TestDataset_LastUpdatedDataAbsentDecodesToNil proves the nullable field
// distinguishes "never had a file upload/delete" (nil) from a real
// zero-value timestamp — the schema types last_updated_data as
// ["string","null"].
func TestDataset_LastUpdatedDataAbsentDecodesToNil(t *testing.T) {
	raw := `{
		"_id": "ds-2",
		"name": "no stats yet",
		"producer_id": "prod-1",
		"category": "finance",
		"data_freshness": "daily",
		"visibility": "public",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"created_by": "user-1"
	}`

	var ds Dataset
	if err := json.Unmarshal([]byte(raw), &ds); err != nil {
		t.Fatalf("unmarshal Dataset: %v", err)
	}
	if ds.LastUpdatedData != nil {
		t.Errorf("LastUpdatedData = %v, want nil (absent)", *ds.LastUpdatedData)
	}
	if ds.TotalFiles != 0 || ds.AccessCount != 0 {
		t.Errorf("expected zero-value storage stats when absent, got TotalFiles=%d AccessCount=%d", ds.TotalFiles, ds.AccessCount)
	}
}

// TestConfig_String_RedactsSecrets is requirement 5 of the API-key feature
// (design §4.11): APIKey must never appear in a %v/%+v of a Config — an
// incidental debug print, or an error wrapped with %w. AWSSecretAccessKey
// is redacted on the same basis (both are bootstrap secrets); CustomerID,
// Region and APIEndpoint are deliberately left visible.
func TestConfig_String_RedactsSecrets(t *testing.T) {
	cfg := Config{
		APIEndpoint:        "https://api-go.helix.tools",
		AWSAccessKeyID:     "AKIA-VISIBLE-NOT-SECRET",
		AWSSecretAccessKey: "aStaticSecretThatMustNeverBePrinted12345",
		CustomerID:         "customer-redaction-test",
		Region:             "us-east-1",
		APIKey:             "hlx_oHBvRPOIvGrv5iFlbCBFNOgmBjMtpsiaOclRz3AwzKs",
		CredentialMode:     CredentialModeSTS,
	}

	out := fmt.Sprintf("%v", cfg)
	if strings.Contains(out, cfg.APIKey) {
		t.Fatalf("String() = %q, leaked the raw API key", out)
	}
	if strings.Contains(out, cfg.AWSSecretAccessKey) {
		t.Fatalf("String() = %q, leaked the raw AWS secret access key", out)
	}
	if n := strings.Count(out, "<redacted>"); n != 2 {
		t.Errorf("String() = %q, want exactly 2 redacted placeholders, got %d", out, n)
	}
	// Negative control: non-secret fields must still be visible — proves
	// String() targets specific fields rather than redacting everything.
	for _, want := range []string{cfg.APIEndpoint, cfg.AWSAccessKeyID, cfg.CustomerID, cfg.Region, string(cfg.CredentialMode)} {
		if !strings.Contains(out, want) {
			t.Errorf("String() = %q, want it to still contain non-secret value %q", out, want)
		}
	}

	outPlus := fmt.Sprintf("%+v", cfg)
	if strings.Contains(outPlus, cfg.APIKey) || strings.Contains(outPlus, cfg.AWSSecretAccessKey) {
		t.Fatalf("%%+v = %q, leaked a secret", outPlus)
	}

	// Zero-value Config: both secret fields absent must redact to nothing
	// (empty string, not a stray "<redacted>"), confirming the guard is on
	// presence, not unconditional.
	emptyOut := fmt.Sprintf("%v", Config{})
	if strings.Contains(emptyOut, "<redacted>") {
		t.Errorf("String() on a zero-value Config = %q, want no redaction placeholder when there is nothing to redact", emptyOut)
	}
}
