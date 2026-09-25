package types

import (
	"encoding/json"
	"testing"
)

// The customer-facing dataset API (GET/PATCH/list) serialises through the
// API's DatasetResponse, which carries the identifier as "id" and NEVER as
// "_id" (parity audit B-01). These tests use that real wire shape.

func TestDataset_Decode_IDFromIDOnlyBody(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"id":"ds-wire-1","name":"n","producer_id":"p"}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.ID != "ds-wire-1" {
		t.Errorf("ID = %q, want %q (API sends only \"id\")", ds.ID, "ds-wire-1")
	}
	if ds.IDAlias != "ds-wire-1" {
		t.Errorf("IDAlias = %q, want %q (existing IDAlias callers keep working)", ds.IDAlias, "ds-wire-1")
	}
}

func TestDataset_Decode_IDFromLegacyUnderscoreID(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"_id":"ds-legacy","name":"n"}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.ID != "ds-legacy" {
		t.Errorf("ID = %q, want %q (stored-document shape still decodes)", ds.ID, "ds-legacy")
	}
}

func TestDataset_Decode_UnderscoreIDWinsWhenBothPresent(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"_id":"ds-stored","id":"ds-alias"}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.ID != "ds-stored" {
		t.Errorf("ID = %q, want %q (existing behaviour: _id is authoritative when both are sent)", ds.ID, "ds-stored")
	}
	if ds.IDAlias != "ds-alias" {
		t.Errorf("IDAlias = %q, want the raw id value %q", ds.IDAlias, "ds-alias")
	}
}

func TestDataset_Decode_NoIDLeavesIDEmpty(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"name":"n"}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.ID != "" || ds.IDAlias != "" {
		t.Errorf("ID/IDAlias = %q/%q, want both empty when the body carries neither key", ds.ID, ds.IDAlias)
	}
}

func TestDataset_Decode_IDInsideListEnvelope(t *testing.T) {
	var resp struct {
		Datasets []Dataset `json:"datasets"`
	}
	body := `{"datasets":[{"id":"a"},{"id":"b","total_size_bytes":42}]}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Datasets) != 2 || resp.Datasets[0].ID != "a" || resp.Datasets[1].ID != "b" {
		t.Fatalf("list decode lost ids: %+v", resp.Datasets)
	}
}

// The API has no size_bytes on the wire: the real size arrives as
// total_size_bytes (parity audit B-01), so SizeBytes used to decode to 0.
func TestDataset_Decode_SizeBytesFallsBackToTotalSizeBytes(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"id":"a","total_size_bytes":1234}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.SizeBytes != 1234 {
		t.Errorf("SizeBytes = %d, want 1234 (from total_size_bytes)", ds.SizeBytes)
	}
	if ds.TotalSizeBytes != 1234 {
		t.Errorf("TotalSizeBytes = %d, want 1234 (unchanged)", ds.TotalSizeBytes)
	}
}

func TestDataset_Decode_ExplicitSizeBytesWinsOverTotal(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"id":"a","size_bytes":10,"total_size_bytes":99}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.SizeBytes != 10 {
		t.Errorf("SizeBytes = %d, want 10 (an explicit size_bytes is authoritative)", ds.SizeBytes)
	}
}

// A malformed body must still surface the decoder's error rather than being
// swallowed by the custom UnmarshalJSON.
func TestDataset_Decode_MalformedBodyErrors(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"id": 5}`), &ds); err == nil {
		t.Fatal("expected a type error for a numeric id, got nil")
	}
}
