package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSubscription_DecodesAutoRenewCancellationReasonSubscribedAt: the API
// sends these three fields. Before they were typed, a consumer decoding a
// Subscription silently lost them, so a cancelled subscription showed no reason.
func TestSubscription_DecodesAutoRenewCancellationReasonSubscribedAt(t *testing.T) {
	body := `{
		"_id": "sub-1",
		"consumer_id": "consumer-1",
		"producer_id": "producer-1",
		"tier": "free",
		"status": "cancelled",
		"auto_renew": true,
		"cancellation_reason": "non-payment",
		"subscribed_at": "2026-09-01T10:00:00Z",
		"created_at": "2026-09-01T10:00:00Z",
		"updated_at": "2026-09-20T10:00:00Z"
	}`

	var sub Subscription
	if err := json.Unmarshal([]byte(body), &sub); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !sub.AutoRenew {
		t.Errorf("AutoRenew = false, want true")
	}
	if sub.CancellationReason != "non-payment" {
		t.Errorf("CancellationReason = %q, want \"non-payment\"", sub.CancellationReason)
	}
	if sub.SubscribedAt != "2026-09-01T10:00:00Z" {
		t.Errorf("SubscribedAt = %q, want the RFC 3339 value", sub.SubscribedAt)
	}
}

// TestSubscription_NewFieldsOmittedWhenUnset: the API omits auto_renew when
// false and omits the other two when empty, so a Subscription built without
// them must not send them back.
func TestSubscription_NewFieldsOmittedWhenUnset(t *testing.T) {
	raw, err := json.Marshal(Subscription{ID: "sub-1", ConsumerID: "c", ProducerID: "p", Tier: "free", Status: "active"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"auto_renew", "cancellation_reason", "subscribed_at"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("marshalled Subscription carries %q although it is unset: %s", key, raw)
		}
	}
}
