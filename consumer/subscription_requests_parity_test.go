package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

const twoRequestsBody = `{
	"requests": [
		{
			"_id": "id-1",
			"request_id": "req-1",
			"consumer_id": "consumer-1",
			"producer_id": "producer-1",
			"tier": "free",
			"status": "pending",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z"
		},
		{
			"_id": "id-2",
			"request_id": "req-2",
			"consumer_id": "consumer-1",
			"producer_id": "producer-2",
			"tier": "free",
			"status": "approved",
			"created_at": "2026-01-02T00:00:00Z",
			"updated_at": "2026-01-02T00:00:00Z"
		}
	],
	"count": 2
}`

// TestListSubscriptionRequests_Parity drives the canonical public method
// end-to-end against an httptest server. It pins:
//   - the method is PUBLIC under the canonical name (compile-time: it is
//     called here from outside the function body),
//   - it GETs /v1/subscription-requests with no status filter,
//   - it returns the unwrapped []SubscriptionRequest slice.
func TestListSubscriptionRequests_Parity(t *testing.T) {
	var gotPath string
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(twoRequestsBody))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	reqs, err := c.ListSubscriptionRequests(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/v1/subscription-requests" {
		t.Errorf("expected path /v1/subscription-requests, got %q", gotPath)
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	if reqs[0].RequestID != "req-1" || reqs[1].Status != types.SubscriptionRequestStatusApproved {
		t.Errorf("unexpected request payload: %+v", reqs)
	}
}

// TestListSubscriptionRequests_StatusFilter verifies the status query param
// is appended and URL-escaped.
func TestListSubscriptionRequests_StatusFilter(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("status")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"requests": [], "count": 0}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.ListSubscriptionRequests(context.Background(), types.SubscriptionRequestStatusPending); err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}
	if gotQuery != "pending" {
		t.Errorf("expected status=pending query, got %q", gotQuery)
	}
}

// TestListMySubscriptionRequests_BackwardCompat asserts the deprecated alias
// still works and delegates to the canonical method (same path, same result).
func TestListMySubscriptionRequests_BackwardCompat(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(twoRequestsBody))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	reqs, err := c.ListMySubscriptionRequests(context.Background(), "")
	if err != nil {
		t.Fatalf("ListMySubscriptionRequests (alias): %v", err)
	}
	if gotPath != "/v1/subscription-requests" {
		t.Errorf("alias hit wrong path %q", gotPath)
	}
	if len(reqs) != 2 {
		t.Fatalf("alias returned %d requests, want 2", len(reqs))
	}
}

// TestGetSubscriptionRequest_Parity drives the public single-request getter.
func TestGetSubscriptionRequest_Parity(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"_id": "id-1",
			"request_id": "req-abc123",
			"consumer_id": "consumer-1",
			"producer_id": "producer-1",
			"tier": "free",
			"status": "rejected",
			"rejection_reason": "out of scope",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z"
		}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	req, err := c.GetSubscriptionRequest(context.Background(), "req-abc123")
	if err != nil {
		t.Fatalf("GetSubscriptionRequest: %v", err)
	}
	if gotPath != "/v1/subscription-requests/req-abc123" {
		t.Errorf("expected path /v1/subscription-requests/req-abc123, got %q", gotPath)
	}
	if req == nil || req.RequestID != "req-abc123" {
		t.Fatalf("unexpected request: %+v", req)
	}
	if req.Status != types.SubscriptionRequestStatusRejected {
		t.Errorf("expected status rejected, got %q", req.Status)
	}
}
