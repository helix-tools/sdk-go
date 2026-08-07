package producer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// Per-consumer approval pricing (per-consumer-pricing program): a producer
// can approve a subscription request at a price that overrides the
// dataset's own marketplace price for that one consumer.
//   - nil (absent opts.PriceMonthlyCents): dataset's own price applies.
//   - pointer to 0: free grant, provisioned immediately even on a paid
//     dataset.
//   - pointer to >0: approved_pending_payment, even on a free dataset.
//   - negative: rejected client-side, never reaches the wire.
//
// These tests drive the REAL Producer.ApproveSubscriptionRequest method
// against an httptest server (real SigV4 signing + real error mapping),
// asserting on the ACTUAL marshalled request body — not a reimplementation
// of the method's internal map-building logic.

func pricePtr(v int64) *int64 { return &v }

// TestApproveSubscriptionRequest_PriceZeroIsSent pins the single most
// important case: a pointer to 0 MUST be serialized onto the wire as
// price_monthly_cents:0 (free grant), not omitted. omitempty on a pointer
// only omits nil; conflating "nil" with "points to zero" is the exact bug
// this feature exists to avoid.
func TestApproveSubscriptionRequest_PriceZeroIsSent(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"req-1","status":"approved","price_monthly_cents":0}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	result, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{
		PriceMonthlyCents: pricePtr(0),
	})
	if err != nil {
		t.Fatalf("ApproveSubscriptionRequest: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/subscription-requests/req-1" {
		t.Errorf("path = %q, want /v1/subscription-requests/req-1", gotPath)
	}

	v, present := gotBody["price_monthly_cents"]
	if !present {
		t.Fatalf("body = %v, want price_monthly_cents present (0 must be sent, not omitted)", gotBody)
	}
	if v != float64(0) {
		t.Errorf("body price_monthly_cents = %v, want 0", v)
	}
	if gotBody["action"] != "approve" {
		t.Errorf("body action = %v, want approve", gotBody["action"])
	}

	if result.PriceMonthlyCents == nil || *result.PriceMonthlyCents != 0 {
		t.Errorf("decoded PriceMonthlyCents = %v, want pointer to 0", result.PriceMonthlyCents)
	}
}

// TestApproveSubscriptionRequest_PriceNilOmitted pins that a nil opts (and,
// separately, opts with PriceMonthlyCents left nil) leaves the key ABSENT
// from the wire body — the dataset's own marketplace price applies.
func TestApproveSubscriptionRequest_PriceNilOmitted(t *testing.T) {
	t.Run("nil opts", func(t *testing.T) {
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"_id":"req-1","status":"approved"}`))
		}))
		defer server.Close()

		p := newTestProducer(server.URL)
		if _, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", nil); err != nil {
			t.Fatalf("ApproveSubscriptionRequest: %v", err)
		}
		if _, present := gotBody["price_monthly_cents"]; present {
			t.Errorf("body = %v, want no price_monthly_cents key", gotBody)
		}
		if len(gotBody) != 1 || gotBody["action"] != "approve" {
			t.Errorf("body = %v, want exactly {action: approve}", gotBody)
		}
	})

	t.Run("opts set but price nil", func(t *testing.T) {
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"_id":"req-1","status":"approved"}`))
		}))
		defer server.Close()

		p := newTestProducer(server.URL)
		notes := "approved, no price override"
		if _, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{
			Notes: &notes,
		}); err != nil {
			t.Fatalf("ApproveSubscriptionRequest: %v", err)
		}
		if _, present := gotBody["price_monthly_cents"]; present {
			t.Errorf("body = %v, want no price_monthly_cents key", gotBody)
		}
		if gotBody["notes"] != notes {
			t.Errorf("body notes = %v, want %q", gotBody["notes"], notes)
		}
	})
}

// TestApproveSubscriptionRequest_PricePositiveSent pins that a positive
// price is sent as-is, alongside notes/dataset_id, and that the response's
// approved_pending_payment status round-trips.
func TestApproveSubscriptionRequest_PricePositiveSent(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"req-1","status":"approved_pending_payment","price_monthly_cents":1000}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	notes := "comping a discount, not free"
	datasetID := "dataset-1"
	result, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{
		Notes:             &notes,
		DatasetID:         &datasetID,
		PriceMonthlyCents: pricePtr(1000),
	})
	if err != nil {
		t.Fatalf("ApproveSubscriptionRequest: %v", err)
	}

	if gotBody["price_monthly_cents"] != float64(1000) {
		t.Errorf("body price_monthly_cents = %v, want 1000", gotBody["price_monthly_cents"])
	}
	if gotBody["notes"] != notes {
		t.Errorf("body notes = %v, want %q", gotBody["notes"], notes)
	}
	if gotBody["dataset_id"] != datasetID {
		t.Errorf("body dataset_id = %v, want %q", gotBody["dataset_id"], datasetID)
	}
	if result.Status != "approved_pending_payment" {
		t.Errorf("status = %q, want approved_pending_payment", result.Status)
	}
	if result.PriceMonthlyCents == nil || *result.PriceMonthlyCents != 1000 {
		t.Errorf("decoded PriceMonthlyCents = %v, want pointer to 1000", result.PriceMonthlyCents)
	}
}

// TestApproveSubscriptionRequest_NegativePriceRejectedClientSide pins that a
// negative price fails fast as a *ValidationError and never reaches the
// transport — the server also rejects it (400), but the SDK should not
// waste a round trip on an obviously-invalid value.
func TestApproveSubscriptionRequest_NegativePriceRejectedClientSide(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	result, err := p.ApproveSubscriptionRequest(context.Background(), "req-1", &types.ApproveSubscriptionRequestOptions{
		PriceMonthlyCents: pricePtr(-1),
	})
	if err == nil {
		t.Fatal("expected validation error for negative price, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result on validation failure, got %+v", result)
	}

	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if vErr.Field != "price_monthly_cents" {
		t.Errorf("expected field price_monthly_cents, got %q", vErr.Field)
	}
	if requests != 0 {
		t.Errorf("negative price must not hit the API, got %d request(s)", requests)
	}
}
