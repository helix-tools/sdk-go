package producer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/helix-tools/sdk-go/v2/types"
)

// ConnectOnboard starts (or resumes) Stripe Connect Express payout onboarding.
//
// POST /v1/self/connect/onboard — no request body; the producer id comes from
// the JWT (RequireProducerOrBoth). Returns the hosted Account Link URL plus the
// Connect account id. The SDK NEVER opens or redirects to the URL itself — the
// producer must open it to submit KYC and bank details.
//
// Replaces the earlier GetConnectOnboardingLink, which returned only the URL
// string and silently dropped the account id; no tagged release ever shipped
// that name.
func (p *Producer) ConnectOnboard(ctx context.Context) (*types.ConnectOnboardResponse, error) {
	var resp types.ConnectOnboardResponse
	if err := p.makeAPIRequest(ctx, http.MethodPost, "/v1/self/connect/onboard", nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetConnectStatus gets the producer's Stripe Connect (Express) payout account
// status.
//
// GET /v1/self/connect/status. A producer who has never onboarded gets an
// empty/false-y status. Use PayoutsEnabled (or the derived CanPriceDatasets) to
// know whether a dataset can be priced > 0.
func (p *Producer) GetConnectStatus(ctx context.Context) (*types.ConnectStatus, error) {
	var resp types.ConnectStatus
	if err := p.makeAPIRequest(ctx, http.MethodGet, "/v1/self/connect/status", nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// CreateConnectLoginLink creates a one-time login link to the producer's Stripe
// Express dashboard.
//
// POST /v1/self/connect/login-link — no request body; the producer id comes
// from the JWT. Returns the hosted Stripe Express dashboard URL only — the SDK
// NEVER opens or redirects to it.
//
// The server responds 403 when the producer has no Connect account yet, or has
// one but has not finished submitting onboarding details — logging in only
// works for an account that has completed onboarding. That surfaces here as an
// *APIError with StatusCode 403; call ConnectOnboard (and complete the hosted
// flow) first.
func (p *Producer) CreateConnectLoginLink(ctx context.Context) (*types.ConnectLoginLinkResponse, error) {
	var resp types.ConnectLoginLinkResponse
	if err := p.makeAPIRequest(ctx, http.MethodPost, "/v1/self/connect/login-link", nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetEarnings gets the producer's marketplace earnings rollup.
//
// GET /v1/self/marketplace/earnings (optionally ?period=). The response is a
// tolerant pass-through map (the earnings contract is not yet frozen server-side).
// An empty period is omitted (server default).
func (p *Producer) GetEarnings(ctx context.Context, period string) (types.MarketplaceEarnings, error) {
	path := "/v1/self/marketplace/earnings"
	if trimmed := strings.TrimSpace(period); trimmed != "" {
		path += "?period=" + url.QueryEscape(trimmed)
	}

	var resp types.MarketplaceEarnings
	if err := p.makeAPIRequest(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// SetDatasetMarketplace sets a dataset's marketplace pricing / listing
// (owner-only).
//
// PATCH /v1/datasets/:id/marketplace. Only the provided (non-nil) fields are sent
// (PATCH semantics): PriceMonthlyCents (USD cents; 0 = free) and/or Listed.
// Returns the updated dataset.
//
// When the marketplace_payments feature flag is off the endpoint responds 404 and
// this returns that API error.
func (p *Producer) SetDatasetMarketplace(ctx context.Context, datasetID string, input types.SetDatasetMarketplaceInput) (*types.Dataset, error) {
	path := fmt.Sprintf("/v1/datasets/%s/marketplace", url.PathEscape(datasetID))

	var resp types.Dataset
	if err := p.makeAPIRequest(ctx, http.MethodPatch, path, input, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}
