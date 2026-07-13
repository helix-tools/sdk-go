package producer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/helix-tools/sdk-go/v2/types"
)

// GetConnectOnboardingLink starts (or resumes) Stripe Connect Express payout
// onboarding and returns the hosted Account Link URL the producer must visit to
// complete it.
//
// POST /v1/self/connect/onboard. Returns the URL string only — the SDK NEVER
// opens or redirects to it.
func (p *Producer) GetConnectOnboardingLink(ctx context.Context) (string, error) {
	var resp struct {
		URL string `json:"url"`
	}
	if err := p.makeAPIRequest(ctx, http.MethodPost, "/v1/self/connect/onboard", nil, &resp); err != nil {
		return "", err
	}

	return resp.URL, nil
}

// GetConnectStatus gets the producer's Stripe Connect (Express) payout account
// status.
//
// GET /v1/self/connect/status. Mirrors the company connect_* fields; a producer
// who has never onboarded gets an empty/false-y status. Use
// ConnectPayoutsEnabled to know whether a dataset can be priced > 0.
func (p *Producer) GetConnectStatus(ctx context.Context) (*types.ConnectStatus, error) {
	var resp types.ConnectStatus
	if err := p.makeAPIRequest(ctx, http.MethodGet, "/v1/self/connect/status", nil, &resp); err != nil {
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
