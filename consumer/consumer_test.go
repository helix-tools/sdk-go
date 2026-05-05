package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestDefaultHTTPClientTimeoutIsBounded locks in the defense-in-depth
// contract introduced in v2.1.1: the Consumer's httpClient must carry a
// non-zero Timeout so any HTTP call (getDataset, getDownloadUrl, S3
// download, outcome callback) has a client-level safety net even if a
// future code path omits a per-call context deadline.
//
// We can't exercise NewConsumer directly here because it calls AWS STS,
// but we can pin the constant the constructor uses and verify that an
// http.Client built from it has the expected non-zero Timeout.
func TestDefaultHTTPClientTimeoutIsBounded(t *testing.T) {
	if defaultHTTPClientTimeout <= 0 {
		t.Fatalf("defaultHTTPClientTimeout must be > 0, got %v", defaultHTTPClientTimeout)
	}

	// Pin the documented value: longer than the 5s per-call ctx budget
	// for the outcome callback so per-call deadlines still win.
	if defaultHTTPClientTimeout != 10*time.Second {
		t.Fatalf("defaultHTTPClientTimeout drifted from 10s, got %v — "+
			"update the comment in consumer.go if this is intentional",
			defaultHTTPClientTimeout)
	}

	client := &http.Client{Timeout: defaultHTTPClientTimeout}
	if client.Timeout == 0 {
		t.Fatalf("http.Client built from defaultHTTPClientTimeout has zero Timeout")
	}
}

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

// TestCreateSubscriptionRequestPayloadMarshal tests that the payload is correctly marshaled.
func TestCreateSubscriptionRequestPayloadMarshal(t *testing.T) {
	datasetID := "dataset-123"
	message := "Please grant me access"

	payload := types.CreateSubscriptionRequestPayload{
		ProducerID: "company-456",
		DatasetID:  &datasetID,
		Tier:       "premium",
		Message:    &message,
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["producer_id"] != "company-456" {
		t.Errorf("expected producer_id 'company-456', got '%v'", parsed["producer_id"])
	}

	if parsed["dataset_id"] != "dataset-123" {
		t.Errorf("expected dataset_id 'dataset-123', got '%v'", parsed["dataset_id"])
	}

	if parsed["tier"] != "premium" {
		t.Errorf("expected tier 'premium', got '%v'", parsed["tier"])
	}

	if parsed["message"] != "Please grant me access" {
		t.Errorf("expected message 'Please grant me access', got '%v'", parsed["message"])
	}
}

// TestCreateSubscriptionRequestPayloadWithoutOptional tests payload without optional fields.
// "free" is the canonical tier; the SDK defaults empty Tier to "free" before sending.
func TestCreateSubscriptionRequestPayloadWithoutOptional(t *testing.T) {
	payload := types.CreateSubscriptionRequestPayload{
		ProducerID: "company-456",
		Tier:       "free",
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["producer_id"] != "company-456" {
		t.Errorf("expected producer_id 'company-456', got '%v'", parsed["producer_id"])
	}

	if parsed["tier"] != "free" {
		t.Errorf("expected tier 'free', got '%v'", parsed["tier"])
	}

	// Optional fields should be omitted when nil
	if _, exists := parsed["dataset_id"]; exists {
		t.Errorf("dataset_id should be omitted when nil")
	}

	if _, exists := parsed["message"]; exists {
		t.Errorf("message should be omitted when nil")
	}
}

// TestCreateSubscriptionRequest_DefaultTierIsFree exercises the default-tier
// branch in Consumer.CreateSubscriptionRequest end-to-end against a stub HTTP
// server. When the caller does NOT set Tier, the SDK must send tier="free"
// (NOT "basic" — see helix-api v1.8.2 which now rejects "basic" with 400).
func TestCreateSubscriptionRequest_DefaultTierIsFree(t *testing.T) {
	var capturedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"_id": "req-x",
			"request_id": "req-x",
			"consumer_id": "consumer-1",
			"producer_id": "producer-1",
			"tier": "free",
			"status": "pending",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z"
		}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	// No Tier set → SDK should default it to "free".
	result, err := c.CreateSubscriptionRequest(context.Background(), types.CreateSubscriptionRequestInput{
		ProducerID: "producer-1",
	})
	if err != nil {
		t.Fatalf("CreateSubscriptionRequest: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}

	tier, ok := capturedBody["tier"].(string)
	if !ok {
		t.Fatalf("tier field missing or not string in payload: %#v", capturedBody)
	}
	if tier != "free" {
		t.Errorf("expected outgoing tier=\"free\", got %q (helix-api v1.8.2 rejects non-canonical tiers with 400)", tier)
	}
}

// TestCreateSubscriptionRequest_Accepts201Created pins makeAPIRequest's
// 2xx-success contract. The helix API returns 201 Created on a successful
// POST /v1/subscription-requests; before v2.1.3, makeAPIRequest only
// accepted 200, so every real production call surfaced a fake "API request
// failed: 201" error to the caller. Any regression that narrows the success
// range back to a single status code will fail this test.
func TestCreateSubscriptionRequest_Accepts201Created(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"_id": "req-201",
			"request_id": "req-201",
			"consumer_id": "consumer-1",
			"producer_id": "producer-1",
			"tier": "free",
			"status": "pending",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z"
		}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	result, err := c.CreateSubscriptionRequest(context.Background(), types.CreateSubscriptionRequestInput{
		ProducerID: "producer-1",
	})
	if err != nil {
		t.Fatalf("CreateSubscriptionRequest returned error on 201 response: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if result.RequestID != "req-201" {
		t.Errorf("expected RequestID \"req-201\", got %q", result.RequestID)
	}
	if result.Status != "pending" {
		t.Errorf("expected Status \"pending\", got %q", result.Status)
	}
}

// TestMakeAPIRequest_StatusCodeContract pins makeAPIRequest's "any 2xx is
// success, everything else is an error" contract. The bug fixed in v2.1.3
// was a `resp.StatusCode != 200` check that rejected 201 Created responses
// from the helix API on POST /v1/subscription-requests. The fix widens the
// success window to the full 2xx range, matching the HTTP RFC and the
// pattern used in producer.go and api/client.go.
//
// Tests cover happy (200, 201, 204), bad (400, 401, 500), and edge (199,
// 300) status codes. Each case drives CreateSubscriptionRequest end-to-end
// against an httptest server that returns the desired status, asserting
// that 2xx values reach the success path and non-2xx values surface as
// "API request failed: %d - …" errors with the status code preserved.
//
// CreateSubscriptionRequest is the exported wrapper we use because
// makeAPIRequest is package-private; its single makeAPIRequest call site
// exercises the full request → status-code → decode pipeline.
func TestMakeAPIRequest_StatusCodeContract(t *testing.T) {
	// successBody is a minimally valid SubscriptionRequest payload — enough
	// for json.Decoder to populate result without erroring. Used for every
	// 2xx case so the success-path decode step also runs.
	const successBody = `{
		"_id": "req-x",
		"request_id": "req-x",
		"consumer_id": "consumer-1",
		"producer_id": "producer-1",
		"tier": "free",
		"status": "pending",
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z"
	}`

	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		wantInBody string // substring expected in error message (for !nil err only)
	}{
		// Happy: 2xx success path.
		{name: "happy_200_OK", status: http.StatusOK, body: successBody, wantErr: false},
		{name: "happy_201_Created", status: http.StatusCreated, body: successBody, wantErr: false},
		// 204 No Content: no body. CreateSubscriptionRequest passes a non-nil
		// result pointer, so an empty body returns io.EOF from json.Decode.
		// In production 204 is used by DELETE-style endpoints (see
		// CancelSubscription) where result is nil and the EOF path is
		// skipped — that's the realistic shape, exercised by
		// happy_204_NoContent_NoResult below.
		{name: "happy_204_NoContent_NoResult", status: http.StatusNoContent, body: "", wantErr: false},
		// Bad: 4xx/5xx error path.
		{name: "bad_400_BadRequest", status: http.StatusBadRequest, body: `{"error":"validation failed"}`, wantErr: true, wantInBody: "400"},
		{name: "bad_401_Unauthorized", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantErr: true, wantInBody: "401"},
		{name: "bad_500_InternalServerError", status: http.StatusInternalServerError, body: "internal error", wantErr: true, wantInBody: "500"},
		// Edge: 1xx and 3xx are NOT success per the 2xx contract.
		// - 199 is below the 2xx range. Go's net/http transport treats 1xx
		//   responses as informational and waits for a final response,
		//   producing a transport-level error (EOF) rather than reaching
		//   our status-code check. Either way the call fails, so we only
		//   assert wantErr=true without pinning the exact message — the
		//   contract is "non-2xx must NOT reach the success path".
		// - 300 is a redirect; the SDK does NOT follow because AWS SigV4
		//   binds the signature to the original URL (silently following
		//   would re-sign or fail). Reaches our status check normally.
		{name: "edge_199_below_2xx", status: 199, body: "", wantErr: true},
		{name: "edge_300_MultipleChoices", status: http.StatusMultipleChoices, body: "", wantErr: true, wantInBody: "300"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer server.Close()

			c := newTestConsumer(server.URL)

			// happy_204_NoContent_NoResult uses CancelSubscription because
			// it passes result=nil to makeAPIRequest, matching the realistic
			// 204 use case (DELETE endpoints with empty body).
			var err error
			if tc.name == "happy_204_NoContent_NoResult" {
				err = c.CancelSubscription(context.Background(), "sub-1")
			} else {
				_, err = c.CreateSubscriptionRequest(context.Background(), types.CreateSubscriptionRequestInput{
					ProducerID: "producer-1",
				})
			}

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for status %d, got nil", tc.status)
				}
				// Confirm the original status code surfaces in the error
				// message — callers rely on it to distinguish 400 vs 500.
				if tc.wantInBody != "" && !strings.Contains(err.Error(), tc.wantInBody) {
					t.Errorf("error message %q missing expected substring %q (status code should surface to callers)",
						err.Error(), tc.wantInBody)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected success for status %d, got error: %v", tc.status, err)
			}
		})
	}
}

// TestMakeAPIRequest_MalformedBodyOn201 verifies the body-parse contract
// is independent of the status-code contract. A 201 Created response is
// success per HTTP semantics — even if the body is unparseable JSON, the
// request itself succeeded server-side (the resource was created in
// Mongo). The SDK surfaces the parse error to the caller because
// CreateSubscriptionRequest's API contract returns *SubscriptionRequest,
// but the failure mode is "malformed response body" not "request failed".
//
// This test pins the boundary: status code 201 → past the status check
// (no "API request failed" error), then json.Decode fails → that decode
// error is what the caller sees. A regression that conflates the two
// (e.g. swallowing parse errors on 2xx) would break this test.
func TestMakeAPIRequest_MalformedBodyOn201(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// Truncated/invalid JSON — opens an object but never closes it.
		_, _ = w.Write([]byte(`{"request_id": "req-x"`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)
	_, err := c.CreateSubscriptionRequest(context.Background(), types.CreateSubscriptionRequestInput{
		ProducerID: "producer-1",
	})
	if err == nil {
		t.Fatal("expected decode error on malformed 201 body, got nil")
	}
	// Must NOT be the status-code-rejection error — the request DID
	// succeed; only the body parse failed.
	if strings.Contains(err.Error(), "API request failed") {
		t.Errorf("malformed 201 body should produce a decode error, not a status-code error: %v", err)
	}
}

// TestSubscriptionRequestUnmarshal tests unmarshaling a subscription request response.
func TestSubscriptionRequestUnmarshal(t *testing.T) {
	responseJSON := `{
		"_id": "req-123",
		"request_id": "req-123",
		"consumer_id": "customer-111",
		"consumer_name": "Test Consumer",
		"consumer_email": "test@example.com",
		"producer_id": "company-456",
		"producer_name": "Test Producer",
		"dataset_id": "dataset-789",
		"tier": "premium",
		"message": "Please grant access",
		"status": "pending",
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-02T00:00:00Z"
	}`

	var request types.SubscriptionRequest
	if err := json.Unmarshal([]byte(responseJSON), &request); err != nil {
		t.Fatalf("failed to unmarshal subscription request: %v", err)
	}

	if request.ID != "req-123" {
		t.Errorf("expected ID 'req-123', got '%s'", request.ID)
	}

	if request.RequestID != "req-123" {
		t.Errorf("expected RequestID 'req-123', got '%s'", request.RequestID)
	}

	if request.ConsumerID != "customer-111" {
		t.Errorf("expected ConsumerID 'customer-111', got '%s'", request.ConsumerID)
	}

	if request.ProducerID != "company-456" {
		t.Errorf("expected ProducerID 'company-456', got '%s'", request.ProducerID)
	}

	if request.DatasetID == nil || *request.DatasetID != "dataset-789" {
		t.Errorf("expected DatasetID 'dataset-789', got '%v'", request.DatasetID)
	}

	if request.Tier != "premium" {
		t.Errorf("expected Tier 'premium', got '%s'", request.Tier)
	}

	if request.Message == nil || *request.Message != "Please grant access" {
		t.Errorf("expected Message 'Please grant access', got '%v'", request.Message)
	}

	if request.Status != "pending" {
		t.Errorf("expected Status 'pending', got '%s'", request.Status)
	}
}

// TestSubscriptionRequestUnmarshalApproved tests unmarshaling an approved request.
func TestSubscriptionRequestUnmarshalApproved(t *testing.T) {
	responseJSON := `{
		"_id": "req-123",
		"request_id": "req-123",
		"consumer_id": "customer-111",
		"producer_id": "company-456",
		"tier": "basic",
		"status": "approved",
		"approved_at": "2024-01-03T00:00:00Z",
		"approved_by": "admin-user",
		"subscription_id": "sub-456",
		"notes": "Approved for Q1 trial",
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-03T00:00:00Z"
	}`

	var request types.SubscriptionRequest
	if err := json.Unmarshal([]byte(responseJSON), &request); err != nil {
		t.Fatalf("failed to unmarshal approved subscription request: %v", err)
	}

	if request.Status != "approved" {
		t.Errorf("expected Status 'approved', got '%s'", request.Status)
	}

	if request.ApprovedAt == nil || *request.ApprovedAt != "2024-01-03T00:00:00Z" {
		t.Errorf("expected ApprovedAt '2024-01-03T00:00:00Z', got '%v'", request.ApprovedAt)
	}

	if request.ApprovedBy == nil || *request.ApprovedBy != "admin-user" {
		t.Errorf("expected ApprovedBy 'admin-user', got '%v'", request.ApprovedBy)
	}

	if request.SubscriptionID == nil || *request.SubscriptionID != "sub-456" {
		t.Errorf("expected SubscriptionID 'sub-456', got '%v'", request.SubscriptionID)
	}

	if request.Notes == nil || *request.Notes != "Approved for Q1 trial" {
		t.Errorf("expected Notes 'Approved for Q1 trial', got '%v'", request.Notes)
	}
}

// TestSubscriptionRequestUnmarshalRejected tests unmarshaling a rejected request.
func TestSubscriptionRequestUnmarshalRejected(t *testing.T) {
	responseJSON := `{
		"_id": "req-123",
		"request_id": "req-123",
		"consumer_id": "customer-111",
		"producer_id": "company-456",
		"tier": "enterprise",
		"status": "rejected",
		"rejected_at": "2024-01-03T00:00:00Z",
		"rejection_reason": "Insufficient credentials",
		"created_at": "2024-01-01T00:00:00Z",
		"updated_at": "2024-01-03T00:00:00Z"
	}`

	var request types.SubscriptionRequest
	if err := json.Unmarshal([]byte(responseJSON), &request); err != nil {
		t.Fatalf("failed to unmarshal rejected subscription request: %v", err)
	}

	if request.Status != "rejected" {
		t.Errorf("expected Status 'rejected', got '%s'", request.Status)
	}

	if request.RejectedAt == nil || *request.RejectedAt != "2024-01-03T00:00:00Z" {
		t.Errorf("expected RejectedAt '2024-01-03T00:00:00Z', got '%v'", request.RejectedAt)
	}

	if request.RejectionReason == nil || *request.RejectionReason != "Insufficient credentials" {
		t.Errorf("expected RejectionReason 'Insufficient credentials', got '%v'", request.RejectionReason)
	}
}

// TestListDatasetsPathBuilding tests that ListDatasets builds the correct API path.
func TestListDatasetsPathBuilding(t *testing.T) {
	tests := []struct {
		name       string
		producerID []string
		wantPath   string
	}{
		{
			name:       "no producer ID",
			producerID: nil,
			wantPath:   "/v1/datasets",
		},
		{
			name:       "empty variadic",
			producerID: []string{},
			wantPath:   "/v1/datasets",
		},
		{
			name:       "empty string producer ID",
			producerID: []string{""},
			wantPath:   "/v1/datasets",
		},
		{
			name:       "with producer ID",
			producerID: []string{"company-123456"},
			wantPath:   "/v1/datasets?producer_id=company-123456",
		},
		{
			name:       "producer ID with special chars",
			producerID: []string{"company-123&test=value"},
			wantPath:   "/v1/datasets?producer_id=company-123%26test%3Dvalue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build path using same logic as ListDatasets
			path := "/v1/datasets"
			if len(tt.producerID) > 0 && tt.producerID[0] != "" {
				path = "/v1/datasets?producer_id=" + url.QueryEscape(tt.producerID[0])
			}

			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

// TestCreateSubscriptionRequestInputConversion tests converting input to payload.
func TestCreateSubscriptionRequestInputConversion(t *testing.T) {
	datasetID := "dataset-abc"
	message := "Request access for analytics"

	input := types.CreateSubscriptionRequestInput{
		ProducerID: "company-789",
		DatasetID:  &datasetID,
		Tier:       "professional",
		Message:    &message,
	}

	payload := types.CreateSubscriptionRequestPayload{
		ProducerID: input.ProducerID,
		DatasetID:  input.DatasetID,
		Tier:       input.Tier,
		Message:    input.Message,
	}

	if payload.ProducerID != "company-789" {
		t.Errorf("expected ProducerID 'company-789', got '%s'", payload.ProducerID)
	}

	if payload.DatasetID == nil || *payload.DatasetID != "dataset-abc" {
		t.Errorf("expected DatasetID 'dataset-abc', got '%v'", payload.DatasetID)
	}

	if payload.Tier != "professional" {
		t.Errorf("expected Tier 'professional', got '%s'", payload.Tier)
	}

	if payload.Message == nil || *payload.Message != "Request access for analytics" {
		t.Errorf("expected Message 'Request access for analytics', got '%v'", payload.Message)
	}
}
