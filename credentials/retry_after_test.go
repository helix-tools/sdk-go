package credentials

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Retry-After-aware waits inside one mint (v2.20.2).
//
// The live failure: the credential service answered POST /v1/credentials/
// session with 429 "rate limit exceeded: retry after 1 seconds", and the
// mint loop's ~200-600 ms backoff spent all 3 attempts inside that second.
// A 429 that states a wait (Retry-After header, or the API's "retry after N
// seconds" message) now stretches the next wait to at least that long,
// capped at mintRetryAfterCap (5 s). Same rule as the Python SDK.
//
// No real sleeping: Provider.sleep is replaced by a recorder, so every wait
// is asserted exactly and the suite stays fast.

type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *sleepRecorder) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

// retryProvider returns a Provider against broker whose waits are recorded
// instead of slept.
func retryProvider(t *testing.T, broker *fakeBroker) (*Provider, *sleepRecorder) {
	t.Helper()
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	rec := &sleepRecorder{}
	p.sleep = rec.sleep
	return p, rec
}

func rateLimitedThenOK(message string, header http.Header) *fakeBrokerSpec {
	return &fakeBrokerSpec{
		respond: func(n int) (int, string) {
			if n == 1 {
				return http.StatusTooManyRequests, errorBody("rate_limited", message, "req-429")
			}
			return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
		},
		header: func(n int) http.Header {
			if n == 1 {
				return header
			}
			return nil
		},
	}
}

type fakeBrokerSpec struct {
	respond func(int) (int, string)
	header  func(int) http.Header
}

func (s *fakeBrokerSpec) start(t *testing.T) *fakeBroker {
	t.Helper()
	broker := newFakeBroker(t, s.respond)
	broker.header = s.header
	return broker
}

// retrieveAfterOneRetry checks a mint succeeded after exactly 2 calls and 1 wait,
// and returns that wait.
func retrieveAfterOneRetry(t *testing.T, spec *fakeBrokerSpec) time.Duration {
	t.Helper()
	broker := spec.start(t)
	p, rec := retryProvider(t, broker)
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("broker calls = %d, want 2 (one retry)", broker.calls())
	}
	waits := rec.recorded()
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want exactly one", waits)
	}
	return waits[0]
}

// assertNormalBackoff checks wait is backoffDelay(1)'s range: 200 ms +/- 25%.
func assertNormalBackoff(t *testing.T, wait time.Duration) {
	t.Helper()
	lo, hi := mintRetryBaseDelay*3/4, mintRetryBaseDelay*5/4
	if wait < lo || wait >= hi {
		t.Fatalf("wait = %v, want the normal backoff in [%v, %v)", wait, lo, hi)
	}
}

// The exact live failure: one retry, and the wait honours the stated second.
func TestMintRetry_429RetryAfterMessageThen200_WaitsAtLeastThatLong(t *testing.T) {
	wait := retrieveAfterOneRetry(t, rateLimitedThenOK("rate limit exceeded: retry after 1 seconds", nil))
	if wait < time.Second || wait > mintRetryAfterCap {
		t.Fatalf("wait = %v, want >= 1s and <= %v", wait, mintRetryAfterCap)
	}
}

func TestMintRetry_RetryAfterHeaderIsHonoured(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"plain", http.Header{"Retry-After": {"2"}}, 2 * time.Second},
		{"leading_zeros", http.Header{"Retry-After": {"0000000002"}}, 2 * time.Second},
		{"lower_case_name", http.Header{"retry-after": {"3"}}, 3 * time.Second},
		{"surrounding_space", http.Header{"Retry-After": {" 4 "}}, 4 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retrieveAfterOneRetry(t, rateLimitedThenOK("rate limit exceeded", tc.header)); got != tc.want {
				t.Fatalf("wait = %v, want %v", got, tc.want)
			}
		})
	}
}

// A header that is plain digits wins over the message.
func TestMintRetry_RetryAfterHeaderWinsOverMessage(t *testing.T) {
	got := retrieveAfterOneRetry(t, rateLimitedThenOK("retry after 4 seconds", http.Header{"Retry-After": {"2"}}))
	if got != 2*time.Second {
		t.Fatalf("wait = %v, want 2s (the header)", got)
	}
}

// Large and enormous values cap at 5 s and never overflow or panic.
func TestMintRetry_RetryAfterIsCappedAtFiveSeconds(t *testing.T) {
	cases := []struct {
		name    string
		header  http.Header
		message string
	}{
		{"header_60", http.Header{"Retry-After": {"60"}}, "rate limit exceeded"},
		{"message_60", nil, "rate limit exceeded: retry after 60 seconds"},
		{"header_1000", http.Header{"Retry-After": {"1000"}}, "rate limit exceeded"},
		{"header_20_digits", http.Header{"Retry-After": {"99999999999999999999"}}, "rate limit exceeded"},
		{"header_5000_digits", http.Header{"Retry-After": {strings.Repeat("9", 5000)}}, "rate limit exceeded"},
		{"message_5000_digits", nil, "retry after " + strings.Repeat("9", 5000) + " seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retrieveAfterOneRetry(t, rateLimitedThenOK(tc.message, tc.header)); got != mintRetryAfterCap {
				t.Fatalf("wait = %v, want the %v cap", got, mintRetryAfterCap)
			}
		})
	}
}

// Garbage the SDK cannot read as whole seconds is ignored without crashing:
// the normal backoff applies (same rule as the Python SDK).
func TestMintRetry_UnusableRetryAfterFallsBackToBackoff(t *testing.T) {
	for _, value := range []string{"Wed, 21 Oct 2026 07:28:00 GMT", "-1", "+5", "inf", "nan", "1.5", "²", "", "0"} {
		t.Run(value, func(t *testing.T) {
			assertNormalBackoff(t, retrieveAfterOneRetry(t, rateLimitedThenOK("rate limit exceeded", http.Header{"Retry-After": {value}})))
		})
	}
}

// Retry-After is a 429 signal only; a 503 carrying one keeps the backoff.
func TestMintRetry_RetryAfterIsOnlyHonouredOn429(t *testing.T) {
	spec := &fakeBrokerSpec{
		respond: func(n int) (int, string) {
			if n == 1 {
				return http.StatusServiceUnavailable, errorBody("unavailable", "retry after 4 seconds", "req-503")
			}
			return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
		},
		header: func(int) http.Header { return http.Header{"Retry-After": {"4"}} },
	}
	assertNormalBackoff(t, retrieveAfterOneRetry(t, spec))
}

func TestMintRetry_429ThreeTimes_FailsAfterExactlyThreeAttempts(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusTooManyRequests, errorBody("rate_limited", "rate limit exceeded: retry after 1 seconds", "req-429")
	})
	p, rec := retryProvider(t, broker)
	_, err := p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected an error after exhausting attempts, got nil")
	}
	if broker.calls() != mintMaxAttempts {
		t.Fatalf("broker calls = %d, want exactly %d", broker.calls(), mintMaxAttempts)
	}
	waits := rec.recorded()
	if len(waits) != mintMaxAttempts-1 {
		t.Fatalf("waits = %v, want %d (none after the last attempt)", waits, mintMaxAttempts-1)
	}
	for i, w := range waits {
		if w < time.Second || w > mintRetryAfterCap {
			t.Errorf("wait %d = %v, want >= 1s and <= %v", i, w, mintRetryAfterCap)
		}
	}
	var mErr *MintError
	if !errors.As(err, &mErr) || mErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the last attempt's 429 MintError", err)
	}
	// Exhausted rate limiting is not a verdict on the credential: the next
	// Retrieve tries the broker again.
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("second Retrieve: expected an error, got nil")
	}
	if broker.calls() != 2*mintMaxAttempts {
		t.Fatalf("broker calls after second Retrieve = %d, want %d (not latched)", broker.calls(), 2*mintMaxAttempts)
	}
}

func TestMintRetry_401IsNotRetried(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusUnauthorized, errorBody("unauthorized", "retry after 1 seconds", "req-401")
	})
	broker.header = func(int) http.Header { return http.Header{"Retry-After": {"1"}} }
	p, rec := retryProvider(t, broker)
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("expected an error, got nil")
	}
	if broker.calls() != 1 {
		t.Fatalf("broker calls = %d, want 1 (401 is never retried)", broker.calls())
	}
	if waits := rec.recorded(); len(waits) != 0 {
		t.Fatalf("waits = %v, want none", waits)
	}
}

// 408 is retried, matching the TypeScript and Python SDKs.
func TestMintRetry_408Then200Succeeds(t *testing.T) {
	spec := &fakeBrokerSpec{respond: func(n int) (int, string) {
		if n == 1 {
			return http.StatusRequestTimeout, errorBody("request_timeout", "request timed out", "req-408")
		}
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	}}
	assertNormalBackoff(t, retrieveAfterOneRetry(t, spec))
}

// A context that ends during a wait stops the loop with the context's
// error and makes no further attempt.
func TestMintRetry_ContextCancelledDuringWaitStops(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusTooManyRequests, errorBody("rate_limited", "retry after 1 seconds", "req-429")
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepContext(ctx, d)
	}
	if _, err := p.Retrieve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("broker calls = %d, want 1", broker.calls())
	}
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepContext: %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext on a cancelled ctx = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("sleepContext on a cancelled ctx took %v, want immediate", elapsed)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	cases := []struct {
		header, message string
		want            time.Duration
	}{
		{"", "", 0},
		{"", "rate limit exceeded", 0},
		{"", "rate limit exceeded: retry after 1 seconds", time.Second},
		{"", "Retry After 1 second", time.Second},
		{"1", "", time.Second},
		{"5", "", 5 * time.Second},
		{"6", "", mintRetryAfterCap},
		{"999", "", mintRetryAfterCap},
		{"0001", "", time.Second},
		{"0", "", 0},
		{"000", "", 0},
		{"abc", "retry after 2 seconds", 2 * time.Second},
		{"-1", "", 0},
		{"1e3", "", 0},
		{"", "retry after -3 seconds", 0},
		{strings.Repeat("0", 5000) + "3", "", 3 * time.Second},
	}
	for _, tc := range cases {
		if got := retryAfterDelay(tc.header, tc.message); got != tc.want {
			t.Errorf("retryAfterDelay(%q, %q) = %v, want %v", tc.header, tc.message, got, tc.want)
		}
	}
}

// Self-attack: a broker that rate-limits forever with the largest, most
// hostile waits it can state must still get exactly mintMaxAttempts calls,
// and no single wait may exceed the cap.
func TestMintRetry_HostileRetryAfterNeverExceedsBounds(t *testing.T) {
	headers := []string{"  " + strings.Repeat("0", 4096) + strings.Repeat("9", 4096) + "  ", "9223372036854775808", "-9223372036854775808", "0"}
	for _, h := range headers {
		broker := newFakeBroker(t, func(int) (int, string) {
			return http.StatusTooManyRequests, errorBody("rate_limited", "retry after 18446744073709551616 seconds", "req-429")
		})
		broker.header = func(int) http.Header { return http.Header{"Retry-After": {h}} }
		p, rec := retryProvider(t, broker)
		if _, err := p.Retrieve(context.Background()); err == nil {
			t.Fatal("expected an error, got nil")
		}
		if broker.calls() != mintMaxAttempts {
			t.Fatalf("header %.20q: broker calls = %d, want exactly %d", h, broker.calls(), mintMaxAttempts)
		}
		for i, w := range rec.recorded() {
			if w <= 0 || w > mintRetryAfterCap {
				t.Errorf("header %.20q: wait %d = %v, want in (0, %v]", h, i, w, mintRetryAfterCap)
			}
		}
	}
}

// Wiring: a Provider exactly as NewProvider builds it (real sleep, no test
// override) really waits out the live failure's stated second.
func TestMintRetry_DefaultProviderReallyWaitsRetryAfter(t *testing.T) {
	broker := rateLimitedThenOK("rate limit exceeded: retry after 1 seconds", nil).start(t)
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	start := time.Now()
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("elapsed = %v, want >= 1s (the stated Retry-After)", elapsed)
	}
	if broker.calls() != 2 {
		t.Fatalf("broker calls = %d, want 2", broker.calls())
	}
}
