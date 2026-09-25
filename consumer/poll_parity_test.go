package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// fakeSQS answers the two SQS actions PollNotifications/ClearQueue issue and
// records each request's target and JSON body, so a test can assert what the
// SDK actually put on the wire.
type fakeSQS struct {
	srv *httptest.Server

	mu        sync.Mutex
	targets   []string
	requests  []map[string]any
	badLength int // requests whose Content-Length disagreed with the body actually received
}

func newFakeSQS(t *testing.T) *fakeSQS {
	t.Helper()
	f := &fakeSQS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.targets = append(f.targets, r.Header.Get("X-Amz-Target"))
		f.requests = append(f.requests, body)
		if r.ContentLength != int64(len(raw)) {
			f.badLength++
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "ReceiveMessage") {
			_, _ = w.Write([]byte(`{"Messages":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// attach points c's SQS client at the fake.
func (f *fakeSQS) attach(c *Consumer) {
	c.sqsClient = sqs.NewFromConfig(c.awsConfig, func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(f.srv.URL)
	})
}

func (f *fakeSQS) last(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no SQS request was made")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeSQS) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// ----------------------------------------------------------------------------
// A-08: pollNotifications option semantics.
// ----------------------------------------------------------------------------

func TestPollNotifications_WireOptions(t *testing.T) {
	cases := []struct {
		name         string
		opts         PollNotificationsOptions
		wantVis      float64
		wantWait     float64 // 0 means "absent or 0" (a short poll)
		wantMaxMsgs  float64
		wantWaitZero bool // the key must be PRESENT on the wire and equal 0
	}{
		{name: "defaults", opts: PollNotificationsOptions{}, wantVis: 300, wantWait: 20, wantMaxMsgs: 10},
		{name: "custom visibility timeout", opts: PollNotificationsOptions{VisibilityTimeout: 45}, wantVis: 45, wantWait: 20, wantMaxMsgs: 10},
		{name: "visibility timeout capped at the AWS limit", opts: PollNotificationsOptions{VisibilityTimeout: 99999}, wantVis: 43200, wantWait: 20, wantMaxMsgs: 10},
		{name: "negative visibility timeout falls back to the default", opts: PollNotificationsOptions{VisibilityTimeout: -5}, wantVis: 300, wantWait: 20, wantMaxMsgs: 10},
		{name: "explicit wait", opts: PollNotificationsOptions{WaitTimeSeconds: 5}, wantVis: 300, wantWait: 5, wantMaxMsgs: 10},
		{name: "wait capped at 20", opts: PollNotificationsOptions{WaitTimeSeconds: 60}, wantVis: 300, wantWait: 20, wantMaxMsgs: 10},
		{name: "negative wait falls back to 20", opts: PollNotificationsOptions{WaitTimeSeconds: -3}, wantVis: 300, wantWait: 20, wantMaxMsgs: 10},
		{name: "ShortPoll returns immediately", opts: PollNotificationsOptions{ShortPoll: true}, wantVis: 300, wantWaitZero: true, wantMaxMsgs: 10},
		{name: "ShortPoll wins over WaitTimeSeconds", opts: PollNotificationsOptions{ShortPoll: true, WaitTimeSeconds: 15}, wantVis: 300, wantWaitZero: true, wantMaxMsgs: 10},
		{name: "max messages passed through", opts: PollNotificationsOptions{MaxMessages: 3}, wantVis: 300, wantWait: 20, wantMaxMsgs: 3},
		{name: "negative max messages falls back to 10", opts: PollNotificationsOptions{MaxMessages: -4}, wantVis: 300, wantWait: 20, wantMaxMsgs: 10},
		{name: "max messages capped at 10", opts: PollNotificationsOptions{MaxMessages: 50}, wantVis: 300, wantWait: 20, wantMaxMsgs: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSQS(t)
			c := newTestConsumer("http://unused.invalid")
			f.attach(c)
			queue := "https://sqs.example/q-1"
			c.queueURL = &queue

			if _, err := c.PollNotifications(context.Background(), tc.opts); err != nil {
				t.Fatalf("PollNotifications: %v", err)
			}
			got := f.last(t)
			if got["VisibilityTimeout"] != tc.wantVis {
				t.Errorf("VisibilityTimeout = %v, want %v", got["VisibilityTimeout"], tc.wantVis)
			}
			gotWait, hasWait := got["WaitTimeSeconds"].(float64)
			if tc.wantWaitZero {
				// The SQS serializer omits a zero value, and an omitted wait
				// means "the queue default" (a 20s long poll), so a short poll
				// must carry an EXPLICIT 0.
				if !hasWait || gotWait != 0 {
					t.Errorf("WaitTimeSeconds = %v (present: %v), want an explicit 0 for a short poll", got["WaitTimeSeconds"], hasWait)
				}
			} else if gotWait != tc.wantWait {
				t.Errorf("WaitTimeSeconds = %v, want %v", got["WaitTimeSeconds"], tc.wantWait)
			}
			if got["MaxNumberOfMessages"] != tc.wantMaxMsgs {
				t.Errorf("MaxNumberOfMessages = %v, want %v", got["MaxNumberOfMessages"], tc.wantMaxMsgs)
			}
			if got["QueueUrl"] != queue {
				t.Errorf("QueueUrl = %v, want %q", got["QueueUrl"], queue)
			}
			// A rewritten body (ShortPoll) must still be framed correctly.
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.badLength != 0 {
				t.Errorf("%d request(s) had a Content-Length that disagrees with the body", f.badLength)
			}
		})
	}
}

// ----------------------------------------------------------------------------
// A-09: queue discovery works when the API rejects role=consumer.
// ----------------------------------------------------------------------------

// subsAPI serves GET /v1/subscriptions. When roleStatus is non-zero it is the
// status returned for requests carrying role=consumer (the API answers 400
// "role does not match customer type" for a credential whose customer_type is
// not "both"); the plain list is always served.
type subsAPI struct {
	srv        *httptest.Server
	mu         sync.Mutex
	queries    []string
	roleStatus int
	rows       string
}

func newSubsAPI(t *testing.T, roleStatus int, rows string) *subsAPI {
	t.Helper()
	a := &subsAPI{roleStatus: roleStatus, rows: rows}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.queries = append(a.queries, r.URL.RawQuery)
		a.mu.Unlock()
		if r.URL.Query().Get("role") == "consumer" && a.roleStatus != 0 {
			w.WriteHeader(a.roleStatus)
			_, _ = w.Write([]byte(`{"error":"role does not match customer type"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subscriptions":[` + a.rows + `],"page":1,"total_pages":1}`))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *subsAPI) requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queries)
}

func (a *subsAPI) anyWithoutRole() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, q := range a.queries {
		if !strings.Contains(q, "role=") {
			return true
		}
	}
	return false
}

// The API returns sqs_queue_url on EVERY row, including rows where the caller
// is the producer. The first row here is such a row: picking it would poll (or
// purge) somebody else's queue.
const producerSideRow = `{"_id":"sub-p","consumer_id":"someone-else","producer_id":"test-customer","dataset_id":"ds-1","tier":"free","status":"active","sqs_queue_url":"https://sqs.example/OTHERS-queue"}`
const consumerRow = `{"_id":"sub-c","consumer_id":"test-customer","producer_id":"prod-1","dataset_id":"ds-1","tier":"free","status":"active","sqs_queue_url":"https://sqs.example/MY-queue"}`

func TestPollNotifications_FallsBackToPlainListWhenRoleRejected(t *testing.T) {
	api := newSubsAPI(t, http.StatusBadRequest, producerSideRow+","+consumerRow)
	f := newFakeSQS(t)
	c := newTestConsumer(api.srv.URL)
	f.attach(c)

	if _, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true}); err != nil {
		t.Fatalf("PollNotifications must survive a 400 on role=consumer: %v", err)
	}
	if !api.anyWithoutRole() {
		t.Error("expected a retry of the subscription list without role")
	}
	if got := f.last(t)["QueueUrl"]; got != "https://sqs.example/MY-queue" {
		t.Errorf("polled %v, want the queue of the row where this customer is the consumer", got)
	}
}

func TestClearQueue_FallsBackToPlainListWhenRoleRejected(t *testing.T) {
	api := newSubsAPI(t, http.StatusBadRequest, producerSideRow+","+consumerRow)
	f := newFakeSQS(t)
	c := newTestConsumer(api.srv.URL)
	f.attach(c)

	if err := c.ClearQueue(context.Background()); err != nil {
		t.Fatalf("ClearQueue must survive a 400 on role=consumer: %v", err)
	}
	if got := f.last(t)["QueueUrl"]; got != "https://sqs.example/MY-queue" {
		t.Errorf("purged %v, want ONLY the queue of the row where this customer is the consumer", got)
	}
}

// Bypass: the fallback list is unfiltered, so it must still be narrowed to rows
// where this customer is the consumer. A list holding only producer-side rows
// must be an error — never a poll or purge of another customer's queue.
func TestQueueDiscovery_FallbackNeverPicksAProducerSideQueue(t *testing.T) {
	for name, run := range map[string]func(*Consumer) error{
		"PollNotifications": func(c *Consumer) error {
			_, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true})
			return err
		},
		"ClearQueue": func(c *Consumer) error { return c.ClearQueue(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			api := newSubsAPI(t, http.StatusBadRequest, producerSideRow)
			f := newFakeSQS(t)
			c := newTestConsumer(api.srv.URL)
			f.attach(c)

			err := run(c)
			if err == nil || !strings.Contains(err.Error(), "where you are the consumer") {
				t.Fatalf("err = %v, want the no-consumer-subscription error", err)
			}
			if f.count() != 0 {
				t.Errorf("SQS was called %d time(s); it must not be touched", f.count())
			}
		})
	}
}

func TestQueueDiscovery_NoFallbackWhenRoleAccepted(t *testing.T) {
	api := newSubsAPI(t, 0, consumerRow)
	f := newFakeSQS(t)
	c := newTestConsumer(api.srv.URL)
	f.attach(c)

	if _, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true}); err != nil {
		t.Fatalf("PollNotifications: %v", err)
	}
	if api.requests() != 1 || api.anyWithoutRole() {
		t.Errorf("subscription list requests = %d (without role: %v), want exactly one role=consumer request", api.requests(), api.anyWithoutRole())
	}
}

// Only a 400 (the documented role rejection) triggers the fallback; a real
// server failure must surface, not be papered over with a second request.
func TestQueueDiscovery_NoFallbackOnServerError(t *testing.T) {
	api := newSubsAPI(t, http.StatusInternalServerError, consumerRow)
	f := newFakeSQS(t)
	c := newTestConsumer(api.srv.URL)
	f.attach(c)

	_, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true})
	if err == nil {
		t.Fatal("expected the 500 to surface")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("err = %v, want an *APIError with status 500", err)
	}
	if api.requests() != 1 {
		t.Errorf("subscription list requests = %d, want 1 (no fallback on a 500)", api.requests())
	}
}

// ----------------------------------------------------------------------------
// A-06: typed, status-bearing errors from the consumer.
// ----------------------------------------------------------------------------

func TestMakeAPIRequest_ReturnsTypedAPIError(t *testing.T) {
	for _, tc := range []struct {
		status int
		check  func(*APIError) bool
		name   string
	}{
		{http.StatusUnauthorized, (*APIError).IsUnauthorized, "IsUnauthorized"},
		{http.StatusForbidden, (*APIError).IsForbidden, "IsForbidden"},
		{http.StatusNotFound, (*APIError).IsNotFound, "IsNotFound"},
		{http.StatusConflict, (*APIError).IsConflict, "IsConflict"},
		{http.StatusTooManyRequests, (*APIError).IsRateLimited, "IsRateLimited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"nope"}`))
			}))
			defer server.Close()

			_, err := newTestConsumer(server.URL).GetDataset(context.Background(), "ds-1")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %T %v, want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Body != `{"error":"nope"}` {
				t.Errorf("APIError = %+v, want status %d and the response body", apiErr, tc.status)
			}
			if !tc.check(apiErr) {
				t.Errorf("%s = false for status %d", tc.name, tc.status)
			}
			// The pre-existing message format is unchanged.
			if !strings.HasPrefix(err.Error(), "API request failed: ") {
				t.Errorf("Error() = %q, want the historical 'API request failed: <status> - <body>' text", err.Error())
			}
			// And no predicate fires for a different status.
			other := &APIError{StatusCode: http.StatusTeapot}
			if other.IsUnauthorized() || other.IsForbidden() || other.IsNotFound() || other.IsConflict() || other.IsRateLimited() {
				t.Error("a 418 must not satisfy any status predicate")
			}
		})
	}
}

// The three "cannot resolve a queue" outcomes keep their historical messages,
// per caller, and never touch SQS.
func TestQueueDiscovery_NoQueueOutcomes(t *testing.T) {
	notProvisioned := `{"_id":"sub-c","consumer_id":"test-customer","producer_id":"prod-1","dataset_id":"ds-1","tier":"free","status":"active"}`

	for _, tc := range []struct {
		name        string
		rows        string
		wantPoll    string
		wantClear   string
		roleRejects bool
	}{
		{name: "no subscriptions", rows: "", wantPoll: "no active subscriptions found. Create a subscription first", wantClear: "no active subscriptions found. Cannot determine queue URL"},
		{name: "queue not provisioned", rows: notProvisioned, wantPoll: "per-consumer queue not provisioned", wantClear: "per-consumer queue not provisioned"},
		{name: "not provisioned after the role fallback", rows: notProvisioned, roleRejects: true, wantPoll: "per-consumer queue not provisioned", wantClear: "per-consumer queue not provisioned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := 0
			if tc.roleRejects {
				status = http.StatusBadRequest
			}
			api := newSubsAPI(t, status, tc.rows)
			f := newFakeSQS(t)

			c := newTestConsumer(api.srv.URL)
			f.attach(c)
			if _, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true}); err == nil || !strings.Contains(err.Error(), tc.wantPoll) {
				t.Errorf("PollNotifications err = %v, want it to contain %q", err, tc.wantPoll)
			}

			c = newTestConsumer(api.srv.URL)
			f.attach(c)
			if err := c.ClearQueue(context.Background()); err == nil || !strings.Contains(err.Error(), tc.wantClear) {
				t.Errorf("ClearQueue err = %v, want it to contain %q", err, tc.wantClear)
			}

			if f.count() != 0 {
				t.Errorf("SQS was called %d time(s); it must not be touched when no queue resolves", f.count())
			}
		})
	}
}

// If the retry without a role fails too, that failure — not the original 400 —
// is what the caller sees, still wrapped so errors.As finds the status.
func TestQueueDiscovery_FallbackListFailureSurfaces(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Query().Get("role") == "consumer" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()

	f := newFakeSQS(t)
	c := newTestConsumer(server.URL)
	f.attach(c)

	_, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want the 500 from the retry", err)
	}
	if hits != 2 {
		t.Errorf("subscription requests = %d, want the role=consumer attempt plus one retry", hits)
	}
}
