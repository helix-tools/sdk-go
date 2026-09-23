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

// The Go API's actual response shape for GET /v1/datasets and
// GET /v1/subscriptions is a paginated OBJECT — {datasets, total_count,
// page, limit, total_pages} / {subscriptions, total_count, count, page,
// limit, total_pages}. ListDatasets and ListSubscriptions already decoded
// into a local {items, count} struct (so they never hit the unmarshal
// error ListMyDatasets did), but they never followed total_pages, so a
// consumer/producer with more than the API's default page size (20) got a
// silently truncated result. These tests pin the pagination fix.

// datasetsPageJSON renders one page of the API's exact ListDatasetsResponse
// shape. The dataset id field is "id" (internal/resources/datasets/
// types.go:90's DatasetResponse.ID, json:"id") — NOT "_id"; a fixture using
// "_id" would silently match this package's local Dataset.ID tag even
// though it doesn't match what the real API sends, concealing a decode bug
// behind a passing test.
func datasetsPageJSON(names []string, page, totalPages int) string {
	items := ""
	for i, name := range names {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(`{"id":"ds-%s","name":%q}`, name, name)
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
	if datasets[0].ID != "ds-a" || datasets[1].ID != "ds-b" {
		t.Errorf("datasets = %+v, want ids [ds-a ds-b]", datasets)
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
		if wantID := "ds-" + name; datasets[i].ID != wantID {
			t.Errorf("datasets[%d].ID = %q, want %q", i, datasets[i].ID, wantID)
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
	if len(datasets) != 1 || datasets[0].Name != "a" || datasets[0].ID != "ds-a" {
		t.Fatalf("datasets = %+v, want exactly [{ID: ds-a Name: a}]", datasets)
	}

	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 non-empty, page 2 empty stops the loop)", got)
	}
}

// TestListDatasets_EnvelopeDecodeRejectsBareArrayShape proves exactly one
// thing: ListDatasets' envelope-object decode target rejects a bare JSON
// array, exercised through the public method rather than a standalone
// json.Unmarshal. It is NOT a regression control for the pagination fix
// (following total_pages, the page-mismatch/page-omission guard) — do not
// cite it as covering that. Restoring the pre-fix decoder (which also
// targeted an envelope object, it just never read total_pages) still
// rejects this same bare-array fixture, so this test stays green even with
// the pagination-following logic reverted. The historical truncation bug
// this package actually had (silently stopping after page 1 because
// total_pages was never read) is covered, through the same public method,
// by TestListDatasets_FollowsThreePages_InOrder's exact-request-count
// assertion and TestListDatasets_StopsOnEmptyPageDespiteHighTotalPages; the
// page-mismatch/page-omission guard is covered by
// TestListDatasets_ServerIgnoresPageParam_ReturnsError and
// TestListDatasets_ServerOmitsPageWithMultiplePages_ReturnsError.
func TestListDatasets_EnvelopeDecodeRejectsBareArrayShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"ds-a","name":"a"}]`))
	}))
	defer server.Close()

	if _, err := newTestConsumer(server.URL).ListDatasets(context.Background()); err == nil {
		t.Fatal("ListDatasets unexpectedly succeeded against a bare-array response; the envelope-decode check no longer holds")
	}
}

// TestListSubscriptions_FollowsAllPages pins that the second caller of
// paginateAll (GET /v1/subscriptions) is wired correctly: its own field
// names ("subscriptions", not "datasets") and its own item type must
// decode and paginate exactly like ListDatasets does.
//
// The mock is keyed on the ?page query parameter the client actually sent
// (not a request counter), and the test asserts the exact sequence of
// requested pages (1,2,3) — a client that re-requested page 1 instead of
// advancing would get the same two items back forever and fail this
// assertion, rather than being silently satisfied by a counter that
// advances regardless of what the client asked for.
func TestListSubscriptions_FollowsAllPages(t *testing.T) {
	pages := [][]string{{"sub-1", "sub-2"}, {"sub-3"}, {"sub-4"}}

	var mu sync.Mutex
	var requestedPages []int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query param: %v", err)
		}

		mu.Lock()
		requestedPages = append(requestedPages, page)
		mu.Unlock()

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
	if len(subs) != 4 {
		t.Fatalf("len(subs) = %d, want 4 (%+v)", len(subs), subs)
	}
	if subs[0].ID != "sub-1" || subs[1].ID != "sub-2" || subs[2].ID != "sub-3" || subs[3].ID != "sub-4" {
		t.Errorf("subs = %+v, want ids [sub-1 sub-2 sub-3 sub-4] in order", subs)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []int{1, 2, 3}; !reflect.DeepEqual(requestedPages, want) {
		t.Errorf("requested pages = %v, want %v", requestedPages, want)
	}
}

// TestListDatasets_ServerIgnoresPageParam_ReturnsError is acceptance
// criterion 2 for ListDatasets: a server that ignores ?page and keeps
// answering with page=1 (total_pages=3) must not be trusted to be
// advancing — paginateAll must notice the response's page doesn't match
// the page it requested and stop with an error, instead of appending the
// same item three times.
func TestListDatasets_ServerIgnoresPageParam_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Always reports page=1, regardless of the requested ?page.
		_, _ = w.Write([]byte(datasetsPageJSON([]string{"a"}, 1, 3)))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err == nil {
		t.Fatalf("ListDatasets unexpectedly succeeded against a server that ignores ?page; datasets = %+v", datasets)
	}
	if len(datasets) != 0 {
		t.Errorf("datasets = %+v, want no duplicated items on error", datasets)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the mismatch)", got)
	}
}

// TestListSubscriptions_ServerIgnoresPageParam_ReturnsError is the same
// acceptance criterion 2 scenario for ListSubscriptions, pinning that the
// page-mismatch check applies to both paginateAll callers in this package,
// not just ListDatasets.
func TestListSubscriptions_ServerIgnoresPageParam_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		body := `{"subscriptions":[{"_id":"sub-1","consumer_id":"test-customer","producer_id":"prod-1","tier":"free","status":"active"}],"total_count":3,"count":1,"page":1,"limit":100,"total_pages":3}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err == nil {
		t.Fatalf("ListSubscriptions unexpectedly succeeded against a server that ignores ?page; subs = %+v", subs)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want no duplicated items on error", subs)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the mismatch)", got)
	}
}

// TestListDatasets_ServerOmitsPageWithMultiplePages_ReturnsError is the
// review round-3 finding: a server whose page-mismatch check has nothing
// to compare against because it never sends "page" at all bypassed the
// TestListDatasets_ServerIgnoresPageParam_ReturnsError guard entirely
// (respPage stayed nil, so `respPage != nil && *respPage != page` never
// fired), so a page-ignoring server that also omits "page" made
// paginateAll happily re-append the same item for every claimed page. The
// real API always echoes "page" on both /v1/datasets and /v1/subscriptions
// (internal/resources/datasets and .../subscriptions types.go, `json:"page"`
// with no `omitempty`), so once more than one page is being followed, a
// response missing that field is untrustworthy and must error instead of
// being silently accepted.
func TestListDatasets_ServerOmitsPageWithMultiplePages_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Exactly the reviewer's server: total_pages > 1, "page" absent.
		_, _ = w.Write([]byte(`{"datasets":[{"id":"A"}],"total_pages":3}`))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err == nil {
		t.Fatalf("ListDatasets unexpectedly succeeded against a server that omits \"page\" on a multi-page response; datasets = %+v", datasets)
	}
	if len(datasets) != 0 {
		t.Errorf("datasets = %+v, want no duplicated items on error", datasets)
	}
	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Errorf("request count = %d, want exactly 1 (the very first response already omits \"page\" while claiming 3 pages)", got)
	}
}

// TestListSubscriptions_ServerOmitsPageWithMultiplePages_ReturnsError is
// the same round-3 finding for ListSubscriptions, pinning that the guard
// applies to both paginateAll callers in this package, not just
// ListDatasets.
func TestListSubscriptions_ServerOmitsPageWithMultiplePages_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		body := `{"subscriptions":[{"_id":"sub-1","consumer_id":"test-customer","producer_id":"prod-1","tier":"free","status":"active"}],"total_count":3,"count":1,"total_pages":3}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err == nil {
		t.Fatalf("ListSubscriptions unexpectedly succeeded against a server that omits \"page\" on a multi-page response; subs = %+v", subs)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want no duplicated items on error", subs)
	}
	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Errorf("request count = %d, want exactly 1 (the very first response already omits \"page\" while claiming 3 pages)", got)
	}
}

// TestListDatasets_SinglePageOmitsPage_Accepted is the companion
// acceptance case to the two tests above: a single-page result
// (total_pages <= 1) has nothing ambiguous to detect from an omitted
// "page" — there is only ever one page to have served — so it must stay
// accepted exactly as before, not start erroring because of the new guard.
func TestListDatasets_SinglePageOmitsPage_Accepted(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"datasets":[{"id":"ds-a","name":"a"}],"total_count":1,"limit":100,"total_pages":1}`))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(datasets) != 1 || datasets[0].ID != "ds-a" {
		t.Fatalf("datasets = %+v, want exactly [{ID: ds-a}]", datasets)
	}
	if got := atomic.LoadInt32(&requestCount); got != 1 {
		t.Errorf("request count = %d, want exactly 1", got)
	}
}

// TestListDatasets_ServerOmitsPageAndDropsTotalPages_ReturnsError is the
// review round-4 finding: the round-3 guard checks each response only
// against ITSELF (respPage vs totalPages on that same response), so a
// server that flips its story between requests can dodge both existing
// checks at once — page 1 honestly reports {page:1, total_pages:3}, then
// page 2 repeats the same item while omitting "page" AND dropping
// total_pages to 1, which reads as a valid single-page response in
// isolation. Without locking the pagination shape after page 1, this
// silently produced [A,A] with no error. paginateAll must notice
// total_pages changed from what page 1 promised and stop.
func TestListDatasets_ServerOmitsPageAndDropsTotalPages_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write([]byte(`{"datasets":[{"id":"A"}],"page":1,"total_pages":3}`))
			return
		}
		// Repeats item A, omits "page", and reports total_pages=1 instead
		// of the 3 it promised on page 1.
		_, _ = w.Write([]byte(`{"datasets":[{"id":"A"}],"total_pages":1}`))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err == nil {
		t.Fatalf("ListDatasets unexpectedly succeeded against a server that changes total_pages and drops \"page\" after page 1; datasets = %+v", datasets)
	}
	if len(datasets) != 0 {
		t.Errorf("datasets = %+v, want no duplicated items on error", datasets)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the shape change)", got)
	}
}

// TestListDatasets_TotalPagesChangesBetweenPages_ReturnsError pins that the
// locked-shape guard fires on total_pages alone, even when "page" stays
// present and correct on every response — a server can't be trusted if it
// changes its mind about how many pages exist partway through a call.
func TestListDatasets_TotalPagesChangesBetweenPages_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write([]byte(datasetsPageJSON([]string{"a"}, 1, 3)))
			return
		}
		// page is present and correct, but total_pages dropped 3 -> 2.
		_, _ = w.Write([]byte(datasetsPageJSON([]string{"b"}, 2, 2)))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err == nil {
		t.Fatalf("ListDatasets unexpectedly succeeded against a server whose total_pages changed 3->2; datasets = %+v", datasets)
	}
	if len(datasets) != 0 {
		t.Errorf("datasets = %+v, want no items on error", datasets)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects total_pages changed)", got)
	}
}

// TestListSubscriptions_ServerOmitsPageAndDropsTotalPages_ReturnsError is
// the same round-4 scenario for ListSubscriptions, pinning that the
// locked-shape guard applies to both paginateAll callers in this package.
func TestListSubscriptions_ServerOmitsPageAndDropsTotalPages_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			body := `{"subscriptions":[{"_id":"sub-1","consumer_id":"test-customer","producer_id":"prod-1","tier":"free","status":"active"}],"total_count":3,"count":1,"page":1,"limit":100,"total_pages":3}`
			_, _ = w.Write([]byte(body))
			return
		}
		body := `{"subscriptions":[{"_id":"sub-1","consumer_id":"test-customer","producer_id":"prod-1","tier":"free","status":"active"}],"total_count":3,"count":1,"total_pages":1}`
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err == nil {
		t.Fatalf("ListSubscriptions unexpectedly succeeded against a server that changes total_pages and drops \"page\" after page 1; subs = %+v", subs)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want no duplicated items on error", subs)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the shape change)", got)
	}
}

// TestListDatasets_EmptyResult_ReturnsEmptyNotNilSlice is acceptance
// criterion 4: a successful empty list must marshal to JSON `[]`, not
// `null`, exactly as it did before the pagination fix.
func TestListDatasets_EmptyResult_ReturnsEmptyNotNilSlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(datasetsPageJSON(nil, 1, 1)))
	}))
	defer server.Close()

	datasets, err := newTestConsumer(server.URL).ListDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if datasets == nil {
		t.Fatal("ListDatasets returned a nil slice for an empty result, want a non-nil empty slice")
	}

	got, err := json.Marshal(datasets)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("json.Marshal(datasets) = %s, want []", got)
	}
}

// TestListSubscriptions_EmptyResult_ReturnsEmptyNotNilSlice is the same
// acceptance criterion 4 scenario for ListSubscriptions.
func TestListSubscriptions_EmptyResult_ReturnsEmptyNotNilSlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"subscriptions":[],"total_count":0,"count":0,"page":1,"limit":100,"total_pages":1}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	subs, err := newTestConsumer(server.URL).ListSubscriptions(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if subs == nil {
		t.Fatal("ListSubscriptions returned a nil slice for an empty result, want a non-nil empty slice")
	}

	got, err := json.Marshal(subs)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("json.Marshal(subs) = %s, want []", got)
	}
}
