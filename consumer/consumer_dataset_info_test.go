package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Subscription.dataset_info is decoded on the SDK's real path,
// Consumer.ListSubscriptions (GET /v1/subscriptions), not just by a bare
// json.Unmarshal of types.Subscription. These tests drive that method against
// an httptest server (real SigV4 signing + the real response decode).

const subscriptionsWithDatasetInfoBody = `{
	"subscriptions": [
		{
			"_id": "sub-1",
			"consumer_id": "cons-1",
			"dataset_id": "ds-1",
			"producer_id": "prod-1",
			"tier": "free",
			"status": "active",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z",
			"dataset_info": {
				"name": "Phone Feed",
				"last_updated": "2026-09-01T12:00:00Z",
				"updated_at": "2026-09-01T12:00:05Z",
				"record_count": 1604854,
				"size_bytes": 987654321
			}
		}
	],
	"count": 1
}`

const subscriptionsWithoutDatasetInfoBody = `{
	"subscriptions": [
		{
			"_id": "sub-2",
			"consumer_id": "cons-1",
			"dataset_id": "ds-2",
			"producer_id": "prod-1",
			"tier": "free",
			"status": "active",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z"
		}
	],
	"count": 1
}`

// fractionalRecordCountBody carries a non-integer record_count, which the
// int64 field must reject rather than silently truncate to a wrong count.
const fractionalRecordCountBody = `{
	"subscriptions": [
		{
			"_id": "sub-3",
			"consumer_id": "cons-1",
			"dataset_id": "ds-3",
			"producer_id": "prod-1",
			"tier": "free",
			"status": "active",
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z",
			"dataset_info": {"record_count": 1604854.5}
		}
	],
	"count": 1
}`

func newSubscriptionsServer(t *testing.T, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/subscriptions" {
			t.Errorf("unexpected request %s %s, want GET /v1/subscriptions", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return server
}

func TestListSubscriptions_DatasetInfoPresent(t *testing.T) {
	server := newSubscriptionsServer(t, subscriptionsWithDatasetInfoBody)

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("len(subs) = %d, want 1", len(subs))
	}

	di := subs[0].DatasetInfo
	if di == nil {
		t.Fatal("DatasetInfo = nil, want populated struct")
	}
	if di.Name != "Phone Feed" {
		t.Errorf("Name = %q, want %q", di.Name, "Phone Feed")
	}
	if di.LastUpdated != "2026-09-01T12:00:00Z" {
		t.Errorf("LastUpdated = %q, want %q", di.LastUpdated, "2026-09-01T12:00:00Z")
	}
	if di.UpdatedAt != "2026-09-01T12:00:05Z" {
		t.Errorf("UpdatedAt = %q, want %q", di.UpdatedAt, "2026-09-01T12:00:05Z")
	}
	if di.RecordCount != 1604854 {
		t.Errorf("RecordCount = %d, want 1604854", di.RecordCount)
	}
	if di.SizeBytes != 987654321 {
		t.Errorf("SizeBytes = %d, want 987654321", di.SizeBytes)
	}
}

func TestListSubscriptions_DatasetInfoAbsent(t *testing.T) {
	server := newSubscriptionsServer(t, subscriptionsWithoutDatasetInfoBody)

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("len(subs) = %d, want 1", len(subs))
	}
	if subs[0].DatasetInfo != nil {
		t.Errorf("DatasetInfo = %+v, want nil when the API omits dataset_info", subs[0].DatasetInfo)
	}
}

func TestListSubscriptions_DatasetInfoFractionalRecordCountErrors(t *testing.T) {
	server := newSubscriptionsServer(t, fractionalRecordCountBody)

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err == nil {
		t.Fatalf("ListSubscriptions accepted record_count 1604854.5 (subs = %+v), want a decode error", subs)
	}
	if subs != nil {
		t.Errorf("subs = %+v alongside error, want nil", subs)
	}
}
