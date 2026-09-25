package producer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestUpdateDataset_UsesIDFromIDOnlyListResponse is the README flow against
// the REAL wire shape (parity audit B-01): the dataset API sends "id" and never
// "_id". ListMyDatasets -> UpdateDataset(ctx, ds.ID, ...) must PATCH
// /v1/datasets/<id>, not /v1/datasets/ (which the API rejects).
func TestUpdateDataset_UsesIDFromIDOnlyListResponse(t *testing.T) {
	var mu sync.Mutex
	var patchPath, patchMethod string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/datasets":
			// Real DatasetResponse shape: "id", total_size_bytes, no _id/size_bytes/created_by.
			_, _ = w.Write([]byte(`{"datasets":[{"id":"ds-wire-7","name":"orders","producer_id":"test-producer","status":"active","total_size_bytes":2048}],"page":1,"total_pages":1}`))
		case r.Method == http.MethodPatch:
			mu.Lock()
			patchPath, patchMethod = r.URL.Path, r.Method
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"ds-wire-7","name":"orders","description":"updated","status":"active"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	datasets, err := p.ListMyDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}
	if len(datasets) != 1 {
		t.Fatalf("expected 1 dataset, got %d", len(datasets))
	}
	ds := datasets[0]
	if ds.ID != "ds-wire-7" {
		t.Fatalf("Dataset.ID = %q, want ds-wire-7 (id-only body)", ds.ID)
	}
	if ds.SizeBytes != 2048 {
		t.Errorf("Dataset.SizeBytes = %d, want 2048 (from total_size_bytes)", ds.SizeBytes)
	}

	updated, err := p.UpdateDataset(context.Background(), ds.ID, types.DatasetUpdateInput{Description: strptr("updated")})
	if err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if patchMethod != http.MethodPatch || patchPath != "/v1/datasets/ds-wire-7" {
		t.Errorf("PATCH went to %s %q, want PATCH /v1/datasets/ds-wire-7", patchMethod, patchPath)
	}
	if updated.ID != "ds-wire-7" {
		t.Errorf("updated.ID = %q, want ds-wire-7 (PATCH response is id-only too)", updated.ID)
	}
}
