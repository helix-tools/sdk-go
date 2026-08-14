package types

import (
	"encoding/json"
	"testing"
)

// TestSubscription_ConsumerInfoRoundTrip pins subscription.consumer_info
// (subscription.schema.json) to Subscription.ConsumerInfo. Without this
// field, producers reading GET /v1/subscriptions silently lose the
// server-side consumer enrichment used to render the Subscribers tab.
func TestSubscription_ConsumerInfoRoundTrip(t *testing.T) {
	raw := `{
		"_id": "sub-1",
		"consumer_id": "cons-1",
		"dataset_id": "ds-1",
		"producer_id": "prod-1",
		"tier": "free",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z",
		"consumer_info": {
			"company_name": "Acme Data Co",
			"email": "buyer@acme.example"
		}
	}`

	var sub Subscription
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		t.Fatalf("unmarshal Subscription: %v", err)
	}
	if sub.ConsumerInfo == nil {
		t.Fatal("ConsumerInfo = nil, want populated struct")
	}
	if sub.ConsumerInfo.CompanyName != "Acme Data Co" {
		t.Errorf("ConsumerInfo.CompanyName = %q, want %q", sub.ConsumerInfo.CompanyName, "Acme Data Co")
	}
	if sub.ConsumerInfo.Email != "buyer@acme.example" {
		t.Errorf("ConsumerInfo.Email = %q, want %q", sub.ConsumerInfo.Email, "buyer@acme.example")
	}
}

// TestSubscription_ConsumerInfoAbsentIsNil proves subscriptions read on a
// path that doesn't populate the enrichment decode to a nil pointer, not a
// zero-value struct that would look like an empty-but-present record.
func TestSubscription_ConsumerInfoAbsentIsNil(t *testing.T) {
	raw := `{
		"_id": "sub-2",
		"consumer_id": "cons-2",
		"dataset_id": "ds-1",
		"producer_id": "prod-1",
		"tier": "free",
		"status": "active",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z"
	}`

	var sub Subscription
	if err := json.Unmarshal([]byte(raw), &sub); err != nil {
		t.Fatalf("unmarshal Subscription: %v", err)
	}
	if sub.ConsumerInfo != nil {
		t.Errorf("ConsumerInfo = %+v, want nil", sub.ConsumerInfo)
	}
}
