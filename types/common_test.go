package types

import (
	"encoding/json"
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
		"created_by": "thalesfsp",
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
		"created_by": "thalesfsp"
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
