package producer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// approveEnvelopeBody is the REAL approve wire shape (parity audit A-01): both
// POST /v1/subscription-requests/:id (action=approve) and
// POST /v1/subscription-requests/:id/approve return
// {"request": <SubscriptionRequest>, "subscription": <Subscription|null>}.
// A flat SubscriptionRequest body is what the old mocks planted, so a decoder
// that ignored the envelope still went green.
const approveEnvelopeBody = `{
	"request": {
		"_id": "req-1", "request_id": "req-1", "consumer_id": "cons-1", "producer_id": "prod-1",
		"tier": "free", "status": "approved", "subscription_id": "sub-9",
		"created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-09-01T00:00:01Z"
	},
	"subscription": {
		"_id": "sub-9", "consumer_id": "cons-1", "producer_id": "prod-1", "dataset_id": null,
		"tier": "free", "status": "active",
		"created_at": "2026-09-01T00:00:01Z", "updated_at": "2026-09-01T00:00:01Z"
	}
}`

func approveServer(t *testing.T, body string, gotBody *map[string]any, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		if gotBody != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, gotBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// The compat method keeps its (*types.SubscriptionRequest, error) signature
// and must return the envelope's request — Status and ID non-empty.
func TestApproveSubscriptionRequest_ReturnsEnvelopeRequest(t *testing.T) {
	var path string
	server := approveServer(t, approveEnvelopeBody, nil, &path)
	defer server.Close()

	p := newTestProducer(server.URL)

	// Compile-time pin of the public signature (Ringboost-safe: no change).
	var _ func(context.Context, string, *types.ApproveSubscriptionRequestOptions) (*types.SubscriptionRequest, error) = p.ApproveSubscriptionRequest

	got, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", nil)
	if err != nil {
		t.Fatalf("ApproveSubscriptionRequest: %v", err)
	}
	if got.Status != "approved" {
		t.Errorf("Status = %q, want approved (decoded from envelope.request)", got.Status)
	}
	if got.ID != "req-1" {
		t.Errorf("ID = %q, want req-1", got.ID)
	}
	if got.SubscriptionID == nil || *got.SubscriptionID != "sub-9" {
		t.Errorf("SubscriptionID = %v, want pointer to sub-9", got.SubscriptionID)
	}
	if path != "/v1/subscription-requests/req-1" {
		t.Errorf("path = %q, want /v1/subscription-requests/req-1", path)
	}
}

func TestApproveSubscriptionRequestWithSubscription_ReturnsBothHalves(t *testing.T) {
	server := approveServer(t, approveEnvelopeBody, nil, nil)
	defer server.Close()

	p := newTestProducer(server.URL)

	got, err := p.ApproveSubscriptionRequestWithSubscription(context.Background(), "req-1", nil)
	if err != nil {
		t.Fatalf("ApproveSubscriptionRequestWithSubscription: %v", err)
	}
	if got.Request.Status != "approved" || got.Request.ID != "req-1" {
		t.Errorf("Request = %+v, want approved/req-1", got.Request)
	}
	if got.Subscription == nil {
		t.Fatal("Subscription = nil, want the provisioned subscription")
	}
	if got.Subscription.ID != "sub-9" || got.Subscription.Status != "active" {
		t.Errorf("Subscription = %+v, want sub-9/active", got.Subscription)
	}
}

// approved_pending_payment provisions nothing yet: the API sends
// "subscription": null and the envelope must decode to a nil Subscription.
func TestApproveSubscriptionRequestWithSubscription_PendingPaymentHasNilSubscription(t *testing.T) {
	body := `{"request":{"_id":"req-2","status":"approved_pending_payment","price_monthly_cents":1000},"subscription":null}`
	server := approveServer(t, body, nil, nil)
	defer server.Close()

	p := newTestProducer(server.URL)

	got, err := p.ApproveSubscriptionRequestWithSubscription(context.Background(), "req-2", &types.ApproveSubscriptionRequestOptions{PriceMonthlyCents: pricePtr(1000)})
	if err != nil {
		t.Fatalf("ApproveSubscriptionRequestWithSubscription: %v", err)
	}
	if got.Request.Status != "approved_pending_payment" {
		t.Errorf("Request.Status = %q, want approved_pending_payment", got.Request.Status)
	}
	if got.Subscription != nil {
		t.Errorf("Subscription = %+v, want nil for subscription:null", got.Subscription)
	}

	// The compat method returns the request half here too.
	compat, err := p.ApproveSubscriptionRequest(context.Background(), "req-2", nil)
	if err != nil || compat.Status != "approved_pending_payment" {
		t.Errorf("ApproveSubscriptionRequest = %+v, %v; want approved_pending_payment", compat, err)
	}
}

// Bypass: a body that is not the envelope (flat request, or an empty object)
// must surface an error, never a zero-valued "success" that looks approved.
func TestApproveSubscriptionRequest_RejectsBodyWithoutRequest(t *testing.T) {
	for name, body := range map[string]string{
		"flat request body": `{"_id":"req-1","status":"approved"}`,
		"empty object":      `{}`,
		"null request":      `{"request":null,"subscription":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := approveServer(t, body, nil, nil)
			defer server.Close()
			p := newTestProducer(server.URL)

			got, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", nil)
			if err == nil {
				t.Fatalf("expected an error for %s, got %+v", name, got)
			}
			if got != nil {
				t.Errorf("expected nil result with the error, got %+v", got)
			}
			if !strings.Contains(err.Error(), "request") {
				t.Errorf("error %q should say the approve response had no request", err)
			}

			gotBoth, err := p.ApproveSubscriptionRequestWithSubscription(context.Background(), "req-1", nil)
			if err == nil || gotBoth != nil {
				t.Errorf("WithSubscription = %+v, %v; want nil + error", gotBoth, err)
			}
		})
	}
}

// D3: the dataset_id approve option is a no-op server-side (the API has no such
// field). It is deprecated: passing it warns once and is no longer sent.
func TestApproveSubscriptionRequest_DatasetIDIsDeprecatedAndNotSent(t *testing.T) {
	var buf bytes.Buffer
	prevOut := deprecationWriter
	deprecationWriter = &buf
	warnedApproveDatasetID.Store(false)
	defer func() { deprecationWriter = prevOut }()

	var body map[string]any
	server := approveServer(t, approveEnvelopeBody, &body, nil)
	defer server.Close()
	p := newTestProducer(server.URL)

	datasetID := "ds-1"
	if _, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{DatasetID: &datasetID}); err != nil {
		t.Fatalf("ApproveSubscriptionRequest: %v", err)
	}
	if _, present := body["dataset_id"]; present {
		t.Errorf("body = %v; dataset_id must not be sent (the API ignores it)", body)
	}
	out := buf.String()
	if !strings.Contains(out, "DatasetID") || !strings.Contains(strings.ToLower(out), "deprecated") {
		t.Errorf("expected a deprecation warning naming DatasetID, got %q", out)
	}

	// Second call in the same process: warns once, not per call.
	buf.Reset()
	if _, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{DatasetID: &datasetID}); err != nil {
		t.Fatalf("second ApproveSubscriptionRequest: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("deprecation warning repeated on the second call: %q", buf.String())
	}
}

// No warning when the deprecated option is not passed.
func TestApproveSubscriptionRequest_NoWarningWithoutDatasetID(t *testing.T) {
	var buf bytes.Buffer
	prevOut := deprecationWriter
	deprecationWriter = &buf
	warnedApproveDatasetID.Store(false)
	defer func() { deprecationWriter = prevOut }()

	server := approveServer(t, approveEnvelopeBody, nil, nil)
	defer server.Close()
	p := newTestProducer(server.URL)

	notes := "ok"
	if _, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{Notes: &notes}); err != nil {
		t.Fatalf("ApproveSubscriptionRequest: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected warning: %q", buf.String())
	}
}
