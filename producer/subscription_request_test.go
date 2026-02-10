package producer

import (
	"encoding/json"
	"testing"

	"github.com/helix-tools/sdk-go/types"
)

// TestApproveRejectPayloadMarshal tests that approve/reject payload is correctly marshaled.
func TestApproveRejectPayloadMarshal(t *testing.T) {
	reason := "Not authorized"
	notes := "Internal review complete"

	tests := []struct {
		name     string
		payload  types.ApproveRejectPayload
		expected map[string]any
	}{
		{
			name: "approve with notes",
			payload: types.ApproveRejectPayload{
				Action: "approve",
				Notes:  &notes,
			},
			expected: map[string]any{
				"action": "approve",
				"notes":  "Internal review complete",
			},
		},
		{
			name: "reject with reason",
			payload: types.ApproveRejectPayload{
				Action: "reject",
				Reason: &reason,
			},
			expected: map[string]any{
				"action": "reject",
				"reason": "Not authorized",
			},
		},
		{
			name: "approve without notes",
			payload: types.ApproveRejectPayload{
				Action: "approve",
			},
			expected: map[string]any{
				"action": "approve",
			},
		},
		{
			name: "reject without reason",
			payload: types.ApproveRejectPayload{
				Action: "reject",
			},
			expected: map[string]any{
				"action": "reject",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jsonBytes, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("failed to marshal payload: %v", err)
			}

			var parsed map[string]any
			if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}

			if parsed["action"] != tt.expected["action"] {
				t.Errorf("expected action '%v', got '%v'", tt.expected["action"], parsed["action"])
			}

			if expectedNotes, exists := tt.expected["notes"]; exists {
				if parsed["notes"] != expectedNotes {
					t.Errorf("expected notes '%v', got '%v'", expectedNotes, parsed["notes"])
				}
			} else {
				if _, exists := parsed["notes"]; exists {
					t.Errorf("notes should be omitted when nil")
				}
			}

			if expectedReason, exists := tt.expected["reason"]; exists {
				if parsed["reason"] != expectedReason {
					t.Errorf("expected reason '%v', got '%v'", expectedReason, parsed["reason"])
				}
			} else {
				if _, exists := parsed["reason"]; exists {
					t.Errorf("reason should be omitted when nil")
				}
			}
		})
	}
}

// TestSubscriptionRequestsResponseUnmarshal tests unmarshaling list response.
func TestSubscriptionRequestsResponseUnmarshal(t *testing.T) {
	responseJSON := `{
		"requests": [
			{
				"_id": "req-001",
				"request_id": "req-001",
				"consumer_id": "customer-111",
				"consumer_name": "Consumer One",
				"producer_id": "company-456",
				"tier": "basic",
				"status": "pending",
				"created_at": "2024-01-01T00:00:00Z",
				"updated_at": "2024-01-01T00:00:00Z"
			},
			{
				"_id": "req-002",
				"request_id": "req-002",
				"consumer_id": "customer-222",
				"consumer_name": "Consumer Two",
				"producer_id": "company-456",
				"tier": "premium",
				"status": "pending",
				"created_at": "2024-01-02T00:00:00Z",
				"updated_at": "2024-01-02T00:00:00Z"
			}
		],
		"count": 2
	}`

	var response types.SubscriptionRequestsResponse
	if err := json.Unmarshal([]byte(responseJSON), &response); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if response.Count != 2 {
		t.Errorf("expected count 2, got %d", response.Count)
	}

	if len(response.Requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(response.Requests))
	}

	// Check first request
	req1 := response.Requests[0]
	if req1.ID != "req-001" {
		t.Errorf("expected first request ID 'req-001', got '%s'", req1.ID)
	}
	if req1.ConsumerName != "Consumer One" {
		t.Errorf("expected first consumer name 'Consumer One', got '%s'", req1.ConsumerName)
	}
	if req1.Status != "pending" {
		t.Errorf("expected first status 'pending', got '%s'", req1.Status)
	}

	// Check second request
	req2 := response.Requests[1]
	if req2.ID != "req-002" {
		t.Errorf("expected second request ID 'req-002', got '%s'", req2.ID)
	}
	if req2.Tier != "premium" {
		t.Errorf("expected second tier 'premium', got '%s'", req2.Tier)
	}
}

// TestApproveSubscriptionRequestOptions tests the options type.
func TestApproveSubscriptionRequestOptions(t *testing.T) {
	notes := "Approved after verification"
	datasetID := "dataset-specific"

	opts := types.ApproveSubscriptionRequestOptions{
		Notes:     &notes,
		DatasetID: &datasetID,
	}

	if opts.Notes == nil || *opts.Notes != "Approved after verification" {
		t.Errorf("expected Notes 'Approved after verification', got '%v'", opts.Notes)
	}

	if opts.DatasetID == nil || *opts.DatasetID != "dataset-specific" {
		t.Errorf("expected DatasetID 'dataset-specific', got '%v'", opts.DatasetID)
	}
}

// TestApprovePayloadMapConstruction tests constructing approval payload as map.
func TestApprovePayloadMapConstruction(t *testing.T) {
	notes := "Approved"
	datasetID := "ds-123"

	// This mirrors the logic in ApproveSubscriptionRequest
	payloadMap := map[string]any{
		"action": "approve",
	}

	opts := &types.ApproveSubscriptionRequestOptions{
		Notes:     &notes,
		DatasetID: &datasetID,
	}

	if opts.Notes != nil {
		payloadMap["notes"] = *opts.Notes
	}
	if opts.DatasetID != nil {
		payloadMap["dataset_id"] = *opts.DatasetID
	}

	if payloadMap["action"] != "approve" {
		t.Errorf("expected action 'approve', got '%v'", payloadMap["action"])
	}

	if payloadMap["notes"] != "Approved" {
		t.Errorf("expected notes 'Approved', got '%v'", payloadMap["notes"])
	}

	if payloadMap["dataset_id"] != "ds-123" {
		t.Errorf("expected dataset_id 'ds-123', got '%v'", payloadMap["dataset_id"])
	}

	// Verify it marshals correctly
	jsonBytes, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["action"] != "approve" {
		t.Errorf("after marshal/unmarshal: expected action 'approve', got '%v'", parsed["action"])
	}
	if parsed["notes"] != "Approved" {
		t.Errorf("after marshal/unmarshal: expected notes 'Approved', got '%v'", parsed["notes"])
	}
	if parsed["dataset_id"] != "ds-123" {
		t.Errorf("after marshal/unmarshal: expected dataset_id 'ds-123', got '%v'", parsed["dataset_id"])
	}
}

// TestRejectPayloadMapConstruction tests constructing rejection payload as map.
func TestRejectPayloadMapConstruction(t *testing.T) {
	reason := "Insufficient information"

	// This mirrors the logic in RejectSubscriptionRequest
	payloadMap := map[string]any{
		"action": "reject",
	}

	if reason != "" {
		payloadMap["reason"] = reason
	}

	if payloadMap["action"] != "reject" {
		t.Errorf("expected action 'reject', got '%v'", payloadMap["action"])
	}

	if payloadMap["reason"] != "Insufficient information" {
		t.Errorf("expected reason 'Insufficient information', got '%v'", payloadMap["reason"])
	}

	// Verify it marshals correctly
	jsonBytes, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["action"] != "reject" {
		t.Errorf("after marshal/unmarshal: expected action 'reject', got '%v'", parsed["action"])
	}
	if parsed["reason"] != "Insufficient information" {
		t.Errorf("after marshal/unmarshal: expected reason 'Insufficient information', got '%v'", parsed["reason"])
	}
}

// TestRejectPayloadWithoutReason tests rejection without a reason.
func TestRejectPayloadWithoutReason(t *testing.T) {
	reason := ""

	payloadMap := map[string]any{
		"action": "reject",
	}

	if reason != "" {
		payloadMap["reason"] = reason
	}

	if payloadMap["action"] != "reject" {
		t.Errorf("expected action 'reject', got '%v'", payloadMap["action"])
	}

	if _, exists := payloadMap["reason"]; exists {
		t.Errorf("reason should not be in payload when empty")
	}
}
