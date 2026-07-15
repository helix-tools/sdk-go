package producer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// Marketplace producer surface (PR6/PR11 / ClickUp 86e01nb3t):
//   - ConnectOnboard        -> POST  /v1/self/connect/onboard      -> {url, account_id}
//   - GetConnectStatus      -> GET   /v1/self/connect/status
//   - CreateConnectLoginLink -> POST /v1/self/connect/login-link   -> {url}
//   - GetEarnings           -> GET   /v1/self/marketplace/earnings
//   - SetDatasetMarketplace -> PATCH /v1/datasets/:id/marketplace
//
// Drives the REAL Producer methods end-to-end against an httptest server (real
// SigV4 signing + real error mapping). The Connect surface is anchored to the
// Go API's internal/resources/connect/types.go response structs (OnboardResponse,
// StatusResponse, LoginLinkResponse) — verified field-for-field against
// helix-tools/api origin/main and mirrored by the Python SDK's PR #11
// (connect_onboard / get_connect_status / create_connect_login_link).

func mktPtr[T any](v T) *T { return &v }

// TestConnectOnboard_HappyPath pins that ConnectOnboard POSTs with no body and
// decodes BOTH url and account_id — the exact defect the Python SDK's PR #11
// fixed in the old GetConnectOnboardingLink (which silently dropped account_id).
func TestConnectOnboard_HappyPath(t *testing.T) {
	var gotPath, gotMethod string
	var gotBodyLen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBodyLen = len(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://connect.stripe.com/setup/e/acct/x","account_id":"acct_123"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.ConnectOnboard(context.Background())
	if err != nil {
		t.Fatalf("ConnectOnboard: %v", err)
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
	if resp.URL != "https://connect.stripe.com/setup/e/acct/x" {
		t.Errorf("url = %q", resp.URL)
	}
	if resp.AccountID != "acct_123" {
		t.Errorf("account_id = %q, want acct_123", resp.AccountID)
	}
}

// TestConnectOnboard_ServerError pins that a non-2xx surfaces as *APIError and
// the response pointer is nil (bad path).
func TestConnectOnboard_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"stripe unavailable"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.ConnectOnboard(context.Background())
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if resp != nil {
		t.Errorf("expected nil response on error, got %+v", resp)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
}

// TestGetConnectStatus_HappyPath pins the FULL StatusResponse contract
// (internal/resources/connect/types.go on helix-tools/api): unprefixed field
// names (charges_enabled, not connect_charges_enabled), plus the fields the
// stale Go SDK shape was missing entirely: onboarding_complete,
// can_price_datasets, requirements_due, disabled_reason.
func TestGetConnectStatus_HappyPath(t *testing.T) {
	var gotPath, gotMethod string
	body := `{"connect_account_id":"acct_123","charges_enabled":true,"payouts_enabled":true,"details_submitted":true,"onboarding_complete":true,"can_price_datasets":true,"requirements_due":false,"disabled_reason":""}`
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
	if status.ConnectAccountID != "acct_123" {
		t.Errorf("connect_account_id = %q, want acct_123", status.ConnectAccountID)
	}
	if !status.ChargesEnabled {
		t.Error("charges_enabled = false, want true")
	}
	if !status.PayoutsEnabled {
		t.Error("payouts_enabled = false, want true")
	}
	if !status.DetailsSubmitted {
		t.Error("details_submitted = false, want true")
	}
	if !status.OnboardingComplete {
		t.Error("onboarding_complete = false, want true")
	}
	if !status.CanPriceDatasets {
		t.Error("can_price_datasets = false, want true")
	}
	if status.RequirementsDue {
		t.Error("requirements_due = true, want false")
	}
	if status.DisabledReason != "" {
		t.Errorf("disabled_reason = %q, want empty", status.DisabledReason)
	}
}

// TestGetConnectStatus_RequirementsDueIsBool is a negative control anchoring the
// exact defect flagged in the task: requirements_due is a BOOL flag (Stripe has
// outstanding requirements), never a list of requirement keys. Decoding a JSON
// array into the bool field must fail loudly rather than silently zero out —
// proving the field really is typed bool end-to-end, not tolerantly ignored.
func TestGetConnectStatus_RequirementsDueIsBool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// The pre-PR#11 (wrong) shape some callers might still send: a list.
		_, _ = w.Write([]byte(`{"requirements_due":["individual.verification.document"]}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.GetConnectStatus(context.Background()); err == nil {
		t.Fatal("expected a decode error when requirements_due is a JSON array, not a bool")
	}
}

// TestGetConnectStatus_RequirementsDueTrue exercises the true branch explicitly
// (the "action required" / disabled_reason-populated path).
func TestGetConnectStatus_RequirementsDueTrue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"connect_account_id":"acct_456","payouts_enabled":false,"requirements_due":true,"disabled_reason":"requirements.past_due"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	status, err := p.GetConnectStatus(context.Background())
	if err != nil {
		t.Fatalf("GetConnectStatus: %v", err)
	}
	if !status.RequirementsDue {
		t.Error("requirements_due = false, want true")
	}
	if status.DisabledReason != "requirements.past_due" {
		t.Errorf("disabled_reason = %q, want requirements.past_due", status.DisabledReason)
	}
	if status.PayoutsEnabled {
		t.Error("payouts_enabled = true, want false")
	}
}

// TestGetConnectStatus_ToleratesPartial exercises the edge case of a producer
// who has never onboarded: the server omits most fields and every Go field
// must decode to its zero value rather than erroring.
func TestGetConnectStatus_ToleratesPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"connect_account_id":"","payouts_enabled":false}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	status, err := p.GetConnectStatus(context.Background())
	if err != nil {
		t.Fatalf("GetConnectStatus: %v", err)
	}
	if status.ConnectAccountID != "" {
		t.Errorf("connect_account_id = %q, want empty (never onboarded)", status.ConnectAccountID)
	}
	if status.ChargesEnabled {
		t.Error("charges_enabled = true, want false (absent -> zero value)")
	}
	if status.RequirementsDue {
		t.Error("requirements_due = true, want false (absent -> zero value)")
	}
}

// TestCreateConnectLoginLink_HappyPath pins the request shape (POST, no body)
// and the response decode for the method the stale Go SDK was missing entirely.
func TestCreateConnectLoginLink_HappyPath(t *testing.T) {
	var gotPath, gotMethod string
	var gotBodyLen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		gotBodyLen = len(raw)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://connect.stripe.com/express/y"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.CreateConnectLoginLink(context.Background())
	if err != nil {
		t.Fatalf("CreateConnectLoginLink: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/self/connect/login-link" {
		t.Errorf("path = %q, want /v1/self/connect/login-link", gotPath)
	}
	if gotBodyLen != 0 {
		t.Errorf("request body length = %d, want 0 (no body)", gotBodyLen)
	}
	if resp.URL != "https://connect.stripe.com/express/y" {
		t.Errorf("url = %q", resp.URL)
	}
}

// TestCreateConnectLoginLink_Forbidden pins that a 403 (no Connect account yet,
// or onboarding incomplete) surfaces cleanly as *APIError — the bad path the
// Python SDK's PR #11 documents mapping to PermissionDeniedError.
func TestCreateConnectLoginLink_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"connect account onboarding incomplete"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.CreateConnectLoginLink(context.Background())
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
	if resp != nil {
		t.Errorf("expected nil response on 403, got %+v", resp)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "onboarding incomplete") {
		t.Errorf("expected server error body to pass through, got %q", apiErr.Body)
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
