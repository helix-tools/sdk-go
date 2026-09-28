package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// subscriptionRequestsPageJSON renders one page of the API's paginated
// GET /v1/subscription-requests response shape — {requests, total_count,
// count, page, limit, total_pages} — field names copied from PLAN-FINAL's
// "Final API contract" section.
func subscriptionRequestsPageJSON(ids []string, page, totalPages int) string {
	items := ""
	for i, id := range ids {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(`{"_id":%q,"request_id":%q,"consumer_id":"test-consumer","producer_id":"producer-1","tier":"free","status":"pending","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`, id, id)
	}
	return fmt.Sprintf(`{"requests":[%s],"total_count":%d,"count":%d,"page":%d,"limit":100,"total_pages":%d}`,
		items, len(ids), len(ids), page, totalPages)
}

// TestListSubscriptionRequests_FollowsThreePages_InOrder is acceptance
// criterion 1: 250 rows across 3 pages of 100 must all come back, in order,
// via exactly 3 requests each carrying page and limit=100, with no status
// key (the consumer default: empty status means unfiltered, not "pending").
func TestListSubscriptionRequests_FollowsThreePages_InOrder(t *testing.T) {
	var all []string
	for i := 0; i < 250; i++ {
		all = append(all, fmt.Sprintf("req-%03d", i))
	}
	pages := [][]string{all[0:100], all[100:200], all[200:250]}

	var mu sync.Mutex
	var requestedPages []int
	var requestedLimits []string
	var sawStatusKey bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query param: %v", err)
		}

		mu.Lock()
		requestedPages = append(requestedPages, page)
		requestedLimits = append(requestedLimits, r.URL.Query().Get("limit"))
		if r.URL.Query().Has("status") {
			sawStatusKey = true
		}
		mu.Unlock()

		if page < 1 || page > len(pages) {
			t.Fatalf("unexpected request for page %d (only %d pages exist)", page, len(pages))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(subscriptionRequestsPageJSON(pages[page-1], page, len(pages))))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	reqs, err := c.ListSubscriptionRequests(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}
	if len(reqs) != 250 {
		t.Fatalf("len(reqs) = %d, want 250", len(reqs))
	}
	for i, want := range all {
		if reqs[i].RequestID != want {
			t.Fatalf("reqs[%d].RequestID = %q, want %q (order broken)", i, reqs[i].RequestID, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []int{1, 2, 3}; !reflect.DeepEqual(requestedPages, want) {
		t.Errorf("requested pages = %v, want %v", requestedPages, want)
	}
	for i, limit := range requestedLimits {
		if limit != "100" {
			t.Errorf("request %d: limit = %q, want \"100\"", i, limit)
		}
	}
	if sawStatusKey {
		t.Errorf("a status query key was sent for an empty status filter, want none")
	}
}

// TestListSubscriptionRequests_StatusFilterSentOnEveryPage pins that a
// non-empty status filter is carried on every paged request, not just the
// first.
func TestListSubscriptionRequests_StatusFilterSentOnEveryPage(t *testing.T) {
	pages := [][]string{{"req-1"}, {"req-2"}}
	var mu sync.Mutex
	var requestedStatuses []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query param: %v", err)
		}

		mu.Lock()
		requestedStatuses = append(requestedStatuses, r.URL.Query().Get("status"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(subscriptionRequestsPageJSON(pages[page-1], page, len(pages))))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.ListSubscriptionRequests(context.Background(), "approved"); err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"approved", "approved"}; !reflect.DeepEqual(requestedStatuses, want) {
		t.Errorf("requested statuses = %v, want %v", requestedStatuses, want)
	}
}

// TestListSubscriptionRequests_LegacyServer_SingleGet is acceptance
// criterion 2: an old API build that doesn't understand ?page/?limit and
// answers with today's exact legacy body ({requests,count}, no page/
// total_pages) must be trusted for exactly what it sent — one GET, every
// row it returned.
func TestListSubscriptionRequests_LegacyServer_SingleGet(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := `{"requests":[` +
			`{"_id":"id-1","request_id":"req-1","consumer_id":"c","producer_id":"p","tier":"free","status":"pending","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},` +
			`{"_id":"id-2","request_id":"req-2","consumer_id":"c","producer_id":"p","tier":"free","status":"pending","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},` +
			`{"_id":"id-3","request_id":"req-3","consumer_id":"c","producer_id":"p","tier":"free","status":"pending","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},` +
			`{"_id":"id-4","request_id":"req-4","consumer_id":"c","producer_id":"p","tier":"free","status":"pending","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},` +
			`{"_id":"id-5","request_id":"req-5","consumer_id":"c","producer_id":"p","tier":"free","status":"pending","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}` +
			`],"count":5}`
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	reqs, err := c.ListSubscriptionRequests(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}
	if len(reqs) != 5 {
		t.Fatalf("len(reqs) = %d, want 5", len(reqs))
	}
	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Errorf("request count = %d, want exactly 1 (legacy body must not trigger a second page request)", got)
	}
}

// TestListSubscriptionRequests_ServerIgnoresPageParam_ReturnsError is
// acceptance criterion 3: a server that claims multiple pages exist
// (total_pages=3) but always echoes page=1 must not be trusted to be
// advancing.
func TestListSubscriptionRequests_ServerIgnoresPageParam_ReturnsError(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(subscriptionRequestsPageJSON([]string{"req-a"}, 1, 3)))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	reqs, err := c.ListSubscriptionRequests(context.Background(), "")
	if err == nil {
		t.Fatalf("ListSubscriptionRequests unexpectedly succeeded against a server that ignores ?page; reqs = %+v", reqs)
	}
	if len(reqs) != 0 {
		t.Errorf("reqs = %+v, want no duplicated items on error", reqs)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the mismatch)", got)
	}
}

// TestListSubscriptionRequests_EmptyResult_ReturnsEmptyNotNilSlice is
// acceptance criterion 4: a successful empty list must marshal to JSON
// `[]`, not `null`.
func TestListSubscriptionRequests_EmptyResult_ReturnsEmptyNotNilSlice(t *testing.T) {
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"requests":[],"total_count":0,"count":0,"page":1,"limit":100,"total_pages":0}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	reqs, err := c.ListSubscriptionRequests(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}
	if reqs == nil {
		t.Fatal("ListSubscriptionRequests returned a nil slice for an empty result, want a non-nil empty slice")
	}

	got, err := json.Marshal(reqs)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("json.Marshal(reqs) = %s, want []", got)
	}
	if n := atomic.LoadInt32(&requestCount); n != 1 {
		t.Errorf("request count = %d, want exactly 1", n)
	}
}

// TestListSubscriptionRequests_StatusValueWithAmpersand_EncodedAsLiteral is
// the bypass test named in PLAN-FINAL: a status value that itself looks
// like extra query params ("pending&page=9") must be percent-encoded into
// one literal status value, never interpreted by the server as injecting
// its own page=9 and overriding the page=1 the client actually requested.
func TestListSubscriptionRequests_StatusValueWithAmpersand_EncodedAsLiteral(t *testing.T) {
	const trickyStatus = "pending&page=9"

	var gotStatus, gotPage string
	var requestCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		gotStatus = r.URL.Query().Get("status")
		gotPage = r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(subscriptionRequestsPageJSON(nil, 1, 1)))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)

	if _, err := c.ListSubscriptionRequests(context.Background(), trickyStatus); err != nil {
		t.Fatalf("ListSubscriptionRequests: %v", err)
	}
	if gotStatus != trickyStatus {
		t.Errorf("status query = %q, want the literal value %q", gotStatus, trickyStatus)
	}
	if gotPage != "1" {
		t.Errorf("page query = %q, want \"1\" (must not be overridden by page=9 embedded in the status value)", gotPage)
	}
	if n := atomic.LoadInt32(&requestCount); n != 1 {
		t.Errorf("request count = %d, want exactly 1", n)
	}
}
