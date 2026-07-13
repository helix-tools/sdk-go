package consumer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// Marketplace consumer surface (PR6 / ClickUp 86e01nb3t):
//   - BrowseMarketplace          -> GET  /v1/datasets/marketplace
//   - GetDatasetDetails          -> GET  /v1/datasets/:id/details
//   - CreateSubscriptionCheckout -> POST /v1/subscriptions/checkout -> {url}
//
// Drives the REAL Consumer methods end-to-end against an httptest server (real
// SigV4 signing + real error mapping). Contract anchored to sdk-schemas PR #18
// and the helix-api MarketplaceDatasetsResponse / DatasetDetailsResponse.

const browseBody = `{
	"datasets": [
		{
			"_id": "dataset-1",
			"name": "Phone Feed",
			"producer_id": "producer-a",
			"category": "phone-numbers",
			"marketplace": {
				"price_monthly_cents": 4999,
				"currency": "usd",
				"stripe_product_id": "prod_ABC",
				"stripe_price_id": "price_ABC",
				"listed": true,
				"delisted_at": null
			}
		}
	],
	"pagination": {"total": 1, "page": 1, "per_page": 10, "total_pages": 1}
}`

func TestBrowseMarketplace_NoParams(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotMethod = r.URL.Path, r.URL.RawQuery, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(browseBody))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	resp, err := c.BrowseMarketplace(context.Background(), nil)
	if err != nil {
		t.Fatalf("BrowseMarketplace: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/v1/datasets/marketplace" {
		t.Errorf("path = %q, want /v1/datasets/marketplace", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
	if resp.Pagination.Total != 1 {
		t.Errorf("pagination.total = %d, want 1", resp.Pagination.Total)
	}
	// Marketplace object surfaced (pointer fields deref).
	if len(resp.Datasets) != 1 || resp.Datasets[0].Marketplace == nil {
		t.Fatalf("expected 1 dataset with marketplace, got %+v", resp.Datasets)
	}
	if got := resp.Datasets[0].Marketplace.PriceMonthlyCents; got == nil || *got != 4999 {
		t.Errorf("price_monthly_cents = %v, want 4999", got)
	}
	if got := resp.Datasets[0].Marketplace.Listed; got == nil || *got != true {
		t.Errorf("listed = %v, want true", got)
	}
}

func TestBrowseMarketplace_AllParams(t *testing.T) {
	// The SigV4 signer sorts the wire query, so assert params by parsing (order
	// is semantically irrelevant to the server), not by exact RawQuery string.
	var q url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(browseBody))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	_, err := c.BrowseMarketplace(context.Background(), &types.MarketplaceBrowseParams{
		Search: "phone", Category: "phone-numbers", Sort: "price_asc", Page: 2,
	})
	if err != nil {
		t.Fatalf("BrowseMarketplace: %v", err)
	}
	if q.Get("search") != "phone" || q.Get("category") != "phone-numbers" ||
		q.Get("sort") != "price_asc" || q.Get("page") != "2" {
		t.Errorf("query params = %v, want search=phone category=phone-numbers sort=price_asc page=2", q)
	}
	if len(q) != 4 {
		t.Errorf("query has %d params, want exactly 4", len(q))
	}
}

func TestBrowseMarketplace_PageOnly(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(browseBody))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.BrowseMarketplace(context.Background(), &types.MarketplaceBrowseParams{Page: 3}); err != nil {
		t.Fatalf("BrowseMarketplace: %v", err)
	}
	if gotQuery != "page=3" {
		t.Errorf("query = %q, want page=3", gotQuery)
	}
}

func TestBrowseMarketplace_PageZeroOmitted(t *testing.T) {
	// Go idiom: Page is 1-based; 0 (the zero value) is treated as unset.
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(browseBody))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.BrowseMarketplace(context.Background(), &types.MarketplaceBrowseParams{Page: 0}); err != nil {
		t.Fatalf("BrowseMarketplace: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty (Page 0 = unset)", gotQuery)
	}
}

func TestBrowseMarketplace_ToleratesDatasetWithoutMarketplace(t *testing.T) {
	body := `{"datasets":[{"_id":"d9","name":"Legacy","producer_id":"p"}],"pagination":{"total":1,"page":1,"per_page":10,"total_pages":1}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	resp, err := c.BrowseMarketplace(context.Background(), nil)
	if err != nil {
		t.Fatalf("BrowseMarketplace: %v", err)
	}
	if resp.Datasets[0].Marketplace != nil {
		t.Errorf("expected nil Marketplace (flag off), got %+v", resp.Datasets[0].Marketplace)
	}
	if resp.Datasets[0].Name != "Legacy" {
		t.Errorf("name = %q, want Legacy", resp.Datasets[0].Name)
	}
}

func TestGetDatasetDetails_HappyPath(t *testing.T) {
	var gotPath, gotMethod string
	body := `{"dataset":{"_id":"dataset-1","name":"Phone Feed"},"reviews":[],"related_datasets":[],"subscription_info":{"subscribed":false}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	resp, err := c.GetDatasetDetails(context.Background(), "dataset-1")
	if err != nil {
		t.Fatalf("GetDatasetDetails: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/v1/datasets/dataset-1/details" {
		t.Errorf("path = %q, want /v1/datasets/dataset-1/details", gotPath)
	}
	if resp.Dataset.Name != "Phone Feed" {
		t.Errorf("dataset.name = %q, want Phone Feed", resp.Dataset.Name)
	}
	if v, ok := resp.SubscriptionInfo["subscribed"].(bool); !ok || v {
		t.Errorf("subscription_info.subscribed = %v, want false", resp.SubscriptionInfo["subscribed"])
	}
}

func TestGetDatasetDetails_EscapesID(t *testing.T) {
	var gotEscaped string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"dataset":{},"reviews":[],"related_datasets":[],"subscription_info":{}}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.GetDatasetDetails(context.Background(), "data set"); err != nil {
		t.Fatalf("GetDatasetDetails: %v", err)
	}
	if gotEscaped != "/v1/datasets/data%20set/details" {
		t.Errorf("escaped path = %q, want /v1/datasets/data%%20set/details", gotEscaped)
	}
}

func TestCreateSubscriptionCheckout_DatasetID(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://checkout.stripe.com/c/pay/cs_1"}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	url, err := c.CreateSubscriptionCheckout(context.Background(), types.SubscriptionCheckoutInput{DatasetID: "dataset-1"})
	if err != nil {
		t.Fatalf("CreateSubscriptionCheckout: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/subscriptions/checkout" {
		t.Errorf("path = %q, want /v1/subscriptions/checkout", gotPath)
	}
	if len(gotBody) != 1 || gotBody["dataset_id"] != "dataset-1" {
		t.Errorf("body = %v, want {dataset_id: dataset-1}", gotBody)
	}
	if url != "https://checkout.stripe.com/c/pay/cs_1" {
		t.Errorf("url = %q", url)
	}
}

func TestCreateSubscriptionCheckout_RequestID(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"https://checkout.stripe.com/c/pay/cs_2"}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.CreateSubscriptionCheckout(context.Background(), types.SubscriptionCheckoutInput{RequestID: "req-9"}); err != nil {
		t.Fatalf("CreateSubscriptionCheckout: %v", err)
	}
	if len(gotBody) != 1 || gotBody["request_id"] != "req-9" {
		t.Errorf("body = %v, want {request_id: req-9}", gotBody)
	}
}

func TestCreateSubscriptionCheckout_TrimsID(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"url":"u"}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.CreateSubscriptionCheckout(context.Background(), types.SubscriptionCheckoutInput{DatasetID: "  dataset-1  "}); err != nil {
		t.Fatalf("CreateSubscriptionCheckout: %v", err)
	}
	if gotBody["dataset_id"] != "dataset-1" {
		t.Errorf("body dataset_id = %q, want trimmed dataset-1", gotBody["dataset_id"])
	}
}

func TestCreateSubscriptionCheckout_GuardNeither(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	_, err := c.CreateSubscriptionCheckout(context.Background(), types.SubscriptionCheckoutInput{})
	if err == nil {
		t.Fatal("expected error when neither id provided")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("error = %q, want mention of 'exactly one'", err.Error())
	}
	if hit {
		t.Error("server was hit; guard must short-circuit before the network")
	}
}

func TestCreateSubscriptionCheckout_GuardBoth(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	_, err := c.CreateSubscriptionCheckout(context.Background(), types.SubscriptionCheckoutInput{DatasetID: "d", RequestID: "r"})
	if err == nil {
		t.Fatal("expected error when both ids provided")
	}
	if hit {
		t.Error("server was hit; guard must short-circuit before the network")
	}
}

func TestCreateSubscriptionCheckout_FlagOff404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	_, err := c.CreateSubscriptionCheckout(context.Background(), types.SubscriptionCheckoutInput{DatasetID: "dataset-1"})
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to contain 404", err.Error())
	}
}
