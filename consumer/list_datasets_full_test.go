package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// realDatasetPage is one page of the API's DatasetResponse shape (id, no _id,
// total_size_bytes) with the fields a Go caller needs from a listing
// (parity audit A-15/B-06): producer, category, description, status, sizes,
// marketplace and the full metadata object.
const realDatasetRow = `{
	"id": "ds-full-1",
	"name": "phone-feed",
	"description": "Daily phone numbers",
	"producer_id": "prod-42",
	"category": "telecom",
	"status": "active",
	"visibility": "public",
	"data_freshness": "daily",
	"encryption": true,
	"total_size_bytes": 4096,
	"record_count": 1200,
	"version": "1.2.0",
	"tags": ["a", "b"],
	"metadata": {"compression_enabled": true, "record_format": "ndjson"},
	"marketplace": {"listed": true, "price_monthly_cents": 4999, "currency": "usd"}
}`

func TestListDatasets_ReturnsEveryFieldOfTheListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"datasets":[` + realDatasetRow + `],"total_count":1,"page":1,"limit":100,"total_pages":1}`))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(datasets) != 1 {
		t.Fatalf("len = %d, want 1", len(datasets))
	}
	ds := datasets[0]

	if ds.ID != "ds-full-1" || ds.Name != "phone-feed" {
		t.Errorf("ID/Name = %q/%q, want ds-full-1/phone-feed", ds.ID, ds.Name)
	}
	if ds.ProducerID != "prod-42" || ds.Category != "telecom" || ds.Description != "Daily phone numbers" || ds.Status != "active" {
		t.Errorf("listing lost fields: producer=%q category=%q description=%q status=%q", ds.ProducerID, ds.Category, ds.Description, ds.Status)
	}
	if ds.SizeBytes != 4096 || ds.TotalSizeBytes != 4096 || ds.RecordCount != 1200 {
		t.Errorf("sizes = %d/%d/%d, want 4096/4096/1200", ds.SizeBytes, ds.TotalSizeBytes, ds.RecordCount)
	}
	if ds.Version != "1.2.0" || len(ds.Tags) != 2 {
		t.Errorf("version/tags = %q/%v", ds.Version, ds.Tags)
	}
	if ds.Marketplace == nil || ds.Marketplace.PriceMonthlyCents == nil || *ds.Marketplace.PriceMonthlyCents != 4999 {
		t.Errorf("marketplace = %+v, want price 4999", ds.Marketplace)
	}
	// The full metadata object survives on the embedded record.
	if got := ds.Dataset.Metadata["record_format"]; got != "ndjson" {
		t.Errorf("Dataset.Metadata[record_format] = %v, want ndjson", got)
	}
}

// Source compatibility: the pre-existing Dataset surface (ID, Name and the two
// Metadata flags as bools) keeps compiling and keeps its meaning.
func TestListDatasets_KeepsLegacyMetadataFlags(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(realDatasetRow), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var compress bool = ds.Metadata.CompressionEnabled
	var encrypt bool = ds.Metadata.EncryptionEnabled
	if !compress {
		t.Error("Metadata.CompressionEnabled = false, want true (metadata.compression_enabled)")
	}
	// metadata has no encryption_enabled: the create endpoint promotes it to the
	// top-level "encryption" flag, which must count.
	if !encrypt {
		t.Error("Metadata.EncryptionEnabled = false, want true (top-level encryption:true fallback)")
	}
}

func TestListDatasets_MetadataFlagsFromMetadataObject(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"id":"x","metadata":{"compression_enabled":false,"encryption_enabled":true}}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.Metadata.CompressionEnabled || !ds.Metadata.EncryptionEnabled {
		t.Errorf("flags = %+v, want compression=false encryption=true", ds.Metadata)
	}

	// Wrong-typed or absent flags are false, never a decode error.
	var odd Dataset
	if err := json.Unmarshal([]byte(`{"id":"y","metadata":{"compression_enabled":"yes"}}`), &odd); err != nil {
		t.Fatalf("non-bool flag must not fail the listing: %v", err)
	}
	if odd.Metadata.CompressionEnabled || odd.Metadata.EncryptionEnabled {
		t.Errorf("flags = %+v, want both false", odd.Metadata)
	}
	var none Dataset
	if err := json.Unmarshal([]byte(`{"id":"z"}`), &none); err != nil {
		t.Fatalf("unmarshal without metadata: %v", err)
	}
	if none.ID != "z" || none.Metadata.CompressionEnabled || none.Metadata.EncryptionEnabled {
		t.Errorf("dataset without metadata = %+v", none)
	}
}

// Acceptance Q4: every page is followed AND the rows of the LAST page carry the
// full record too (a decoder that fills only page one would go green on the
// old id/name assertions).
func TestListDatasets_FollowsPagesAndKeepsFullRecordOnEveryPage(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"datasets":[{"id":"p1","name":"one","producer_id":"prod-1"}],"page":1,"total_pages":2}`))
		case 2:
			_, _ = w.Write([]byte(`{"datasets":[{"id":"p2","name":"two","producer_id":"prod-2","category":"c2"}],"page":2,"total_pages":2}`))
		default:
			t.Errorf("unexpected request #%d: %s", n, r.URL.String())
		}
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(datasets) != 2 || datasets[1].ID != "p2" || datasets[1].ProducerID != "prod-2" || datasets[1].Category != "c2" {
		t.Fatalf("datasets = %+v, want both pages with full records", datasets)
	}
}
