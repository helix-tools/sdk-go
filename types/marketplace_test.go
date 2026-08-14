package types

import (
	"encoding/json"
	"testing"
)

// TestDatasetMarketplace_PendingDeletionAtRoundTrip pins pending_deletion_at
// (dataset.schema.json marketplace.pending_deletion_at) to
// DatasetMarketplace.PendingDeletionAt. Without the field, unmarshal silently
// drops it and producers can never see when a delisted dataset becomes
// removable.
func TestDatasetMarketplace_PendingDeletionAtRoundTrip(t *testing.T) {
	raw := `{
		"price_monthly_cents": 500,
		"currency": "usd",
		"listed": false,
		"delisted_at": "2026-07-01T00:00:00Z",
		"pending_deletion_at": "2026-08-01T00:00:00Z"
	}`

	var m DatasetMarketplace
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal DatasetMarketplace: %v", err)
	}
	if m.PendingDeletionAt == nil || *m.PendingDeletionAt != "2026-08-01T00:00:00Z" {
		t.Errorf("PendingDeletionAt = %v, want 2026-08-01T00:00:00Z", m.PendingDeletionAt)
	}
}

// TestDatasetMarketplace_PendingDeletionAtAbsentIsNil proves the "no active
// paid subscription has a known period end" case decodes to nil, not a
// zero-value string.
func TestDatasetMarketplace_PendingDeletionAtAbsentIsNil(t *testing.T) {
	raw := `{"price_monthly_cents": 0, "currency": "usd", "listed": true}`

	var m DatasetMarketplace
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal DatasetMarketplace: %v", err)
	}
	if m.PendingDeletionAt != nil {
		t.Errorf("PendingDeletionAt = %v, want nil", *m.PendingDeletionAt)
	}
}

// TestSubscriptionBilling_PriceAndStripePriceRoundTrip pins
// subscription.billing.price_monthly_cents and
// subscription.billing.stripe_price_id (subscription.schema.json) to
// SubscriptionBilling. These are distinct from the dataset-level
// marketplace.stripe_price_id: this one, when present, overrides the
// dataset's price at checkout.
func TestSubscriptionBilling_PriceAndStripePriceRoundTrip(t *testing.T) {
	raw := `{
		"billing_status": "active",
		"stripe_subscription_id": "sub_ABC123",
		"stripe_price_id": "price_XYZ789",
		"price_monthly_cents": 1500,
		"current_period_end": "2026-09-01T00:00:00Z",
		"cancel_at_period_end": false
	}`

	var b SubscriptionBilling
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal SubscriptionBilling: %v", err)
	}
	if b.StripePriceID == nil || *b.StripePriceID != "price_XYZ789" {
		t.Errorf("StripePriceID = %v, want price_XYZ789", b.StripePriceID)
	}
	if b.PriceMonthlyCents == nil || *b.PriceMonthlyCents != 1500 {
		t.Errorf("PriceMonthlyCents = %v, want 1500", b.PriceMonthlyCents)
	}
}

// TestSubscriptionBilling_FreeSubscriptionOmitsPriceFields proves a free
// subscription (the default/legacy case) decodes both new fields to nil
// rather than a misleading 0.
func TestSubscriptionBilling_FreeSubscriptionOmitsPriceFields(t *testing.T) {
	raw := `{"billing_status": "free"}`

	var b SubscriptionBilling
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal SubscriptionBilling: %v", err)
	}
	if b.StripePriceID != nil {
		t.Errorf("StripePriceID = %v, want nil", *b.StripePriceID)
	}
	if b.PriceMonthlyCents != nil {
		t.Errorf("PriceMonthlyCents = %v, want nil", *b.PriceMonthlyCents)
	}
}
