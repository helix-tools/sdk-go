package api

import (
	"context"
	"testing"

	"github.com/helix-tools/sdk-go/types"
)

// TestSubscriptionRequests runs integration tests for subscription request operations.
// These tests require both producer and consumer credentials.
func TestSubscriptionRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireProducerCredentials(t)
	cfg.RequireConsumerCredentials(t)

	ctx := context.Background()
	testID := GenerateTestID()

	// Create clients for both producer and consumer.
	producerClient := NewTestClient(t, cfg, cfg.ProducerCredentials)
	consumerClient := NewTestClient(t, cfg, cfg.ConsumerCredentials)
	cleanup := NewCleanupRegistry(t)

	defer cleanup.RunAll(ctx)

	var createdRequestID string
	var createdSubscriptionID string

	t.Run("List_Consumer_Requests", func(t *testing.T) {
		var resp types.SubscriptionRequestsResponse

		err := consumerClient.Get(ctx, "/v1/subscription-requests", &resp)
		if err != nil {
			t.Fatalf("failed to list consumer requests: %v", err)
		}

		t.Logf("Consumer has %d subscription requests", resp.Count)
	})

	t.Run("List_Producer_Requests", func(t *testing.T) {
		var resp types.SubscriptionRequestsResponse

		err := producerClient.Get(ctx, "/v1/producers/subscription-requests", &resp)
		if err != nil {
			t.Fatalf("failed to list producer requests: %v", err)
		}

		t.Logf("Producer has %d incoming requests", resp.Count)
	})

	t.Run("Create_SubscriptionRequest", func(t *testing.T) {
		message := "Integration test request - " + testID
		req := types.CreateSubscriptionRequestPayload{
			ProducerID: cfg.ProducerCredentials.CustomerID,
			Tier:       "basic",
			Message:    &message,
		}

		var request types.SubscriptionRequest

		err := consumerClient.Post(ctx, "/v1/subscription-requests", req, &request)
		if err != nil {
			t.Fatalf("failed to create subscription request: %v", err)
		}

		if request.Status != "pending" {
			t.Errorf("expected status pending, got %s", request.Status)
		}

		createdRequestID = request.RequestID
		if createdRequestID == "" {
			createdRequestID = request.ID
		}

		t.Logf("Created subscription request: %s (status: %s)", createdRequestID, request.Status)

		// Register cleanup to reject if not handled.
		cleanup.RegisterSubscriptionRequestCleanup(producerClient, createdRequestID)
	})

	t.Run("Get_SubscriptionRequest_ByID", func(t *testing.T) {
		if createdRequestID == "" {
			t.Skip("no request created")
		}

		var request types.SubscriptionRequest

		err := consumerClient.Get(ctx, "/v1/subscription-requests/"+createdRequestID, &request)
		if err != nil {
			t.Fatalf("failed to get subscription request: %v", err)
		}

		requestID := request.RequestID
		if requestID == "" {
			requestID = request.ID
		}

		if requestID != createdRequestID {
			t.Errorf("expected request ID %s, got %s", createdRequestID, requestID)
		}

		t.Logf("Request: consumer=%s, producer=%s, tier=%s, status=%s",
			request.ConsumerID, request.ProducerID, request.Tier, request.Status)
	})

	t.Run("List_Pending_Requests", func(t *testing.T) {
		var resp types.SubscriptionRequestsResponse

		err := consumerClient.Get(ctx, "/v1/subscription-requests?status=pending", &resp)
		if err != nil {
			t.Fatalf("failed to list pending requests: %v", err)
		}

		t.Logf("Consumer has %d pending requests", resp.Count)
	})

	t.Run("Approve_SubscriptionRequest", func(t *testing.T) {
		if createdRequestID == "" {
			t.Skip("no request created")
		}

		notes := "Approved for integration testing"
		req := types.ApproveRejectPayload{
			Action: "approve",
			Notes:  &notes,
		}

		var resp types.ApproveRequestResponse

		err := producerClient.Post(ctx, "/v1/subscription-requests/"+createdRequestID, req, &resp)
		if err != nil {
			t.Fatalf("failed to approve subscription request: %v", err)
		}

		if resp.Request.Status != "approved" {
			t.Errorf("expected status approved, got %s", resp.Request.Status)
		}

		if resp.Subscription != nil {
			createdSubscriptionID = resp.Subscription.ID
			t.Logf("Approved request, created subscription: %s", createdSubscriptionID)

			// Register cleanup to revoke subscription.
			cleanup.RegisterSubscriptionCleanup(producerClient, createdSubscriptionID)
		} else {
			t.Log("Request approved (subscription info not returned)")
		}
	})

	t.Run("Reject_SubscriptionRequest", func(t *testing.T) {
		// Create a new request to reject.
		message := "Request to be rejected - " + testID
		createReq := types.CreateSubscriptionRequestPayload{
			ProducerID: cfg.ProducerCredentials.CustomerID,
			Tier:       "basic",
			Message:    &message,
		}

		var newRequest types.SubscriptionRequest

		err := consumerClient.Post(ctx, "/v1/subscription-requests", createReq, &newRequest)
		if err != nil {
			t.Fatalf("failed to create request for rejection: %v", err)
		}

		newRequestID := newRequest.RequestID
		if newRequestID == "" {
			newRequestID = newRequest.ID
		}

		t.Logf("Created request to reject: %s", newRequestID)

		// Reject the request.
		reason := "Integration test rejection"
		rejectReq := types.ApproveRejectPayload{
			Action: "reject",
			Reason: &reason,
		}

		var rejectedRequest types.SubscriptionRequest

		err = producerClient.Post(ctx, "/v1/subscription-requests/"+newRequestID, rejectReq, &rejectedRequest)
		if err != nil {
			t.Fatalf("failed to reject subscription request: %v", err)
		}

		if rejectedRequest.Status != "rejected" {
			t.Errorf("expected status rejected, got %s", rejectedRequest.Status)
		}

		t.Logf("Rejected request: %s (reason: %s)", newRequestID, reason)
	})

	t.Run("Get_Request_NotFound", func(t *testing.T) {
		var request types.SubscriptionRequest

		err := consumerClient.Get(ctx, "/v1/subscription-requests/nonexistent-request-id", &request)
		if err == nil {
			t.Error("expected error for nonexistent request")
		}

		if !IsNotFoundError(err) {
			t.Logf("Note: API returned %v (expected 404)", err)
		}
	})
}

// TestSubscriptionRequestValidation tests input validation for subscription requests.
func TestSubscriptionRequestValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireConsumerCredentials(t)

	ctx := context.Background()
	client := NewTestClient(t, cfg, cfg.ConsumerCredentials)

	t.Run("Create_Request_MissingProducerID", func(t *testing.T) {
		req := types.CreateSubscriptionRequestPayload{
			Tier: "basic",
		}

		var request types.SubscriptionRequest

		err := client.Post(ctx, "/v1/subscription-requests", req, &request)
		if err == nil {
			t.Error("expected error for missing producer ID")
		}

		if !IsBadRequestError(err) {
			t.Logf("Note: API returned %v (expected 400)", err)
		}
	})

	t.Run("Create_Request_InvalidProducerID", func(t *testing.T) {
		req := types.CreateSubscriptionRequestPayload{
			ProducerID: "nonexistent-producer-id",
			Tier:       "basic",
		}

		var request types.SubscriptionRequest

		err := client.Post(ctx, "/v1/subscription-requests", req, &request)
		if err == nil {
			t.Log("Warning: API accepted nonexistent producer ID")
		} else {
			t.Logf("Validation error: %v", err)
		}
	})
}
