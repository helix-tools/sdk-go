package types

import (
	"encoding/json"
	"testing"
)

// TestSubscriptionRequestStatus_ApprovedPendingPaymentRoundTrips: the API
// returns approved_pending_payment for a paid request. The SDK must name that
// status with a constant and decode it into the request's Status.
func TestSubscriptionRequestStatus_ApprovedPendingPaymentRoundTrips(t *testing.T) {
	if SubscriptionRequestStatusApprovedPendingPayment != "approved_pending_payment" {
		t.Fatalf("SubscriptionRequestStatusApprovedPendingPayment = %q, want \"approved_pending_payment\"", SubscriptionRequestStatusApprovedPendingPayment)
	}

	var req SubscriptionRequest
	body := `{"_id":"req-1","request_id":"req-1","status":"approved_pending_payment"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Status != SubscriptionRequestStatusApprovedPendingPayment {
		t.Errorf("Status = %q, want the approved_pending_payment constant", req.Status)
	}
}
