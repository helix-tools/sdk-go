package types

// Marketplace (flat monthly per-dataset subscription pricing).
//
// Mirrors sdk-schemas PR #18: dataset.marketplace, subscription.billing, and the
// company Stripe Connect fields. Every field is OPTIONAL — when the server
// `marketplace_payments` feature flag is off the API omits all marketplace
// fields, so absence MUST be tolerated (nil pointers / zero values; never invent
// defaults). Pointer fields distinguish "server sent 0/false" from "absent".

// BillingStatus is the billing (PAYMENT) lifecycle state of a marketplace
// subscription. Spelling follows Stripe ("canceled"); distinct from
// SubscriptionStatus (the ACCESS state, British "cancelled").
type BillingStatus = string

// Canonical BillingStatus values (subscription.billing.billing_status enum).
const (
	BillingStatusFree           BillingStatus = "free"
	BillingStatusPendingPayment BillingStatus = "pending_payment"
	BillingStatusActive         BillingStatus = "active"
	BillingStatusPastDue        BillingStatus = "past_due"
	BillingStatusSuspended      BillingStatus = "suspended"
	BillingStatusCanceled       BillingStatus = "canceled"
)

// DatasetMarketplace is the marketplace pricing on a dataset
// (dataset.marketplace). All fields server-managed EXCEPT PriceMonthlyCents
// (producer-set). Pointers so absence (nil) is distinct from 0/false.
type DatasetMarketplace struct {
	// PriceMonthlyCents is the producer-set flat monthly price in USD cents.
	// 0 = free. nil = absent.
	PriceMonthlyCents *int `json:"price_monthly_cents,omitempty"`
	// Currency is the ISO 4217 code, lowercase (Stripe convention). USD only in v1.
	Currency string `json:"currency,omitempty"`
	// StripeProductID is server-managed; null/absent until synced to Stripe.
	StripeProductID *string `json:"stripe_product_id,omitempty"`
	// StripePriceID is server-managed; null/absent until synced to Stripe.
	StripePriceID *string `json:"stripe_price_id,omitempty"`
	// Listed is server-managed: whether the dataset is listed (visible/subscribable).
	Listed *bool `json:"listed,omitempty"`
	// DelistedAt is a server-managed ISO 8601 timestamp; null while listed.
	DelistedAt *string `json:"delisted_at,omitempty"`
}

// SubscriptionBilling is the billing (payment) state of a marketplace
// subscription (subscription.billing). Optional: free/legacy subscriptions omit
// it or carry BillingStatus "free".
type SubscriptionBilling struct {
	BillingStatus           BillingStatus `json:"billing_status,omitempty"`
	StripeSubscriptionID    *string       `json:"stripe_subscription_id,omitempty"`
	StripeCheckoutSessionID *string       `json:"stripe_checkout_session_id,omitempty"`
	CurrentPeriodEnd        *string       `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd       *bool         `json:"cancel_at_period_end,omitempty"`
}

// MarketplaceBrowseParams are the optional query filters for BrowseMarketplace
// (GET /v1/datasets/marketplace). Only non-empty fields are sent; Page is
// 1-based and 0 (the zero value) is treated as unset.
type MarketplaceBrowseParams struct {
	Search   string
	Category string
	Sort     string
	Page     int
}

// MarketplacePagination is the pagination block on the marketplace browse
// response. Mirrors the helix-api MarketplacePagination.
type MarketplacePagination struct {
	Total      int64 `json:"total"`
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	TotalPages int   `json:"total_pages"`
}

// MarketplaceBrowseResponse is returned by GET /v1/datasets/marketplace
// (BrowseMarketplace). Mirrors the helix-api MarketplaceDatasetsResponse:
// datasets + pagination. Each dataset may carry an optional Marketplace object.
type MarketplaceBrowseResponse struct {
	Datasets   []Dataset             `json:"datasets"`
	Pagination MarketplacePagination `json:"pagination"`
}

// DatasetDetails is returned by GET /v1/datasets/:id/details (GetDatasetDetails).
// A COMPOSITE (mirrors the helix-api DatasetDetailsResponse), not a bare
// dataset. Reviews / SubscriptionInfo are intentionally loose (no frozen schema).
type DatasetDetails struct {
	Dataset          Dataset                  `json:"dataset"`
	Reviews          []map[string]interface{} `json:"reviews"`
	RelatedDatasets  []Dataset                `json:"related_datasets"`
	SubscriptionInfo map[string]interface{}   `json:"subscription_info"`
}

// SubscriptionCheckoutInput is the input for CreateSubscriptionCheckout. EXACTLY
// ONE of DatasetID or RequestID (an approval-gated request awaiting payment) must
// be non-empty.
type SubscriptionCheckoutInput struct {
	// DatasetID subscribes directly to this dataset.
	DatasetID string
	// RequestID completes payment for a previously-approved subscription request.
	RequestID string
}

// ConnectStatus is the producer's Stripe Connect (Express) payout account status
// (GET /v1/self/connect/status). Mirrors the company connect_* fields; a producer
// who has never onboarded gets an empty/false-y status.
type ConnectStatus struct {
	ConnectAccountID        *string `json:"connect_account_id,omitempty"`
	ConnectChargesEnabled   bool    `json:"connect_charges_enabled,omitempty"`
	ConnectPayoutsEnabled   bool    `json:"connect_payouts_enabled,omitempty"`
	ConnectDetailsSubmitted bool    `json:"connect_details_submitted,omitempty"`
	ConnectOnboardedAt      *string `json:"connect_onboarded_at,omitempty"`
}

// SetDatasetMarketplaceInput is the input for SetDatasetMarketplace
// (PATCH /v1/datasets/:id/marketplace). Only non-nil fields are sent (PATCH
// semantics); pointers let 0/false be sent explicitly while nil is omitted.
type SetDatasetMarketplaceInput struct {
	// PriceMonthlyCents is the new flat monthly price in USD cents (0 = free).
	PriceMonthlyCents *int `json:"price_monthly_cents,omitempty"`
	// Listed is whether the dataset is listed (visible/subscribable).
	Listed *bool `json:"listed,omitempty"`
}

// MarketplaceEarnings is the producer marketplace earnings rollup
// (GET /v1/self/marketplace/earnings). Tolerant pass-through: the earnings
// response contract is not yet frozen server-side, so it is an open map the
// caller reads by key.
type MarketplaceEarnings = map[string]interface{}
