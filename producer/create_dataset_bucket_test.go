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

// TestCreateDatasetRecord_IncludesS3BucketName drives the real createDatasetRecord
// (the POST-first path UploadDataset uses) and pins that the POST /v1/datasets
// body carries s3_bucket_name == the producer's BucketName.
//
// The Go API's dataset-create validator REJECTS an empty s3_bucket_name; this
// path previously omitted the field, so Go UploadDataset 400'd against the
// deployed API while Python/TS succeeded. Regression guard for that cross-SDK
// create-payload drift (found 2026-07-05 by the SDK-only E2E suite).
func TestCreateDatasetRecord_IncludesS3BucketName(t *testing.T) {
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

	p := newTestProducer(server.URL) // BucketName = "dme-producer-test"

	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.ndjson")
	if err := os.WriteFile(dataFile, []byte(strings.Repeat(`{"id":1,"name":"x"}`+"\n", 20)), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	opts := NewUploadOptions("e2e-bucket-test")
	opts.Category = "general"
	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/datasets" {
		t.Fatalf("expected POST /v1/datasets, got %s %s", gotMethod, gotPath)
	}
	got, ok := gotBody["s3_bucket_name"]
	if !ok {
		t.Fatalf("create-dataset body is MISSING s3_bucket_name; keys=%v", keysOf(gotBody))
	}
	if got != "dme-producer-test" {
		t.Fatalf("expected s3_bucket_name=%q, got %q", "dme-producer-test", got)
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

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
