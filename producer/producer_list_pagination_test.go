package producer

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

// The Go API's actual response shape for GET /v1/datasets
// (internal/resources/datasets/types.go's ListDatasetsResponse) is an
// OBJECT — {datasets, total_count, page, limit, total_pages} — never a bare
// array. ListMyDatasets used to decode straight into []types.Dataset, which
// fails against every real response with "cannot unmarshal object into Go
// value of type []types.Dataset". These tests pin the fix: decode the
// envelope, and follow every page.

// datasetsPageJSON renders one page of the API's exact ListDatasetsResponse
// shape, field names copied from datasets/types.go.
func datasetsPageJSON(names []string, page, totalPages int) string {
	items := ""
	for i, name := range names {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(`{"id":"ds-%s","name":%q,"producer_id":"test-producer"}`, name, name)
	}
	return fmt.Sprintf(`{"datasets":[%s],"total_count":%d,"page":%d,"limit":100,"total_pages":%d}`,
		items, len(names), page, totalPages)
}

// TestListMyDatasets_ExactAPIShape_ReturnsAllDatasets is acceptance
// criterion 1: a mock returning the API's exact JSON object shape (not a
// bare array) must decode successfully and hand back every dataset.
func TestListMyDatasets_ExactAPIShape_ReturnsAllDatasets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(datasetsPageJSON([]string{"a", "b"}, 1, 1)))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	datasets, err := p.ListMyDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}
	if len(datasets) != 2 {
		t.Fatalf("len(datasets) = %d, want 2", len(datasets))
	}
	if datasets[0].Name != "a" || datasets[1].Name != "b" {
		t.Errorf("datasets = %+v, want names [a b]", datasets)
	}
}

// TestListMyDatasets_FollowsThreePages_InOrder is acceptance criterion 2:
// three pages must yield every item from every page, in order, via exactly
// three requests.
func TestListMyDatasets_FollowsThreePages_InOrder(t *testing.T) {
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

	p := newTestProducer(server.URL)

	datasets, err := p.ListMyDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
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

// TestListMyDatasets_StopsOnEmptyPageDespiteHighTotalPages is acceptance
// criterion 3: a server that always claims total_pages=999 but starts
// returning empty pages must not be trusted past its real data — the loop
// stops at the first empty page instead of making 999 requests (or
// spinning forever if the server never terminates the pattern).
func TestListMyDatasets_StopsOnEmptyPageDespiteHighTotalPages(t *testing.T) {
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
		// Every page after the first is empty, despite total_pages still
		// claiming 999 pages exist.
		_, _ = w.Write([]byte(datasetsPageJSON(nil, page, 999)))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	datasets, err := p.ListMyDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}
	if len(datasets) != 1 || datasets[0].Name != "a" {
		t.Fatalf("datasets = %+v, want exactly [a]", datasets)
	}

	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 non-empty, page 2 empty stops the loop)", got)
	}
}

// TestListMyDatasets_RejectsBareArrayShape is the negative control for
// criterion 1, exercised through the public method rather than a
// standalone json.Unmarshal: a server that regresses to the pre-fix bare
// array (what "cannot unmarshal object into Go value of type
// []types.Dataset" looked like in production) must make ListMyDatasets
// itself fail, not merely a hand-rolled decode in the test body.
func TestListMyDatasets_RejectsBareArrayShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":"ds-a","name":"a"}]`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.ListMyDatasets(context.Background()); err == nil {
		t.Fatal("ListMyDatasets unexpectedly succeeded against a bare-array response; the negative control no longer holds")
	}
}

// TestGetDatasetSubscribers_FollowsAllPages pins that the second caller of
// paginateAll (GET /v1/subscriptions?dataset_id=...) is wired correctly:
// its own field names ("subscriptions", not "datasets") and its own item
// type must decode and paginate exactly like ListMyDatasets does.
//
// The mock is keyed on the ?page query parameter the client actually sent
// (not a request counter), and the test asserts the exact sequence of
// requested pages (1,2,3) — a client that re-requested page 1 instead of
// advancing would get the same two items back forever and fail this
// assertion, rather than being silently satisfied by a counter that
// advances regardless of what the client asked for.
func TestGetDatasetSubscribers_FollowsAllPages(t *testing.T) {
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
			items += fmt.Sprintf(`{"_id":%q,"consumer_id":"cons-1","producer_id":"test-producer","dataset_id":"ds-1","tier":"free","status":"active"}`, id)
		}
		body := fmt.Sprintf(`{"subscriptions":[%s],"total_count":%d,"count":%d,"page":%d,"limit":100,"total_pages":%d}`,
			items, len(pages[page-1]), len(pages[page-1]), page, len(pages))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	subs, err := p.GetDatasetSubscribers(context.Background(), "ds-1")
	if err != nil {
		t.Fatalf("GetDatasetSubscribers: %v", err)
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

// TestListMyDatasets_ServerIgnoresPageParam_ReturnsError is acceptance
// criterion 2 for ListMyDatasets: a server that ignores ?page and keeps
// answering with page=1 (total_pages=3) must not be trusted to be
// advancing — paginateAll must notice the response's page doesn't match
// the page it requested and stop with an error, instead of appending the
// same item three times.
func TestListMyDatasets_ServerIgnoresPageParam_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Always reports page=1, regardless of the requested ?page.
		_, _ = w.Write([]byte(datasetsPageJSON([]string{"a"}, 1, 3)))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	datasets, err := p.ListMyDatasets(context.Background())
	if err == nil {
		t.Fatalf("ListMyDatasets unexpectedly succeeded against a server that ignores ?page; datasets = %+v", datasets)
	}
	if len(datasets) != 0 {
		t.Errorf("datasets = %+v, want no duplicated items on error", datasets)
	}
	// Page 1 legitimately matches (requested 1, server reports 1); the
	// mismatch is only detectable on the second request (requested 2, got
	// 1 again).
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the mismatch)", got)
	}
}

// TestGetDatasetSubscribers_ServerIgnoresPageParam_ReturnsError is the
// same acceptance criterion 2 scenario for GetDatasetSubscribers, pinning
// that the page-mismatch check applies to both paginateAll callers in this
// package, not just ListMyDatasets.
func TestGetDatasetSubscribers_ServerIgnoresPageParam_ReturnsError(t *testing.T) {
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		body := `{"subscriptions":[{"_id":"sub-1","consumer_id":"cons-1","producer_id":"test-producer","dataset_id":"ds-1","tier":"free","status":"active"}],"total_count":3,"count":1,"page":1,"limit":100,"total_pages":3}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	subs, err := p.GetDatasetSubscribers(context.Background(), "ds-1")
	if err == nil {
		t.Fatalf("GetDatasetSubscribers unexpectedly succeeded against a server that ignores ?page; subs = %+v", subs)
	}
	if len(subs) != 0 {
		t.Errorf("subs = %+v, want no duplicated items on error", subs)
	}
	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want exactly 2 (page 1 succeeds, page 2 detects the mismatch)", got)
	}
}

// TestListMyDatasets_EmptyResult_ReturnsEmptyNotNilSlice is acceptance
// criterion 4: a successful empty list must marshal to JSON `[]`, not
// `null`, exactly as it did before the pagination fix.
func TestListMyDatasets_EmptyResult_ReturnsEmptyNotNilSlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(datasetsPageJSON(nil, 1, 1)))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	datasets, err := p.ListMyDatasets(context.Background())
	if err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}
	if datasets == nil {
		t.Fatal("ListMyDatasets returned a nil slice for an empty result, want a non-nil empty slice")
	}

	got, err := json.Marshal(datasets)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("json.Marshal(datasets) = %s, want []", got)
	}
}

// TestGetDatasetSubscribers_EmptyResult_ReturnsEmptyNotNilSlice is the same
// acceptance criterion 4 scenario for GetDatasetSubscribers.
func TestGetDatasetSubscribers_EmptyResult_ReturnsEmptyNotNilSlice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"subscriptions":[],"total_count":0,"count":0,"page":1,"limit":100,"total_pages":1}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	subs, err := p.GetDatasetSubscribers(context.Background(), "ds-1")
	if err != nil {
		t.Fatalf("GetDatasetSubscribers: %v", err)
	}
	if subs == nil {
		t.Fatal("GetDatasetSubscribers returned a nil slice for an empty result, want a non-nil empty slice")
	}

	got, err := json.Marshal(subs)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("json.Marshal(subs) = %s, want []", got)
	}
}
