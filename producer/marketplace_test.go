package producer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// Marketplace producer surface (PR6 / ClickUp 86e01nb3t):
//   - GetConnectOnboardingLink -> POST  /v1/self/connect/onboard    -> {url}
//   - GetConnectStatus         -> GET   /v1/self/connect/status
//   - GetEarnings              -> GET   /v1/self/marketplace/earnings
//   - SetDatasetMarketplace    -> PATCH /v1/datasets/:id/marketplace
//
// Drives the REAL Producer methods end-to-end against an httptest server (real
// SigV4 signing + real error mapping). Contract anchored to sdk-schemas PR #18
// (company connect_*, dataset.marketplace).

func mktPtr[T any](v T) *T { return &v }

func TestGetConnectOnboardingLink(t *testing.T) {
	var gotPath, gotMethod string
	var gotBodyLen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBodyLen = len(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://connect.stripe.com/setup/e/acct/x"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	url, err := p.GetConnectOnboardingLink(context.Background())
	if err != nil {
		t.Fatalf("GetConnectOnboardingLink: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/self/connect/onboard" {
		t.Errorf("path = %q, want /v1/self/connect/onboard", gotPath)
	}
	if gotBodyLen != 0 {
		t.Errorf("request body length = %d, want 0 (no body)", gotBodyLen)
	}
	if url != "https://connect.stripe.com/setup/e/acct/x" {
		t.Errorf("url = %q", url)
	}
}

func TestGetConnectStatus_HappyPath(t *testing.T) {
	var gotPath, gotMethod string
	body := `{"connect_account_id":"acct_123","connect_charges_enabled":true,"connect_payouts_enabled":true,"connect_details_submitted":true,"connect_onboarded_at":"2026-07-10T00:00:00Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	status, err := p.GetConnectStatus(context.Background())
	if err != nil {
		t.Fatalf("GetConnectStatus: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/v1/self/connect/status" {
		t.Errorf("path = %q, want /v1/self/connect/status", gotPath)
	}
	if status.ConnectAccountID == nil || *status.ConnectAccountID != "acct_123" {
		t.Errorf("connect_account_id = %v, want acct_123", status.ConnectAccountID)
	}
	if !status.ConnectPayoutsEnabled {
		t.Error("connect_payouts_enabled = false, want true")
	}
}

func TestGetConnectStatus_ToleratesPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"connect_account_id":null,"connect_payouts_enabled":false}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	status, err := p.GetConnectStatus(context.Background())
	if err != nil {
		t.Fatalf("GetConnectStatus: %v", err)
	}
	if status.ConnectAccountID != nil {
		t.Errorf("connect_account_id = %v, want nil (never onboarded)", status.ConnectAccountID)
	}
	if status.ConnectChargesEnabled {
		t.Error("connect_charges_enabled = true, want false (absent -> zero value)")
	}
}

func TestGetEarnings_NoPeriod(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"period":"all","gross_cents":0}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	earnings, err := p.GetEarnings(context.Background(), "")
	if err != nil {
		t.Fatalf("GetEarnings: %v", err)
	}
	if gotPath != "/v1/self/marketplace/earnings" {
		t.Errorf("path = %q, want /v1/self/marketplace/earnings", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
	if earnings["period"] != "all" {
		t.Errorf("earnings[period] = %v, want all", earnings["period"])
	}
}

func TestGetEarnings_WithPeriod(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.GetEarnings(context.Background(), "2026-07"); err != nil {
		t.Fatalf("GetEarnings: %v", err)
	}
	if gotQuery != "period=2026-07" {
		t.Errorf("query = %q, want period=2026-07", gotQuery)
	}
}

func TestGetEarnings_EmptyPeriodOmitted(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.GetEarnings(context.Background(), "   "); err != nil {
		t.Fatalf("GetEarnings: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty (whitespace period omitted)", gotQuery)
	}
}

func TestSetDatasetMarketplace_BothFields(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"dataset-1","marketplace":{"price_monthly_cents":4999}}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	ds, err := p.SetDatasetMarketplace(context.Background(), "dataset-1", types.SetDatasetMarketplaceInput{
		PriceMonthlyCents: mktPtr(4999),
		Listed:            mktPtr(true),
	})
	if err != nil {
		t.Fatalf("SetDatasetMarketplace: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/v1/datasets/dataset-1/marketplace" {
		t.Errorf("path = %q, want /v1/datasets/dataset-1/marketplace", gotPath)
	}
	if gotBody["price_monthly_cents"] != float64(4999) {
		t.Errorf("body price = %v, want 4999", gotBody["price_monthly_cents"])
	}
	if gotBody["listed"] != true {
		t.Errorf("body listed = %v, want true", gotBody["listed"])
	}
	if ds.Marketplace == nil || ds.Marketplace.PriceMonthlyCents == nil || *ds.Marketplace.PriceMonthlyCents != 4999 {
		t.Errorf("returned dataset marketplace = %+v, want price 4999", ds.Marketplace)
	}
}

func TestSetDatasetMarketplace_PriceOnlyZeroSent(t *testing.T) {
	// A non-nil *int of 0 must be SENT (0 = free), not omitted.
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.SetDatasetMarketplace(context.Background(), "dataset-1", types.SetDatasetMarketplaceInput{
		PriceMonthlyCents: mktPtr(0),
	}); err != nil {
		t.Fatalf("SetDatasetMarketplace: %v", err)
	}
	if _, ok := gotBody["price_monthly_cents"]; !ok {
		t.Errorf("body = %v, want price_monthly_cents present (0 must be sent)", gotBody)
	}
	if gotBody["price_monthly_cents"] != float64(0) {
		t.Errorf("body price = %v, want 0", gotBody["price_monthly_cents"])
	}
	if _, ok := gotBody["listed"]; ok {
		t.Errorf("body = %v, want no listed key (nil omitted)", gotBody)
	}
}

func TestSetDatasetMarketplace_ListedOnlyFalse(t *testing.T) {
	// A non-nil *bool of false must be SENT (delist), not omitted.
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.SetDatasetMarketplace(context.Background(), "dataset-1", types.SetDatasetMarketplaceInput{
		Listed: mktPtr(false),
	}); err != nil {
		t.Fatalf("SetDatasetMarketplace: %v", err)
	}
	if v, ok := gotBody["listed"]; !ok || v != false {
		t.Errorf("body = %v, want listed:false present", gotBody)
	}
	if _, ok := gotBody["price_monthly_cents"]; ok {
		t.Errorf("body = %v, want no price key (nil omitted)", gotBody)
	}
}

func TestSetDatasetMarketplace_EscapesID(t *testing.T) {
	var gotEscaped string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.SetDatasetMarketplace(context.Background(), "data set", types.SetDatasetMarketplaceInput{
		Listed: mktPtr(true),
	}); err != nil {
		t.Fatalf("SetDatasetMarketplace: %v", err)
	}
	if gotEscaped != "/v1/datasets/data%20set/marketplace" {
		t.Errorf("escaped path = %q, want /v1/datasets/data%%20set/marketplace", gotEscaped)
	}
}

func TestSetDatasetMarketplace_FlagOff404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"marketplace disabled"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	_, err := p.SetDatasetMarketplace(context.Background(), "dataset-1", types.SetDatasetMarketplaceInput{
		Listed: mktPtr(true),
	})
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to contain 404", err.Error())
	}
}
