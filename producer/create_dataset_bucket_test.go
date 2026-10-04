package producer

import (
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

// TestCreateDatasetRecord_OmitsS3BucketNameByDefault drives the real
// createDatasetRecord (the path UploadDataset uses) and pins the POST
// /v1/datasets body.
//
// The platform owns the upload destination: the API resolves it server-side,
// so the SDK no longer sends s3_bucket_name (nor its legacy alias s3_bucket),
// matching Python/TS. The test producer has a non-empty BucketName on purpose:
// even a caller who set the deprecated field by hand must not have it sent.
func TestCreateDatasetRecord_OmitsS3BucketNameByDefault(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL) // BucketName = "example-bucket-9"

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.ndjson")
	if err := os.WriteFile(dataFile, []byte(strings.Repeat(`{"id":1,"name":"x"}`+"\n", 20)), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	opts := testUploadOptions("e2e-bucket-test")
	opts.Category = "general"
	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, fakeProcessedFileData()); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/datasets" {
		t.Fatalf("expected POST /v1/datasets, got %s %s", gotMethod, gotPath)
	}
	for _, key := range []string{"s3_bucket_name", "s3_bucket"} {
		if got, present := gotBody[key]; present {
			t.Fatalf("create-dataset body carries %s=%v; the platform owns the destination and the SDK must not send it", key, got)
		}
	}

	// access_tier is likewise required by the create validator (free/premium/enterprise).
	tier, ok := gotBody["access_tier"]
	if !ok {
		t.Fatalf("create-dataset body is MISSING access_tier; keys=%v", keysOf(gotBody))
	}
	if tier != "free" {
		t.Fatalf("expected access_tier=%q, got %q", "free", tier)
	}

	// s3_key MUST be dataset-NAME-keyed (datasets/{name}/data.ndjson.gz), matching
	// Python/TS. A producer-id-keyed default breaks the notify pipeline (the
	// dispatcher derives dataset_name from the key's first segment).
	key, ok := gotBody["s3_key"]
	if !ok {
		t.Fatalf("create-dataset body is MISSING s3_key; keys=%v", keysOf(gotBody))
	}
	if key != "datasets/e2e-bucket-test/data.ndjson.gz" {
		t.Fatalf("expected s3_key=%q, got %q", "datasets/e2e-bucket-test/data.ndjson.gz", key)
	}

	// metadata MUST record encryption/compression so the consumer download reverses
	// them (Consumer.DownloadDataset reads these); else the round-trip sha mismatches.
	md, ok := gotBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("create-dataset body metadata is not an object: %T", gotBody["metadata"])
	}
	if md["encryption_enabled"] != true {
		t.Fatalf("expected metadata.encryption_enabled=true, got %v", md["encryption_enabled"])
	}
	if md["compression_enabled"] != true {
		t.Fatalf("expected metadata.compression_enabled=true, got %v", md["compression_enabled"])
	}
}

// TestCreateDatasetRecord_S3BucketNameOverridePassesThrough is the edge case:
// a caller who explicitly passes s3_bucket_name in DatasetOverrides still has
// it sent untouched — the API, not the SDK, decides whether it is acceptable.
func TestCreateDatasetRecord_S3BucketNameOverridePassesThrough(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	opts := testUploadOptions("bucket-override-test")
	opts.DatasetOverrides = map[string]any{"s3_bucket_name": "caller-chosen-bucket"}
	if _, err := p.createDatasetRecord(context.Background(), writeNDJSON(t, 3), opts, fakeProcessedFileData()); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	if gotBody["s3_bucket_name"] != "caller-chosen-bucket" {
		t.Fatalf("s3_bucket_name = %v, want the caller's explicit override %q", gotBody["s3_bucket_name"], "caller-chosen-bucket")
	}
	if _, present := gotBody["s3_bucket"]; present {
		t.Fatalf("create-dataset body carries s3_bucket=%v; only the caller's own override key may be sent", gotBody["s3_bucket"])
	}
}

// TestCreateDatasetRecord_IncludesVisibility drives the real createDatasetRecord
// and pins that the POST /v1/datasets body carries visibility == "private" by
// default. Python and TS both send "visibility": "private" explicitly on
// create; Go previously sent no visibility key at all, relying on the
// server's own empty-visibility default (datasets/service.go CreateDataset)
// happening to also be "private". That worked today but was a latent
// cross-SDK payload divergence — dead-code audit found alongside the
// per-consumer-pricing work (buildDatasetPayload, the only other place that
// set visibility, had zero callers on the live upload path and was removed).
func TestCreateDatasetRecord_IncludesVisibility(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.ndjson")
	if err := os.WriteFile(dataFile, []byte(strings.Repeat(`{"id":1,"name":"x"}`+"\n", 20)), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	opts := testUploadOptions("e2e-visibility-test")
	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, fakeProcessedFileData()); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	got, ok := gotBody["visibility"]
	if !ok {
		t.Fatalf("create-dataset body is MISSING visibility; keys=%v", keysOf(gotBody))
	}
	if got != "private" {
		t.Fatalf("expected visibility=%q, got %q", "private", got)
	}
}

// TestCreateDatasetRecord_VisibilityOverridable is the edge case: a producer
// who explicitly wants a public dataset can still override visibility via
// DatasetOverrides — the explicit default above must not become a hardcoded
// wall.
func TestCreateDatasetRecord_VisibilityOverridable(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.ndjson")
	if err := os.WriteFile(dataFile, []byte(strings.Repeat(`{"id":1,"name":"x"}`+"\n", 20)), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	opts := testUploadOptions("e2e-visibility-override-test")
	opts.DatasetOverrides = map[string]any{"visibility": "public"}
	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, fakeProcessedFileData()); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	if gotBody["visibility"] != "public" {
		t.Fatalf("expected overridden visibility=%q, got %q", "public", gotBody["visibility"])
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// fakeProcessedFileData stands in for a real processFile() result in tests
// that drive createDatasetRecord directly without paying for real
// compress+KMS-encrypt work.
func fakeProcessedFileData() *ProcessedFileData {
	return &ProcessedFileData{
		OriginalSize: 100,
		Sizes: map[string]any{
			"original_size_bytes":   int64(100),
			"compressed_size_bytes": int64(40),
			"encrypted_size_bytes":  int64(56),
			"encryption_enabled":    true,
			"compression_enabled":   true,
		},
	}
}
