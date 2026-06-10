package producer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/helix-tools/sdk-go/v2/types"
)

// newTestProducer builds a *Producer wired to the given API endpoint without
// calling NewProducer (which hits STS + SSM). Same-package access to
// unexported fields is intentional, mirroring consumer's newTestConsumer.
func newTestProducer(endpoint string) *Producer {
	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
	}
	return &Producer{
		APIEndpoint: endpoint,
		BucketName:  "dme-producer-test",
		CustomerID:  "test-producer",
		Region:      "us-east-1",
		awsConfig:   awsCfg,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

func strptr(s string) *string { return &s }

// TestUpdateDataset_Parity drives the canonical public UpdateDataset method
// end-to-end. It pins:
//   - the method is PUBLIC and takes (ctx, datasetID, types.DatasetUpdateInput),
//   - it issues PATCH /v1/datasets/{id},
//   - the body carries only the non-nil fields (omitempty), and
//   - the decoded *types.Dataset is returned.
func TestUpdateDataset_Parity(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"_id": "ds-123",
			"name": "Updated Name",
			"description": "Updated description",
			"producer_id": "test-producer",
			"visibility": "public",
			"status": "active",
			"version": "2.0.0"
		}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	updated, err := p.UpdateDataset(context.Background(), "ds-123", types.DatasetUpdateInput{
		Description: strptr("Updated description"),
		Visibility:  strptr("public"),
		Status:      strptr(types.DatasetStatusActive),
		Version:     strptr("2.0.0"),
		Tags:        []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("UpdateDataset: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/v1/datasets/ds-123" {
		t.Errorf("expected path /v1/datasets/ds-123, got %q", gotPath)
	}
	// omitempty: name was not set, so it must be absent from the body.
	if _, present := gotBody["name"]; present {
		t.Errorf("unset field 'name' should be omitted from PATCH body, got %+v", gotBody)
	}
	if gotBody["visibility"] != "public" {
		t.Errorf("expected visibility=public in body, got %v", gotBody["visibility"])
	}
	if gotBody["status"] != "active" {
		t.Errorf("expected status=active in body, got %v", gotBody["status"])
	}
	if updated == nil || updated.ID != "ds-123" || updated.Version != "2.0.0" {
		t.Fatalf("unexpected returned dataset: %+v", updated)
	}
}

// TestUpdateDataset_ErrorPropagates verifies a non-2xx response surfaces as
// an *APIError with the status code preserved (bad-path coverage).
func TestUpdateDataset_ErrorPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("not the dataset owner"))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	_, err := p.UpdateDataset(context.Background(), "ds-999", types.DatasetUpdateInput{
		Description: strptr("x"),
	})
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
	if apiErr, ok := err.(*APIError); ok {
		if apiErr.StatusCode != http.StatusForbidden {
			t.Errorf("expected status 403, got %d", apiErr.StatusCode)
		}
	} else {
		t.Errorf("expected *APIError, got %T", err)
	}
}
