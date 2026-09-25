package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
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
	if ds.Record == nil {
		t.Fatal("Record = nil; every listed row must carry the full catalog record")
	}
	rec := ds.Record
	if rec.ID != "ds-full-1" {
		t.Errorf("Record.ID = %q, want ds-full-1 (from the API's id key)", rec.ID)
	}
	if rec.ProducerID != "prod-42" || rec.Category != "telecom" || rec.Description != "Daily phone numbers" || rec.Status != "active" {
		t.Errorf("listing lost fields: producer=%q category=%q description=%q status=%q", rec.ProducerID, rec.Category, rec.Description, rec.Status)
	}
	if rec.SizeBytes != 4096 || rec.TotalSizeBytes != 4096 || rec.RecordCount != 1200 {
		t.Errorf("sizes = %d/%d/%d, want 4096/4096/1200", rec.SizeBytes, rec.TotalSizeBytes, rec.RecordCount)
	}
	if rec.Version != "1.2.0" || len(rec.Tags) != 2 {
		t.Errorf("version/tags = %q/%v", rec.Version, rec.Tags)
	}
	if rec.Marketplace == nil || rec.Marketplace.PriceMonthlyCents == nil || *rec.Marketplace.PriceMonthlyCents != 4999 {
		t.Errorf("marketplace = %+v, want price 4999", rec.Marketplace)
	}
	// The full metadata object survives on the record.
	if got := rec.Metadata["record_format"]; got != "ndjson" {
		t.Errorf("Record.Metadata[record_format] = %v, want ndjson", got)
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

// An explicit metadata flag beats the top-level one, exactly as
// DownloadDataset (resolveEncryptCompress) resolves it.
func TestListDatasets_ExplicitMetadataFlagBeatsTopLevelEncryption(t *testing.T) {
	var ds Dataset
	if err := json.Unmarshal([]byte(`{"id":"x","encryption":true,"metadata":{"encryption_enabled":false}}`), &ds); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ds.Metadata.EncryptionEnabled {
		t.Error("Metadata.EncryptionEnabled = true; metadata.encryption_enabled=false is explicit and must win")
	}
	enc, _ := resolveEncryptCompress(ds.Record)
	if enc != ds.Metadata.EncryptionEnabled {
		t.Errorf("list flag %v disagrees with the download decision %v", ds.Metadata.EncryptionEnabled, enc)
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
// full record too. The server answers by the REQUESTED page number (not by hit
// count), so a client that always asked for page 1 would not go green.
func TestListDatasets_FollowsPagesAndKeepsFullRecordOnEveryPage(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		requested = append(requested, page)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			_, _ = w.Write([]byte(`{"datasets":[{"id":"p1","name":"one","producer_id":"prod-1"}],"page":1,"total_pages":2}`))
		case "2":
			_, _ = w.Write([]byte(`{"datasets":[{"id":"p2","name":"two","producer_id":"prod-2","category":"c2"}],"page":2,"total_pages":2}`))
		default:
			t.Errorf("unexpected page %q: %s", page, r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(requested, []string{"1", "2"}) {
		t.Errorf("requested pages %v, want [1 2]", requested)
	}
	if len(datasets) != 2 || datasets[1].ID != "p2" || datasets[1].Record == nil ||
		datasets[1].Record.ProducerID != "prod-2" || datasets[1].Record.Category != "c2" {
		t.Fatalf("datasets = %+v, want both pages with full records", datasets)
	}
	if datasets[0].Record == nil || datasets[0].Record.ProducerID != "prod-1" {
		t.Errorf("first row lost its record: %+v", datasets[0])
	}
}

// Self-attack (c): GetDataset("") would GET /v1/datasets/ — the COLLECTION —
// and decode a list body into an empty Dataset without any error. Refuse it
// before a request is sent.
func TestGetDataset_RefusesAnEmptyDatasetID(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"datasets":[]}`))
	}))
	defer server.Close()

	for _, id := range []string{"", "  "} {
		ds, err := newTestConsumer(server.URL).GetDataset(context.Background(), id)
		if err == nil || ds != nil {
			t.Errorf("GetDataset(%q) = %+v, %v; want an error and no dataset", id, ds, err)
		}
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("%d request(s) reached the API; an empty id must be refused client-side", hits)
	}
}

// Source compatibility beyond selectors: callers build Dataset values with keyed
// literals (fakes behind their own interfaces) and compare them / use them as
// map keys. Both must keep working.
func TestDataset_KeyedLiteralAndComparability(t *testing.T) {
	a := Dataset{ID: "x", Name: "n"}
	b := Dataset{ID: "x", Name: "n"}
	if a != b {
		t.Error("two equal Dataset values compare unequal")
	}
	seen := map[Dataset]bool{a: true}
	if !seen[b] {
		t.Error("Dataset is no longer usable as a map key")
	}
	a.Metadata.CompressionEnabled = true
	if a == b {
		t.Error("Datasets differing in Metadata compare equal")
	}
}

// GetDownloadURL's legacy nested dataset object is tagged _id, but the API
// identifies datasets with id: the id-only shape must not lose the identifier.
func TestGetDownloadURL_NestedDatasetIDFromIDKey(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"id only":          {`{"download_url":"https://s3.example/x","dataset":{"id":"ds-1","name":"n"}}`, "ds-1"},
		"legacy _id":       {`{"download_url":"https://s3.example/x","dataset":{"_id":"ds-2","name":"n"}}`, "ds-2"},
		"_id wins over id": {`{"download_url":"https://s3.example/x","dataset":{"_id":"ds-3","id":"ds-x"}}`, "ds-3"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			info, err := newTestConsumer(server.URL).GetDownloadURL(context.Background(), "ds-1")
			if err != nil {
				t.Fatalf("GetDownloadURL: %v", err)
			}
			if info.DownloadURL != "https://s3.example/x" || info.Dataset == nil || info.Dataset.ID != tc.want {
				t.Errorf("info = %+v (dataset %+v), want nested dataset id %q", info, info.Dataset, tc.want)
			}
		})
	}

	// Without a nested dataset there is nothing to fix up.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"download_url":"https://s3.example/x"}`))
	}))
	defer server.Close()
	info, err := newTestConsumer(server.URL).GetDownloadURL(context.Background(), "ds-1")
	if err != nil || info.Dataset != nil {
		t.Errorf("info = %+v, err = %v; want no nested dataset", info, err)
	}

	// A malformed body is an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"download_url": 5}`))
	}))
	defer bad.Close()
	if _, err := newTestConsumer(bad.URL).GetDownloadURL(context.Background(), "ds-1"); err == nil {
		t.Error("expected a decode error for a numeric download_url")
	}
}
