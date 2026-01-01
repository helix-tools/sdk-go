package api

import (
	"context"
	"testing"

	"github.com/helix-tools/sdk-go/types"
)

// TestSubscriptions runs integration tests for subscription operations.
// These tests require both producer and consumer credentials.
func TestSubscriptions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireProducerCredentials(t)
	cfg.RequireConsumerCredentials(t)

	ctx := context.Background()
	testID := GenerateTestID()

	producerClient := NewTestClient(t, cfg, cfg.ProducerCredentials)
	consumerClient := NewTestClient(t, cfg, cfg.ConsumerCredentials)
	cleanup := NewCleanupRegistry(t)

	defer cleanup.RunAll(ctx)

	var subscriptionID string

	t.Run("List_Consumer_Subscriptions", func(t *testing.T) {
		var resp types.SubscriptionsResponse

		err := consumerClient.Get(ctx, "/v1/subscriptions", &resp)
		if err != nil {
			t.Fatalf("failed to list consumer subscriptions: %v", err)
		}

		t.Logf("Consumer has %d subscriptions", resp.Count)

		// Log some details if there are subscriptions.
		for i, sub := range resp.Subscriptions {
			if i >= 3 {
				t.Logf("... and %d more", resp.Count-3)
				break
			}

			t.Logf("  - %s: %s (tier: %s, status: %s)",
				sub.ID, sub.DatasetName, sub.Tier, sub.Status)
		}
	})

	t.Run("Create_Subscription_ViaApproval", func(t *testing.T) {
		// Subscriptions are created by approving subscription requests.
		// Create a request and approve it.
		message := "Subscription test - " + testID
		createReq := types.CreateSubscriptionRequestPayload{
			ProducerID: cfg.ProducerCredentials.CustomerID,
			Tier:       "basic",
			Message:    &message,
		}

		var request types.SubscriptionRequest

		err := consumerClient.Post(ctx, "/v1/subscription-requests", createReq, &request)
		if err != nil {
			t.Fatalf("failed to create subscription request: %v", err)
		}

		requestID := request.RequestID
		if requestID == "" {
			requestID = request.ID
		}

		t.Logf("Created request: %s", requestID)

		// Approve the request.
		notes := "Integration test approval"
		approveReq := types.ApproveRejectPayload{
			Action: "approve",
			Notes:  &notes,
		}

		var approveResp types.ApproveRequestResponse

		err = producerClient.Post(ctx, "/v1/subscription-requests/"+requestID, approveReq, &approveResp)
		if err != nil {
			t.Fatalf("failed to approve request: %v", err)
		}

		if approveResp.Subscription != nil {
			subscriptionID = approveResp.Subscription.ID
			t.Logf("Created subscription: %s", subscriptionID)

			// Register cleanup.
			cleanup.RegisterSubscriptionCleanup(producerClient, subscriptionID)
		} else {
			// Try to find the subscription from the list.
			var subs types.SubscriptionsResponse

			err = consumerClient.Get(ctx, "/v1/subscriptions", &subs)
			if err == nil {
				for _, sub := range subs.Subscriptions {
					if sub.RequestID == requestID {
						subscriptionID = sub.ID
						t.Logf("Found subscription: %s", subscriptionID)
						cleanup.RegisterSubscriptionCleanup(producerClient, subscriptionID)

						break
					}
				}
			}
		}
	})

	t.Run("List_Producer_Subscriptions_ByDataset", func(t *testing.T) {
		cfg.RequireTestDatasetID(t)

		var resp types.SubscriptionsResponse

		path := "/v1/subscriptions?dataset_id=" + cfg.TestDatasetID

		err := producerClient.Get(ctx, path, &resp)
		if err != nil {
			t.Fatalf("failed to list dataset subscriptions: %v", err)
		}

		t.Logf("Dataset %s has %d subscribers", cfg.TestDatasetID, resp.Count)
	})

	t.Run("Revoke_Subscription", func(t *testing.T) {
		if subscriptionID == "" {
			t.Skip("no subscription created")
		}

		var resp types.RevokeSubscriptionResponse

		err := producerClient.Put(ctx, "/v1/subscriptions/"+subscriptionID+"/revoke", map[string]string{}, &resp)
		if err != nil {
			t.Fatalf("failed to revoke subscription: %v", err)
		}

		if resp.Status != "cancelled" {
			t.Errorf("expected status cancelled, got %s", resp.Status)
		}

		t.Logf("Revoked subscription: %s (status: %s)", subscriptionID, resp.Status)
	})

	t.Run("Verify_Revoked_Subscription", func(t *testing.T) {
		if subscriptionID == "" {
			t.Skip("no subscription created")
		}

		// List subscriptions and verify the revoked one is not active.
		var resp types.SubscriptionsResponse

		err := consumerClient.Get(ctx, "/v1/subscriptions", &resp)
		if err != nil {
			t.Fatalf("failed to list subscriptions: %v", err)
		}

		for _, sub := range resp.Subscriptions {
			if sub.ID == subscriptionID {
				if sub.Status == "active" {
					t.Errorf("subscription %s should not be active after revocation", subscriptionID)
				} else {
					t.Logf("Subscription %s status: %s", subscriptionID, sub.Status)
				}

				return
			}
		}

		t.Log("Revoked subscription not found in list (as expected)")
	})
}

// TestSubscriptionWithDataset tests subscription flow with a specific dataset.
func TestSubscriptionWithDataset(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg := LoadTestConfig(t)
	cfg.RequireProducerCredentials(t)
	cfg.RequireConsumerCredentials(t)
	cfg.RequireTestDatasetID(t)

	ctx := context.Background()
	testID := GenerateTestID()

	producerClient := NewTestClient(t, cfg, cfg.ProducerCredentials)
	consumerClient := NewTestClient(t, cfg, cfg.ConsumerCredentials)
	cleanup := NewCleanupRegistry(t)

	defer cleanup.RunAll(ctx)

	var subscriptionID string

	t.Run("Request_DatasetSpecific_Subscription", func(t *testing.T) {
		datasetID := cfg.TestDatasetID
		message := "Dataset-specific request - " + testID
		req := types.CreateSubscriptionRequestPayload{
			ProducerID: cfg.ProducerCredentials.CustomerID,
			DatasetID:  &datasetID,
			Tier:       "premium",
			Message:    &message,
		}

		var request types.SubscriptionRequest

		err := consumerClient.Post(ctx, "/v1/subscription-requests", req, &request)
		if err != nil {
			t.Fatalf("failed to create dataset-specific request: %v", err)
		}

		requestID := request.RequestID
		if requestID == "" {
			requestID = request.ID
		}

		t.Logf("Created dataset-specific request: %s", requestID)

		// Approve the request.
		notes := "Dataset access approved"
		approveReq := types.ApproveRejectPayload{
			Action: "approve",
			Notes:  &notes,
		}

		var approveResp types.ApproveRequestResponse

		err = producerClient.Post(ctx, "/v1/subscription-requests/"+requestID, approveReq, &approveResp)
		if err != nil {
			t.Fatalf("failed to approve request: %v", err)
		}

		if approveResp.Subscription != nil {
			subscriptionID = approveResp.Subscription.ID

			if approveResp.Subscription.DatasetID != nil && *approveResp.Subscription.DatasetID == datasetID {
				t.Logf("Created dataset-specific subscription: %s", subscriptionID)
			} else {
				t.Log("Subscription created but dataset ID not confirmed in response")
			}

			cleanup.RegisterSubscriptionCleanup(producerClient, subscriptionID)
		}
	})

	t.Run("Verify_Dataset_Subscriber_Count", func(t *testing.T) {
		if subscriptionID == "" {
			t.Skip("no subscription created")
		}

		var resp types.SubscriptionsResponse

		path := "/v1/subscriptions?dataset_id=" + cfg.TestDatasetID

		err := producerClient.Get(ctx, path, &resp)
		if err != nil {
			t.Fatalf("failed to list dataset subscribers: %v", err)
		}

		if resp.Count < 1 {
			t.Error("expected at least 1 subscriber")
		}

		t.Logf("Dataset has %d subscribers", resp.Count)
	})
}
