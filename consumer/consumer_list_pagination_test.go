package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The Go API's actual response shape for GET /v1/datasets and
// GET /v1/subscriptions is a paginated OBJECT — {datasets, total_count,
// page, limit, total_pages} / {subscriptions, total_count, count, page,
// limit, total_pages}. ListDatasets and ListSubscriptions already decoded
// into a local {items, count} struct (so they never hit the unmarshal
// error ListMyDatasets did), but they never followed total_pages, so a
// consumer/producer with more than the API's default page size (20) got a
// silently truncated result. These tests pin the pagination fix.

func datasetsPageJSON(names []string, page, totalPages int) string {
	items := ""
	for i, name := range names {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(`{"_id":"ds-%s","name":%q}`, name, name)
	}
	return fmt.Sprintf(`{"datasets":[%s],"total_count":%d,"page":%d,"limit":100,"total_pages":%d}`,
		items, len(names), page, totalPages)
}

// TestListDatasets_ExactAPIShape_ReturnsAllDatasets is acceptance
// criterion 1: a mock returning the API's exact JSON object shape must
// decode successfully and hand back every dataset.
func TestListDatasets_ExactAPIShape_ReturnsAllDatasets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(datasetsPageJSON([]string{"a", "b"}, 1, 1)))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(datasets) != 2 {
		t.Fatalf("len(datasets) = %d, want 2", len(datasets))
	}
	if datasets[0].Name != "a" || datasets[1].Name != "b" {
		t.Errorf("datasets = %+v, want names [a b]", datasets)
	}
}

// TestListDatasets_FollowsThreePages_InOrder is acceptance criterion 2:
// three pages must yield every item from every page, in order, via exactly
// three requests.
func TestListDatasets_FollowsThreePages_InOrder(t *testing.T) {
	pages := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		page := int(n)
		if page < 1 || page > len(pages) {
			t.Fatalf("unexpected request for page %d (only %d pages exist)", page, len(pages))
		}
		if got := r.URL.Query().Get("page"); got != fmt.Sprint(page) {
			t.Errorf("request %d: page query param = %q, want %q", page, got, fmt.Sprint(page))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(datasetsPageJSON(pages[page-1], page, len(pages))))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}

	want := []string{"a", "b", "c", "d", "e"}
	if len(datasets) != len(want) {
		t.Fatalf("len(datasets) = %d, want %d (%+v)", len(datasets), len(want), datasets)
	}
	for i, name := range want {
		if datasets[i].Name != name {
			t.Errorf("datasets[%d].Name = %q, want %q", i, datasets[i].Name, name)
		}
	}

	if got := atomic.LoadInt32(&requestCount); got != int32(len(pages)) {
		t.Errorf("request count = %d, want exactly %d", got, len(pages))
	}
}

// TestListDatasets_StopsOnEmptyPageDespiteHighTotalPages is acceptance
// criterion 3: a server that always claims total_pages=999 but starts
// returning empty pages must not be trusted past its real data.
func TestListDatasets_StopsOnEmptyPageDespiteHighTotalPages(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		page := int(n)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if page == 1 {
			_, _ = w.Write([]byte(datasetsPageJSON([]string{"a"}, page, 999)))
			return
		}
		_, _ = w.Write([]byte(datasetsPageJSON(nil, page, 999)))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(datasets) != 1 || datasets[0].Name != "a" {
		t.Fatalf("datasets = %+v, want exactly [a]", datasets)
	}

	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 non-empty, page 2 empty stops the loop)", got)
	}
}

// TestListDatasets_OldStructDecode_TruncatesAgainstRealAPIShape is the
// negative control for criterion 2: decoding a single page the way
// ListDatasets did before this fix (a local {datasets,count} struct with
// no total_pages follow-through) silently drops every dataset past page 1
// — it never errors, it just returns an incomplete list. That silent
// truncation, not a decode error, is why this endpoint needed a different
// negative control than ListMyDatasets' unmarshal failure.
func TestListDatasets_OldStructDecode_TruncatesAgainstRealAPIShape(t *testing.T) {
	// Page 1 of a 3-page result, exactly what the old ListDatasets would
	// have received and stopped at.
	body := datasetsPageJSON([]string{"a", "b"}, 1, 3)

	var oldShape struct {
		Datasets []Dataset `json:"datasets"`
		Count    int       `json:"count"`
	}
	if err := json.Unmarshal([]byte(body), &oldShape); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(oldShape.Datasets) != 2 {
		t.Fatalf("old single-page decode got %d datasets, want 2", len(oldShape.Datasets))
	}
	// The old code had no way to know 3 more datasets existed on pages 2
	// and 3 — that's the bug TestListDatasets_FollowsThreePages_InOrder
	// above now covers.
}

// TestListSubscriptions_FollowsAllPages pins that the second caller of
// paginateAll (GET /v1/subscriptions) is wired correctly: its own field
// names ("subscriptions", not "datasets") and its own item type must
// decode and paginate exactly like ListDatasets does.
func TestListSubscriptions_FollowsAllPages(t *testing.T) {
	pages := [][]string{{"sub-1", "sub-2"}, {"sub-3"}}
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		page := int(n)
		if page < 1 || page > len(pages) {
			t.Fatalf("unexpected request for page %d", page)
		}

		items := ""
		for i, id := range pages[page-1] {
			if i > 0 {
				items += ","
			}
			items += fmt.Sprintf(`{"_id":%q,"consumer_id":"test-customer","producer_id":"prod-1","tier":"free","status":"active"}`, id)
		}
		body := fmt.Sprintf(`{"subscriptions":[%s],"total_count":%d,"count":%d,"page":%d,"limit":100,"total_pages":%d}`,
			items, len(pages[page-1]), len(pages[page-1]), page, len(pages))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("len(subs) = %d, want 3 (%+v)", len(subs), subs)
	}
	if subs[0].ID != "sub-1" || subs[1].ID != "sub-2" || subs[2].ID != "sub-3" {
		t.Errorf("subs = %+v, want ids [sub-1 sub-2 sub-3] in order", subs)
	}
	if got := atomic.LoadInt32(&requestCount); got != int32(len(pages)) {
		t.Errorf("request count = %d, want exactly %d", got, len(pages))
	}
}
