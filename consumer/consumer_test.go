package consumer

import (
	"encoding/json"
	"testing"
)

// TestSubscriptionUnmarshalPreservesExtendedFields ensures Subscription captures metadata new consumers rely on.
func TestSubscriptionUnmarshalPreservesExtendedFields(t *testing.T) {
	payload := `{
		"_id": "sub-123",
		"consumer_id": "customer-111",
		"customer_id": "customer-111",
		"dataset_id": "dataset-42",
		"dataset_name": "Daily Feed",
		"producer_id": "company-999",
		"request_id": "req-101",
		"tier": "premium",
		"status": "active",
		"kms_grant_id": "grant-abc",
		"sqs_queue_url": "https://sqs.us-east-1.amazonaws.com/123/consumer-queue",
		"sqs_queue_arn": "arn:aws:sqs:us-east-1:123:consumer-queue",
		"sns_subscription_arn": "arn:aws:sns:us-east-1:123:topic:abc",
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-02T00:00:00Z"
	}`

	var sub Subscription
	if err := json.Unmarshal([]byte(payload), &sub); err != nil {
		t.Fatalf("failed to unmarshal subscription: %v", err)
	}

	if sub.ID != "sub-123" {
		t.Fatalf("unexpected id: %s", sub.ID)
	}

	if sub.DatasetID == nil || *sub.DatasetID != "dataset-42" {
		t.Fatalf("dataset_id not captured: %+v", sub.DatasetID)
	}

	if sub.DatasetName != "Daily Feed" {
		t.Fatalf("unexpected dataset name: %s", sub.DatasetName)
	}

	if sub.Tier != "premium" {
		t.Fatalf("unexpected tier: %s", sub.Tier)
	}

	if sub.KMSGrantID == nil || *sub.KMSGrantID != "grant-abc" {
		t.Fatalf("kms_grant_id not captured: %+v", sub.KMSGrantID)
	}

	if sub.SQSQueueURL == nil || *sub.SQSQueueURL == "" {
		t.Fatalf("missing sqs queue url: %+v", sub.SQSQueueURL)
	}

	if sub.CreatedAt != "2024-01-01T00:00:00Z" {
		t.Fatalf("unexpected created_at: %s", sub.CreatedAt)
	}

	if sub.UpdatedAt != "2024-01-02T00:00:00Z" {
		t.Fatalf("unexpected updated_at: %s", sub.UpdatedAt)
	}
}
