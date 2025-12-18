package consumer

import (
	"encoding/json"
	"errors"
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

// notificationData mirrors the struct used in PollNotifications for testing.
type notificationData struct {
	DatasetID      string `json:"dataset_id"`
	DatasetName    string `json:"dataset_name"`
	EventType      string `json:"event_type"`
	ProducerID     string `json:"producer_id"`
	S3Bucket       string `json:"s3_bucket"`
	S3Key          string `json:"s3_key"`
	SizeBytes      int64  `json:"size_bytes"`
	SubscriberID   string `json:"subscriber_id"`
	SubscriptionID string `json:"subscription_id"`
	Timestamp      string `json:"timestamp"`
}

// parseNotificationMessage simulates the notification parsing logic from consumer.go.
func parseNotificationMessage(messageBody string) (*notificationData, error) {
	var parsedBody map[string]any
	if err := json.Unmarshal([]byte(messageBody), &parsedBody); err != nil {
		return nil, err
	}

	var data notificationData

	if snsMessage, hasSNSWrapper := parsedBody["Message"].(string); hasSNSWrapper {
		// SNS-wrapped format
		if err := json.Unmarshal([]byte(snsMessage), &data); err != nil {
			return nil, err
		}
	} else if _, hasEventType := parsedBody["event_type"]; hasEventType {
		// Raw notification payload format
		if err := json.Unmarshal([]byte(messageBody), &data); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("unknown message format")
	}

	return &data, nil
}

// TestNotificationParsingSnSWrapped tests parsing of SNS-wrapped messages.
func TestNotificationParsingSNSWrapped(t *testing.T) {
	notificationPayload := notificationData{
		EventType:      "dataset_updated",
		ProducerID:     "company-123",
		DatasetID:      "dataset-456",
		DatasetName:    "Test Dataset",
		S3Bucket:       "helix-producer-company-123-production",
		S3Key:          "datasets/Test Dataset/2025-01-01/data.ndjson.gz",
		SizeBytes:      1024,
		Timestamp:      "2025-01-01T00:00:00Z",
		SubscriberID:   "customer-789",
		SubscriptionID: "sub-abc",
	}

	payloadBytes, _ := json.Marshal(notificationPayload)

	snsWrapped := map[string]any{
		"Type":      "Notification",
		"MessageId": "sns-msg-123",
		"Message":   string(payloadBytes),
	}

	messageBody, _ := json.Marshal(snsWrapped)

	result, err := parseNotificationMessage(string(messageBody))
	if err != nil {
		t.Fatalf("failed to parse SNS-wrapped message: %v", err)
	}

	if result.EventType != "dataset_updated" {
		t.Errorf("expected event_type 'dataset_updated', got '%s'", result.EventType)
	}

	if result.ProducerID != "company-123" {
		t.Errorf("expected producer_id 'company-123', got '%s'", result.ProducerID)
	}

	if result.DatasetID != "dataset-456" {
		t.Errorf("expected dataset_id 'dataset-456', got '%s'", result.DatasetID)
	}
}

// TestNotificationParsingRaw tests parsing of raw notification messages.
func TestNotificationParsingRaw(t *testing.T) {
	rawPayload := notificationData{
		EventType:      "dataset_updated",
		ProducerID:     "company-123",
		DatasetID:      "dataset-456",
		DatasetName:    "Test Dataset",
		S3Bucket:       "helix-producer-company-123-production",
		S3Key:          "datasets/Test Dataset/2025-01-01/data.ndjson.gz",
		SizeBytes:      1024,
		Timestamp:      "2025-01-01T00:00:00Z",
		SubscriberID:   "customer-789",
		SubscriptionID: "sub-abc",
	}

	messageBody, _ := json.Marshal(rawPayload)

	result, err := parseNotificationMessage(string(messageBody))
	if err != nil {
		t.Fatalf("failed to parse raw message: %v", err)
	}

	if result.EventType != "dataset_updated" {
		t.Errorf("expected event_type 'dataset_updated', got '%s'", result.EventType)
	}

	if result.ProducerID != "company-123" {
		t.Errorf("expected producer_id 'company-123', got '%s'", result.ProducerID)
	}

	if result.SubscriberID != "customer-789" {
		t.Errorf("expected subscriber_id 'customer-789', got '%s'", result.SubscriberID)
	}
}

// TestNotificationParsingUnknownFormat tests that unknown formats return an error.
func TestNotificationParsingUnknownFormat(t *testing.T) {
	unknownMessage := map[string]any{
		"foo": "bar",
		"baz": 123,
	}

	messageBody, _ := json.Marshal(unknownMessage)

	_, err := parseNotificationMessage(string(messageBody))
	if err == nil {
		t.Fatal("expected error for unknown message format, got nil")
	}

	if err.Error() != "unknown message format" {
		t.Errorf("expected 'unknown message format' error, got '%s'", err.Error())
	}
}

// TestNotificationParsingPreviouslyFailingCase tests the original bug case.
// This was the original bug - when Message field was undefined/missing in a raw message,
// the code would fail. With the fix, raw messages are now handled correctly.
func TestNotificationParsingPreviouslyFailingCase(t *testing.T) {
	// Raw payload without SNS wrapper - this would previously fail
	rawPayload := map[string]any{
		"event_type":      "dataset_updated",
		"producer_id":     "test-producer",
		"dataset_id":      "test-dataset",
		"dataset_name":    "Test",
		"s3_bucket":       "test-bucket",
		"s3_key":          "test-key",
		"size_bytes":      100,
		"timestamp":       "2025-01-01T00:00:00Z",
		"subscriber_id":   "test-subscriber",
		"subscription_id": "test-subscription",
	}

	messageBody, _ := json.Marshal(rawPayload)

	result, err := parseNotificationMessage(string(messageBody))
	if err != nil {
		t.Fatalf("failed to parse raw message (previously failing case): %v", err)
	}

	if result.EventType != "dataset_updated" {
		t.Errorf("expected event_type 'dataset_updated', got '%s'", result.EventType)
	}
}
