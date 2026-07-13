package consumer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/helix-tools/sdk-go/v2/types"
)

// BrowseMarketplace browses the public dataset marketplace.
//
// GET /v1/datasets/marketplace — a PUBLIC endpoint (the SDK still signs the
// request, which the public route accepts). Only the provided filters are sent
// as query params: search, category, sort, page (1-based; Page 0 = unset).
//
// The returned datasets may each carry an optional Marketplace object; it is nil
// while the marketplace_payments feature flag is off (tolerated, never defaulted).
func (c *Consumer) BrowseMarketplace(ctx context.Context, params *types.MarketplaceBrowseParams) (*types.MarketplaceBrowseResponse, error) {
	path := "/v1/datasets/marketplace"

	if params != nil {
		// Only non-empty filters are sent. Page is 1-based; 0 (the zero value)
		// is treated as unset. url.Values.Encode sorts keys — harmless, and the
		// SigV4 signer sorts the wire query regardless, so order never matters
		// to the server.
		q := url.Values{}
		if params.Search != "" {
			q.Set("search", params.Search)
		}
		if params.Category != "" {
			q.Set("category", params.Category)
		}
		if params.Sort != "" {
			q.Set("sort", params.Sort)
		}
		if params.Page > 0 {
			q.Set("page", strconv.Itoa(params.Page))
		}
		if encoded := q.Encode(); encoded != "" {
			path += "?" + encoded
		}
	}

	var resp types.MarketplaceBrowseResponse
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetDatasetDetails gets the public detail view for a single dataset.
//
// GET /v1/datasets/:id/details — a PUBLIC endpoint. Returns a COMPOSITE
// (dataset + reviews + related datasets + the caller's subscription info), not a
// bare dataset.
func (c *Consumer) GetDatasetDetails(ctx context.Context, datasetID string) (*types.DatasetDetails, error) {
	path := fmt.Sprintf("/v1/datasets/%s/details", url.PathEscape(datasetID))

	var resp types.DatasetDetails
	if err := c.makeAPIRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// CreateSubscriptionCheckout creates a Stripe Checkout session to subscribe to a
// PAID marketplace dataset and returns the hosted Checkout URL.
//
// POST /v1/subscriptions/checkout with EXACTLY ONE of input.DatasetID or
// input.RequestID (an approval-gated request awaiting payment). Returns the URL
// string only — the SDK NEVER opens or redirects to it.
//
// When the marketplace_payments feature flag is off the endpoint responds 404
// and this returns that API error.
func (c *Consumer) CreateSubscriptionCheckout(ctx context.Context, input types.SubscriptionCheckoutInput) (string, error) {
	datasetID := strings.TrimSpace(input.DatasetID)
	requestID := strings.TrimSpace(input.RequestID)

	// Exactly one of DatasetID / RequestID must be provided.
	if (datasetID != "") == (requestID != "") {
		return "", fmt.Errorf(
			"CreateSubscriptionCheckout requires exactly one of DatasetID or RequestID",
		)
	}

	var body map[string]string
	if datasetID != "" {
		body = map[string]string{"dataset_id": datasetID}
	} else {
		body = map[string]string{"request_id": requestID}
	}

	var resp struct {
		URL string `json:"url"`
	}
	if err := c.makeAPIRequest(ctx, http.MethodPost, "/v1/subscriptions/checkout", body, &resp); err != nil {
		return "", err
	}

	return resp.URL, nil
}
