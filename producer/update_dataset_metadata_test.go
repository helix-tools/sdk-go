package producer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestUpdateDataset_RefusesMetadataBeforeAnyRequest: the update endpoint does
// not store metadata, so setting it used to be dropped without a word. Now
// UpdateDataset refuses it as a *ValidationError before any request, even when
// other fields are also set, so a partial update cannot slip through silently.
func TestUpdateDataset_RefusesMetadataBeforeAnyRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ds-1"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	cases := map[string]types.DatasetUpdateInput{
		"metadata alone":              {Metadata: map[string]any{"source": "x"}},
		"metadata with a real change": {Tags: []string{"a"}, Metadata: map[string]any{"source": "x"}},
		"metadata set but empty":      {Metadata: map[string]any{}},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := p.UpdateDataset(context.Background(), "ds-1", input)

			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("err = %v (%T), want a *ValidationError", err, err)
			}
			if vErr.Field != "metadata" {
				t.Errorf("ValidationError.Field = %q, want \"metadata\"", vErr.Field)
			}
		})
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("%d request(s) reached the API for a metadata update; want 0", got)
	}
}

// TestUpdateDataset_WithoutMetadataStillUpdates is the positive control: a
// normal update with Metadata left nil still goes out and succeeds.
func TestUpdateDataset_WithoutMetadataStillUpdates(t *testing.T) {
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ds-1"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.UpdateDataset(context.Background(), "ds-1", types.DatasetUpdateInput{Tags: []string{"a"}}); err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}
	if _, present := sent["metadata"]; present {
		t.Errorf("PATCH body carries metadata: %v", sent["metadata"])
	}
}
