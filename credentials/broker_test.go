package credentials

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helix-tools/sdk-go/v2/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
)

// ---------------------------------------------------------------------------
// Test fixtures / helpers
// ---------------------------------------------------------------------------

const (
	testAccessKeyID     = "ASIAEXAMPLE1234567X"
	testSecretAccessKey = "exampleSecretAccessKey1234567890123456"
	testSessionToken    = "example-session-token-opaque-value"
	testRegion          = "us-east-1"
)

// fakeBroker is a per-test httptest.Server simulating POST
// /v1/credentials/session. respond is called on every inbound request (1
// -indexed call number) and returns the status/body to send; tests mutate
// it between assertions to simulate a broker whose behavior changes over
// time (e.g. transient 500s that later recover). Every inbound request is
// captured (headers, after the body is drained) so signing assertions can
// inspect them, and callCount is incremented atomically so concurrency
// tests (single-flight) can assert exactly how many times the broker was
// actually invoked.
type fakeBroker struct {
	t       *testing.T
	server  *httptest.Server
	respond func(callNum int) (status int, body string)

	callCount int32

	mu       sync.Mutex
	requests []*http.Request

	// block, when non-nil, makes the handler wait for a receive on this
	// channel before responding — used to widen the race window for
	// single-flight tests.
	block chan struct{}
}

func newFakeBroker(t *testing.T, respond func(callNum int) (status int, body string)) *fakeBroker {
	t.Helper()
	f := &fakeBroker{t: t, respond: respond}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeBroker) handle(w http.ResponseWriter, r *http.Request) {
	n := int(atomic.AddInt32(&f.callCount, 1))

	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	f.mu.Unlock()

	if f.block != nil {
		<-f.block
	}

	status, body := f.respond(n)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeBroker) calls() int {
	return int(atomic.LoadInt32(&f.callCount))
}

func (f *fakeBroker) lastRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// successBody builds a fully-populated success response body.
func successBody(expiration time.Time, ttlSeconds int64) string {
	return fmt.Sprintf(`{
		"access_key_id": %q,
		"secret_access_key": %q,
		"session_token": %q,
		"expiration": %q,
		"ttl_seconds": %d,
		"region": %q
	}`, testAccessKeyID, testSecretAccessKey, testSessionToken, expiration.UTC().Format(time.RFC3339), ttlSeconds, testRegion)
}

// errorBody builds the nested error envelope shape (mirrors
// credential_session.schema.json's "error" definition / helix-tools/api PR
// #129's error_handler.go shape).
func errorBody(code, message, requestID string) string {
	return fmt.Sprintf(`{"message": %q, "error": {"code": %q, "message": %q, "request_id": %q}}`,
		message, code, message, requestID)
}

func testBrokerConfig(endpoint string) BrokerConfig {
	return BrokerConfig{
		APIEndpoint:        endpoint,
		CustomerID:         "customer-test-1",
		Region:             testRegion,
		AWSAccessKeyID:     "AKIABOOTSTRAPTESTKEY",
		AWSSecretAccessKey: "bootstrapSecretAccessKeyForTests1234567",
		HTTPClient:         &http.Client{Timeout: 5 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// NewProvider validation (no network I/O — must fail fast, synchronously)
// ---------------------------------------------------------------------------

func TestNewProvider_ValidationErrors(t *testing.T) {
	base := testBrokerConfig("https://example.invalid")

	cases := []struct {
		name    string
		mutate  func(BrokerConfig) BrokerConfig
		wantErr string
	}{
		{
			name:    "missing_api_endpoint",
			mutate:  func(c BrokerConfig) BrokerConfig { c.APIEndpoint = ""; return c },
			wantErr: "APIEndpoint is required",
		},
		{
			name:    "missing_region",
			mutate:  func(c BrokerConfig) BrokerConfig { c.Region = ""; return c },
			wantErr: "Region is required",
		},
		{
			name:    "missing_access_key_id",
			mutate:  func(c BrokerConfig) BrokerConfig { c.AWSAccessKeyID = ""; return c },
			wantErr: "requires AWSAccessKeyID and AWSSecretAccessKey",
		},
		{
			name:    "missing_secret_access_key",
			mutate:  func(c BrokerConfig) BrokerConfig { c.AWSSecretAccessKey = ""; return c },
			wantErr: "requires AWSAccessKeyID and AWSSecretAccessKey",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProvider(tc.mutate(base))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}

	// Happy path: no error.
	if _, err := NewProvider(base); err != nil {
		t.Fatalf("expected valid BrokerConfig to construct cleanly, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Mint request signing (bootstrap SigV4, empty body)
// ---------------------------------------------------------------------------

func TestProvider_BuildRequest_SigV4SignedNoBody(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})

	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	req := broker.lastRequest()
	if req == nil {
		t.Fatal("broker never received a request")
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL.Path != MintPath {
		t.Errorf("path = %q, want %q", req.URL.Path, MintPath)
	}
	if req.ContentLength > 0 {
		t.Errorf("ContentLength = %d, want 0 (broker's request body is fully optional — PR #129 session_controller.go)", req.ContentLength)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("Authorization header = %q, want AWS4-HMAC-SHA256 SigV4 signature", auth)
	}
	if !strings.Contains(auth, "AKIABOOTSTRAPTESTKEY") {
		t.Errorf("Authorization header %q does not reference the bootstrap static access key", auth)
	}
	// The mint request itself must NEVER carry a session token — it is
	// signed with the plain static bootstrap key, not a vended session.
	if req.Header.Get("X-Amz-Security-Token") != "" {
		t.Errorf("mint request must not carry X-Amz-Security-Token (bootstrap is always static)")
	}
}

// ---------------------------------------------------------------------------
// Happy path + clock-skew hardening
// ---------------------------------------------------------------------------

func TestProvider_Retrieve_HappyPath(t *testing.T) {
	wantExpiry := time.Now().Add(15 * time.Minute)
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(wantExpiry, 900)
	})

	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	creds, err := p.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if creds.AccessKeyID != testAccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", creds.AccessKeyID, testAccessKeyID)
	}
	if creds.SecretAccessKey != testSecretAccessKey {
		t.Errorf("SecretAccessKey = %q, want %q", creds.SecretAccessKey, testSecretAccessKey)
	}
	if creds.SessionToken != testSessionToken {
		t.Errorf("SessionToken = %q, want %q", creds.SessionToken, testSessionToken)
	}
	if !creds.CanExpire {
		t.Error("CanExpire = false, want true for a vended STS session")
	}
	if creds.Source != "HelixCredentialBroker" {
		t.Errorf("Source = %q, want HelixCredentialBroker", creds.Source)
	}
	if broker.calls() != 1 {
		t.Errorf("broker calls = %d, want 1", broker.calls())
	}
}

// TestProvider_Retrieve_ClockSkewHardening pins the "local_now + ttl_seconds
// capped by parsed expiration" formula (STS-PLAN.md/C-sdk.md C.1 bullet 4)
// in BOTH directions: whichever bound is EARLIER always wins, so client/
// server clock drift can only ever shorten — never extend — a credential's
// effective lifetime beyond what the server granted.
func TestProvider_Retrieve_ClockSkewHardening(t *testing.T) {
	fixedNow := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	t.Run("server_expiration_earlier_than_local_plus_ttl_caps_down", func(t *testing.T) {
		// Server says the session expires in 2 minutes, but also claims a
		// generous 900s (15min) ttl_seconds — a client clock running slow
		// relative to the server (or a server that pre-dated the
		// expiration) must not extend past the earlier server bound.
		serverExpiry := fixedNow.Add(2 * time.Minute)
		broker := newFakeBroker(t, func(int) (int, string) {
			return http.StatusOK, successBody(serverExpiry, 900)
		})
		p, err := NewProvider(testBrokerConfig(broker.server.URL))
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		p.now = func() time.Time { return fixedNow }

		creds, err := p.Retrieve(context.Background())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if !creds.Expires.Equal(serverExpiry) {
			t.Errorf("Expires = %v, want the EARLIER server expiration %v (local_now+900s would have been %v)",
				creds.Expires, serverExpiry, fixedNow.Add(900*time.Second))
		}
	})

	t.Run("local_plus_ttl_earlier_than_server_expiration_caps_down", func(t *testing.T) {
		// Server's stated expiration is far in the future (e.g. clock skew
		// the other way), but ttl_seconds is short — local_now+ttl must win
		// since it is the earlier (more conservative) bound.
		serverExpiry := fixedNow.Add(1 * time.Hour)
		broker := newFakeBroker(t, func(int) (int, string) {
			return http.StatusOK, successBody(serverExpiry, 60) // 60s ttl
		})
		p, err := NewProvider(testBrokerConfig(broker.server.URL))
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		p.now = func() time.Time { return fixedNow }

		creds, err := p.Retrieve(context.Background())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		wantExpires := fixedNow.Add(60 * time.Second)
		if !creds.Expires.Equal(wantExpires) {
			t.Errorf("Expires = %v, want the EARLIER local_now+ttl_seconds bound %v (server expiration %v must NOT win)",
				creds.Expires, wantExpires, serverExpiry)
		}
	})
}

func TestProvider_Retrieve_NonPositiveTTLRejected(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(time.Minute), 0)
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("expected error for ttl_seconds=0, got nil")
	} else if !strings.Contains(err.Error(), "ttl_seconds") {
		t.Errorf("error = %q, want it to mention ttl_seconds", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Mint response contract validation
// ---------------------------------------------------------------------------

func TestProvider_Retrieve_MissingRequiredFields(t *testing.T) {
	full := map[string]string{
		"access_key_id":     testAccessKeyID,
		"secret_access_key": testSecretAccessKey,
		"session_token":     testSessionToken,
		"expiration":        time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		"ttl_seconds":       "900",
		"region":            testRegion,
	}
	fields := []string{"access_key_id", "secret_access_key", "session_token", "expiration", "ttl_seconds", "region"}

	for _, omit := range fields {
		t.Run("missing_"+omit, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("{")
			first := true
			for _, k := range fields {
				if k == omit {
					continue
				}
				if !first {
					b.WriteString(",")
				}
				first = false
				if k == "ttl_seconds" {
					fmt.Fprintf(&b, "%q: %s", k, full[k])
				} else {
					fmt.Fprintf(&b, "%q: %q", k, full[k])
				}
			}
			b.WriteString("}")

			broker := newFakeBroker(t, func(int) (int, string) {
				return http.StatusOK, b.String()
			})
			p, err := NewProvider(testBrokerConfig(broker.server.URL))
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			_, err = p.Retrieve(context.Background())
			if err == nil {
				t.Fatalf("expected error when %q is missing, got nil", omit)
			}
			if !strings.Contains(err.Error(), omit) {
				t.Errorf("error = %q, want it to name the missing field %q", err.Error(), omit)
			}
			// Must NOT retry a permanently-malformed response.
			if broker.calls() != 1 {
				t.Errorf("broker calls = %d, want 1 (malformed 2xx body must not be retried)", broker.calls())
			}
		})
	}
}

func TestProvider_Retrieve_MalformedJSON(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, `{"access_key_id": "ASIA` // truncated, invalid JSON
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if broker.calls() != 1 {
		t.Errorf("broker calls = %d, want 1 (malformed JSON must not be retried)", broker.calls())
	}
}

// ---------------------------------------------------------------------------
// 403 surfacing (typed errors, no retry) — explicit task requirement
// ---------------------------------------------------------------------------

func TestProvider_Retrieve_403SubscriptionExpired(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusForbidden, errorBody(ErrCodeSubscriptionExpired, "no active subscription", "req-abc-123")
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsSubscriptionExpired(err) {
		t.Errorf("IsSubscriptionExpired(err) = false for err = %v", err)
	}
	mErr, ok := err.(*MintError)
	if !ok {
		t.Fatalf("err type = %T, want *MintError", err)
	}
	if mErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", mErr.StatusCode)
	}
	if mErr.RequestID != "req-abc-123" {
		t.Errorf("RequestID = %q, want %q", mErr.RequestID, "req-abc-123")
	}
	// A definitive authz refusal must surface immediately — never retried.
	if broker.calls() != 1 {
		t.Errorf("broker calls = %d, want 1 (a typed 403 refusal must not be retried)", broker.calls())
	}
}

func TestProvider_Retrieve_403CustomerSuspended(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusForbidden, errorBody(ErrCodeCustomerSuspended, "company is suspended", "req-def-456")
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsCustomerSuspended(err) {
		t.Errorf("IsCustomerSuspended(err) = false for err = %v", err)
	}
	// Negative control: a subscription_expired-shaped error must NOT be
	// misclassified as customer_suspended — proves IsCustomerSuspended is
	// discriminating on the actual code, not vacuously true for any
	// *MintError.
	if IsSubscriptionExpired(err) {
		t.Error("IsSubscriptionExpired(err) = true for a customer_suspended error — code discrimination broken")
	}
}

func TestProvider_Retrieve_401Unauthorized(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusUnauthorized, `{"error": "unauthorized"}`
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if broker.calls() != 1 {
		t.Errorf("broker calls = %d, want 1 (401 must not be retried)", broker.calls())
	}
}

// ---------------------------------------------------------------------------
// Retry / backoff on transient failures
// ---------------------------------------------------------------------------

func TestProvider_Retrieve_429RetriesThenSucceeds(t *testing.T) {
	wantExpiry := time.Now().Add(15 * time.Minute)
	broker := newFakeBroker(t, func(n int) (int, string) {
		if n < mintMaxAttempts {
			return http.StatusTooManyRequests, `{"error":"rate_limited","message":"slow down"}`
		}
		return http.StatusOK, successBody(wantExpiry, 900)
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	start := time.Now()
	creds, err := p.Retrieve(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != testAccessKeyID {
		t.Errorf("AccessKeyID = %q, want %q", creds.AccessKeyID, testAccessKeyID)
	}
	if broker.calls() != mintMaxAttempts {
		t.Errorf("broker calls = %d, want exactly %d (success on the final attempt)", broker.calls(), mintMaxAttempts)
	}
	// Backoff between attempts must be observed (not an instant hot loop).
	if elapsed < mintRetryBaseDelay {
		t.Errorf("elapsed = %v, want >= %v (backoff must actually delay between attempts)", elapsed, mintRetryBaseDelay)
	}
}

func TestProvider_Retrieve_429RetriesExhausted(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusTooManyRequests, `{"error":"rate_limited","message":"slow down"}`
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if broker.calls() != mintMaxAttempts {
		t.Errorf("broker calls = %d, want exactly %d (mintMaxAttempts)", broker.calls(), mintMaxAttempts)
	}
}

func TestProvider_Retrieve_5xxRetryable(t *testing.T) {
	wantExpiry := time.Now().Add(15 * time.Minute)
	broker := newFakeBroker(t, func(n int) (int, string) {
		if n == 1 {
			return http.StatusInternalServerError, "internal error"
		}
		return http.StatusOK, successBody(wantExpiry, 900)
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if broker.calls() != 2 {
		t.Errorf("broker calls = %d, want 2 (one 500 then success)", broker.calls())
	}
}

func TestProvider_Retrieve_NonRetryable4xxDoesNotRetry(t *testing.T) {
	// 400 is neither a typed authz refusal nor 429/5xx — a permanent
	// client-error class that retrying cannot fix.
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusBadRequest, `{"error":"bad_request","message":"malformed scopes"}`
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("expected error, got nil")
	}
	if broker.calls() != 1 {
		t.Errorf("broker calls = %d, want 1 (400 must not be retried)", broker.calls())
	}
}

// TestProvider_Retrieve_NetworkErrorRetryable proves transport-level
// failures (not just HTTP status codes) are retried: the server closes the
// connection on the first attempt, then serves a normal 200.
func TestProvider_Retrieve_NetworkErrorRetryable(t *testing.T) {
	var n int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijacker", http.StatusInternalServerError)
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successBody(time.Now().Add(15*time.Minute), 900)))
	}))
	t.Cleanup(server.Close)

	p, err := NewProvider(testBrokerConfig(server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v (network error on first attempt should have been retried)", err)
	}
	if atomic.LoadInt32(&n) != 2 {
		t.Errorf("server hits = %d, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// SelectProvider — mode-inference / config-mode selection matrix
// ---------------------------------------------------------------------------

func TestSelectProvider_ModeMatrix(t *testing.T) {
	const defaultEndpoint = "https://api-go.helix.tools"

	cases := []struct {
		name string
		cfg  types.Config
		// endpoint overrides defaultEndpoint when non-empty — SelectProvider
		// resolves the mint target from its own apiEndpoint parameter, not
		// cfg.APIEndpoint (mirroring how producer.go/consumer.go call it:
		// SelectProvider(cfg.APIEndpoint, cfg)), so the transport-guard
		// cases below need to vary it independently of cfg.
		endpoint string
		wantErr  string // substring; empty means "no error"
		wantSTS  bool   // asserted only when wantErr == ""
	}{
		{
			name: "static_keys_only_mode_empty_infers_static",
			cfg: types.Config{
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
				Region:             testRegion,
			},
			wantSTS: false,
		},
		{
			name: "static_keys_explicit_static_mode",
			cfg: types.Config{
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
				Region:             testRegion,
				CredentialMode:     types.CredentialModeStatic,
			},
			wantSTS: false,
		},
		{
			name: "static_keys_explicit_sts_mode_bootstraps_via_static",
			cfg: types.Config{
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
				Region:             testRegion,
				CredentialMode:     types.CredentialModeSTS,
			},
			wantSTS: true,
		},
		{
			name:    "no_credentials_mode_empty_errors",
			cfg:     types.Config{Region: testRegion},
			wantErr: "no credentials configured",
		},
		{
			name: "no_static_keys_explicit_sts_mode_errors",
			cfg: types.Config{
				Region:         testRegion,
				CredentialMode: types.CredentialModeSTS,
			},
			wantErr: "requires AWSAccessKeyID and AWSSecretAccessKey",
		},
		{
			// Design §4.11, rule 1: explicit "sts" bootstraps with APIKey
			// when it is set — this is now a real, functioning bootstrap
			// path (superseding the pre-API-key placeholder behavior).
			name: "api_key_alone_explicit_sts_mode_bootstraps_via_key",
			cfg: types.Config{
				Region:         testRegion,
				APIKey:         testAPIKey,
				CredentialMode: types.CredentialModeSTS,
			},
			wantSTS: true,
		},
		{
			// Design §4.11, rule 2: with no mode set, APIKey alone infers
			// "sts via the key" — "sts" bootstrapped by a key IS inferred
			// (only bootstrap-by-static-key is never inferred).
			name: "api_key_alone_mode_empty_infers_sts_via_key",
			cfg: types.Config{
				Region: testRegion,
				APIKey: testAPIKey,
			},
			wantSTS: true,
		},
		{
			// Self-attack: an API key that is only whitespace (e.g. an env
			// var set to a single space) must be treated as absent, not as
			// a malformed-but-present key — falls through to the generic
			// no-credentials error exactly like a truly empty APIKey.
			name: "api_key_whitespace_only_treated_as_absent",
			cfg: types.Config{
				Region: testRegion,
				APIKey: "   \t  ",
			},
			wantErr: "no credentials configured",
		},
		{
			// Design §4.11 transport guard: an API key must never be sent
			// to a non-https, non-loopback endpoint. This must surface as
			// a construction error, not a network attempt.
			name:     "api_key_alone_insecure_endpoint_rejected",
			endpoint: "http://evil.example",
			cfg: types.Config{
				Region: testRegion,
				APIKey: testAPIKey,
			},
			wantErr: "refusing to send a Helix API key",
		},
		{
			// Bypass attempt for the transport guard: a hostname that only
			// CONTAINS "localhost" as a substring (a dotted suffix/prefix
			// lookalike) must NOT be treated as the localhost exception —
			// the match is exact, so this plain-http endpoint is rejected
			// exactly like any other non-https host.
			name:     "api_key_localhost_lookalike_hostname_rejected",
			endpoint: "http://localhost.evil.example",
			cfg: types.Config{
				Region: testRegion,
				APIKey: testAPIKey,
			},
			wantErr: "refusing to send a Helix API key",
		},
		{
			// Self-attack answer: https:// is unconditionally allowed
			// regardless of hostname (the key travels encrypted either
			// way), so this lookalike host is NOT a bypass of the guard —
			// it is allowed on the same basis as any other https endpoint.
			name:     "api_key_localhost_lookalike_hostname_allowed_over_https",
			endpoint: "https://localhost.evil.example",
			cfg: types.Config{
				Region: testRegion,
				APIKey: testAPIKey,
			},
			wantSTS: true,
		},
		{
			// Design §4.11 "no mode" rule: APIKey set + static keys also
			// set -> the key wins, static keys are ignored (warning
			// coverage lives in TestSelectProvider_Warnings).
			name: "api_key_and_static_keys_mode_empty_prefers_key",
			cfg: types.Config{
				Region:             testRegion,
				APIKey:             testAPIKey,
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
			},
			wantSTS: true,
		},
		{
			// Design §4.11, rule 1: explicit "sts" + both credential types
			// set -> the key still wins over static-key bootstrap.
			name: "api_key_and_static_keys_explicit_sts_mode_prefers_key",
			cfg: types.Config{
				Region:             testRegion,
				APIKey:             testAPIKey,
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
				CredentialMode:     types.CredentialModeSTS,
			},
			wantSTS: true,
		},
		{
			// Design §4.11, rule 1: explicit "static" + a set APIKey ->
			// APIKey is ignored, static keys are used (warning coverage in
			// TestSelectProvider_Warnings). Not an *aws.CredentialsCache —
			// same byte-identical static path as every other static case.
			name: "api_key_and_static_keys_explicit_static_mode_ignores_key",
			cfg: types.Config{
				Region:             testRegion,
				APIKey:             testAPIKey,
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
				CredentialMode:     types.CredentialModeStatic,
			},
			wantSTS: false,
		},
		{
			name: "explicit_static_mode_without_static_keys_errors",
			cfg: types.Config{
				Region:         testRegion,
				CredentialMode: types.CredentialModeStatic,
			},
			wantErr: "requires AWSAccessKeyID and AWSSecretAccessKey",
		},
		{
			name: "partial_static_keys_one_missing_errors",
			cfg: types.Config{
				AWSAccessKeyID: "AKIATESTKEY", // secret missing
				Region:         testRegion,
			},
			wantErr: "no credentials configured",
		},
		{
			name: "invalid_mode_string_errors",
			cfg: types.Config{
				AWSAccessKeyID:     "AKIATESTKEY",
				AWSSecretAccessKey: "testSecret",
				Region:             testRegion,
				CredentialMode:     types.CredentialMode("bogus"),
			},
			wantErr: "invalid CredentialMode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := tc.endpoint
			if endpoint == "" {
				endpoint = defaultEndpoint
			}
			provider, err := SelectProvider(endpoint, tc.cfg)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (provider=%v)", tc.wantErr, provider)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			_, isCache := provider.(*aws.CredentialsCache)
			if isCache != tc.wantSTS {
				t.Errorf("provider type = %T (is *aws.CredentialsCache: %v), want sts-mode=%v", provider, isCache, tc.wantSTS)
			}
		})
	}
}

// TestSelectProvider_StaticPath_ByteIdenticalToDirectConstruction is the
// Acme regression guard (C.5 test #2): SelectProvider's static branch
// must retrieve EXACTLY the same aws.Credentials as constructing
// credentials.NewStaticCredentialsProvider directly — proving the new
// mode-selection layer changes nothing observable for existing static
// callers.
func TestSelectProvider_StaticPath_ByteIdenticalToDirectConstruction(t *testing.T) {
	cfg := types.Config{
		AWSAccessKeyID:     "AKIAKNOWNVALUE1234",
		AWSSecretAccessKey: "knownSecretAccessKeyValue123456789",
		Region:             testRegion,
	}

	viaSelect, err := SelectProvider("https://api-go.helix.tools", cfg)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}
	gotCreds, err := viaSelect.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("viaSelect.Retrieve: %v", err)
	}

	direct := awscreds.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, "")
	wantCreds, err := direct.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("direct.Retrieve: %v", err)
	}

	if gotCreds != wantCreds {
		t.Errorf("SelectProvider static-mode credentials = %+v, want byte-identical to direct NewStaticCredentialsProvider %+v", gotCreds, wantCreds)
	}
	if gotCreds.SessionToken != "" {
		t.Errorf("static-mode SessionToken = %q, want empty (no X-Amz-Security-Token on the static path)", gotCreds.SessionToken)
	}

	// Negative control: an sts-mode result must NOT be byte-identical to
	// the static fixture above — proves the equality assertion is actually
	// discriminating credential shape, not vacuously true.
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})
	stsCfg := cfg
	stsCfg.CredentialMode = types.CredentialModeSTS
	viaSTS, err := SelectProvider(broker.server.URL, stsCfg)
	if err != nil {
		t.Fatalf("SelectProvider (sts): %v", err)
	}
	stsCreds, err := viaSTS.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("viaSTS.Retrieve: %v", err)
	}
	if stsCreds == wantCreds {
		t.Fatal("sts-mode credentials must NOT equal the static fixture — negative control failed, comparison is not discriminating")
	}
	if stsCreds.SessionToken == "" {
		t.Error("sts-mode SessionToken is empty, want the vended session token present")
	}
}

// ---------------------------------------------------------------------------
// aws.CredentialsCache refresh engine (real behavior, compressed real time)
// ---------------------------------------------------------------------------

// compressedCache builds a Provider + aws.CredentialsCache pair using a
// compressed real-time scale suitable for fast, deterministic tests: mints
// return a TTL of ttl (both ttl_seconds and expiration agree), the cache's
// ExpiryWindow is set to window with jitter DISABLED (frac=0) for
// determinism. Real aws.CredentialsCache — nothing about its own logic is
// mocked, only the time scale is shrunk from minutes to seconds.
func compressedCache(t *testing.T, window time.Duration, respond func(callNum int) (status int, body string)) (*fakeBroker, *aws.CredentialsCache) {
	t.Helper()
	broker := newFakeBroker(t, respond)
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cache := NewCredentialsCache(p, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = window
		o.ExpiryWindowJitterFrac = 0
	})
	return broker, cache
}

func mintingHandler(ttl time.Duration) func(int) (int, string) {
	return func(int) (int, string) {
		expiry := time.Now().Add(ttl)
		return http.StatusOK, successBody(expiry, int64(ttl.Seconds()))
	}
}

// minTestableRemaining is the floor below which a self-measured "remaining
// fresh time" is too small to reliably assert against in this environment
// (see the round-trip-latency note below) — a timing test that observes
// less than this fails loudly with a clear diagnostic instead of silently
// flaking.
const minTestableRemaining = 500 * time.Millisecond

// mintFreshAndMeasureRemaining performs the first Retrieve (a real mint),
// asserts it cost exactly one broker call, and returns how much of the
// cache-adjusted fresh window is left AT THE MOMENT Retrieve returned.
//
// Sleep durations in the tests below are computed relative to this
// self-measured value rather than a hardcoded constant, because the mint
// round-trip itself (SigV4 signing + one real HTTP round-trip, even against
// a local httptest server) is not free: observed up to several hundred ms
// in this environment on a cold connection. Provider.Retrieve's own
// clock-skew-hardening (Expires = min(local_now+ttl, server_expiration) —
// see broker.go) means that latency eats directly into the effective fresh
// window, since the server-computed expiration is captured BEFORE that
// latency elapses while local_now is captured AFTER. Hardcoding a fixed
// "sleep 300ms, assert still-fresh" assumption is exactly the kind of
// environment-dependent flakiness that produces a green test for the wrong
// reason; measuring the real remaining window and sleeping relative to it
// is robust to any round-trip latency.
func mintFreshAndMeasureRemaining(t *testing.T, ctx context.Context, broker *fakeBroker, cache *aws.CredentialsCache) time.Duration {
	t.Helper()
	creds, err := cache.Retrieve(ctx)
	if err != nil {
		t.Fatalf("initial Retrieve: %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("broker calls after first mint = %d, want 1", broker.calls())
	}
	remaining := time.Until(creds.Expires)
	if remaining < minTestableRemaining {
		t.Fatalf("observed fresh window remaining = %v, want >= %v — mint round-trip latency in this environment "+
			"ate too much of the configured TTL/window budget for this test to assert reliably; widen the test's "+
			"TTL/window parameters", remaining, minTestableRemaining)
	}
	return remaining
}

func TestCredentialsCache_FreshCredentials_NoRemint(t *testing.T) {
	// TTL 12s, window 9s -> nominal adjusted expiry ~3s after mint (actual
	// value self-measured below, since round-trip latency shifts it).
	// Immediately after minting, repeated Retrieve calls well inside the
	// observed fresh period must all serve the cached value.
	broker, cache := compressedCache(t, 9*time.Second, mintingHandler(12*time.Second))
	ctx := context.Background()

	remaining := mintFreshAndMeasureRemaining(t, ctx, broker, cache)

	time.Sleep(remaining / 2) // comfortably inside the observed fresh window
	for i := 0; i < 3; i++ {
		if _, err := cache.Retrieve(ctx); err != nil {
			t.Fatalf("Retrieve #%d: %v", i, err)
		}
	}
	if broker.calls() != 1 {
		t.Errorf("broker calls at ~50%% of the observed fresh window (remaining was %v) = %d, want 1 (no re-mint)", remaining, broker.calls())
	}
}

func TestCredentialsCache_WithinWindow_Remints(t *testing.T) {
	// Same compressed setup as above; sleeping PAST the self-measured
	// remaining fresh time (plus a safety margin) must trigger a re-mint on
	// the next Retrieve call — this is the "refresh at ~2/3 TTL" /
	// "proactive refresh when remaining <= 1/3 TTL" policy proven against
	// the REAL aws.CredentialsCache, not a reimplementation.
	broker, cache := compressedCache(t, 9*time.Second, mintingHandler(12*time.Second))
	ctx := context.Background()

	remaining := mintFreshAndMeasureRemaining(t, ctx, broker, cache)

	time.Sleep(remaining + 500*time.Millisecond)
	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatalf("Retrieve after window boundary: %v", err)
	}
	if broker.calls() != 2 {
		t.Errorf("broker calls after crossing the proactive window (remaining was %v) = %d, want 2 (re-mint expected)", remaining, broker.calls())
	}
}

// TestCredentialsCache_RefreshStorm_AtMostOneMintPerWindow proves the
// negative-control-required "refresh-storm regression": once a refresh has
// happened, a burst of immediately-following Retrieve calls must NOT mint
// again and again — at most one mint per window. The burst runs with no
// sleep at all (freshly minted a moment ago), so it needs no latency
// compensation.
func TestCredentialsCache_RefreshStorm_AtMostOneMintPerWindow(t *testing.T) {
	broker, cache := compressedCache(t, 9*time.Second, mintingHandler(12*time.Second))
	ctx := context.Background()

	remaining := mintFreshAndMeasureRemaining(t, ctx, broker, cache)
	time.Sleep(remaining + 500*time.Millisecond)
	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatalf("Retrieve after window boundary: %v", err)
	}
	if broker.calls() != 2 {
		t.Fatalf("broker calls after crossing the window once = %d, want 2", broker.calls())
	}

	// Rapid burst immediately after the refresh — freshly minted, so all of
	// these must be served from cache.
	for i := 0; i < 10; i++ {
		if _, err := cache.Retrieve(ctx); err != nil {
			t.Fatalf("burst Retrieve #%d: %v", i, err)
		}
	}
	if broker.calls() != 2 {
		t.Errorf("broker calls after a 10-call burst right after refresh = %d, want 2 (at most one mint per window)", broker.calls())
	}
}

// TestCredentialsCache_SingleFlight_ConcurrentCallsMintOnce proves N
// concurrent callers racing into an empty cache share exactly ONE in-flight
// mint (aws.CredentialsCache's own singleflight.Group, exercised through
// our Provider) — run with -race.
func TestCredentialsCache_SingleFlight_ConcurrentCallsMintOnce(t *testing.T) {
	broker := newFakeBroker(t, mintingHandler(15*time.Minute))
	broker.block = make(chan struct{})

	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cache := NewCredentialsCache(p)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]aws.Credentials, n)
	ctx := context.Background()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = cache.Retrieve(ctx)
		}(i)
	}

	// Give every goroutine a chance to reach the broker's blocking handler
	// before releasing it, to widen the race window as much as possible.
	time.Sleep(100 * time.Millisecond)
	close(broker.block)
	wg.Wait()

	if broker.calls() != 1 {
		t.Fatalf("broker calls with %d concurrent callers = %d, want exactly 1 (single-flight)", n, broker.calls())
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
		if results[i].AccessKeyID != testAccessKeyID {
			t.Errorf("goroutine %d: AccessKeyID = %q, want %q", i, results[i].AccessKeyID, testAccessKeyID)
		}
	}
}

// TestCredentialsCache_OptFnsOverrideDefaults proves optFns passed to
// NewCredentialsCache are applied AFTER this package's own defaults (so
// callers/tests can override them) rather than being silently clobbered.
// With ExpiryWindow forced to 0, a credential minted with a very short TTL
// must NOT be proactively refreshed before its true hard expiry — if the
// 5-minute package default were still in effect underneath, this near-
// instantly-expiring credential would incorrectly trigger a re-mint on the
// very next call.
func TestCredentialsCache_OptFnsOverrideDefaults(t *testing.T) {
	// TTL 8s, ExpiryWindow forced to 0 (disable proactive refresh entirely
	// — only the credential's own hard expiry matters). If the PACKAGE
	// DEFAULT 5-minute window were still in effect underneath (i.e. optFns
	// were not actually overriding it), this near-term credential would be
	// considered stale almost immediately and re-minted well before its
	// real hard expiry.
	broker := newFakeBroker(t, mintingHandler(8*time.Second))
	p, err := NewProvider(testBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cache := NewCredentialsCache(p, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = 0
	})
	ctx := context.Background()

	remaining := mintFreshAndMeasureRemaining(t, ctx, broker, cache)

	// Sleep to roughly the midpoint of the observed hard-expiry window —
	// still well before real expiry regardless of round-trip latency.
	time.Sleep(remaining / 2)
	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatalf("Retrieve before hard expiry: %v", err)
	}
	if broker.calls() != 1 {
		t.Errorf("broker calls = %d, want 1 (ExpiryWindow=0 override must suppress proactive refresh entirely, proving optFns win over package defaults)", broker.calls())
	}
}

// TestCredentialsCache_RideThroughBrokerBlipUntilHardExpiry proves the
// required "serve-last-good creds until hard expiry" behavior
// (STS-PLAN.md/C-sdk.md C.1 bullet 3 / R7: "a broker blip is invisible"):
// when a refresh becomes due (past the proactive window) but the true hard
// expiry from the last successful mint has NOT yet passed, a broker outage
// must NOT fail the caller — Retrieve rides through on the last-known-good
// credential. A second call immediately after must be served from that
// same ride-through cache entry without re-attempting a mint (no request
// storm during an outage). codex-REFUTE finding: an earlier revision of
// this provider failed closed as soon as the proactive window was crossed,
// not at true hard expiry — this test and
// TestCredentialsCache_FailClosedExpiry together pin the corrected,
// two-phase behavior end to end via the real aws.CredentialsCache (using
// Provider's HandleFailToRefresh/AdjustExpiresBy overrides — no
// reimplementation of the cache itself).
func TestCredentialsCache_RideThroughBrokerBlipUntilHardExpiry(t *testing.T) {
	const ttl = 6 * time.Second
	broker, cache := compressedCache(t, 4*time.Second, func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, successBody(time.Now().Add(ttl), int64(ttl.Seconds()))
		}
		return http.StatusInternalServerError, "broker down"
	})
	ctx := context.Background()
	mintStart := time.Now()

	remaining := mintFreshAndMeasureRemaining(t, ctx, broker, cache)
	if remaining >= ttl/2 {
		t.Fatalf("observed fresh window remaining = %v, too close to the %v hard TTL for this test's phases to be well separated", remaining, ttl)
	}

	// Cross the proactive window boundary (broker down) — must ride
	// through, not error, since we are still well before the ttl hard
	// expiry.
	time.Sleep(remaining + 500*time.Millisecond)
	rideThroughCreds, err := cache.Retrieve(ctx)
	if err != nil {
		t.Fatalf("expected a ride-through (no error) while still before hard expiry, got: %v", err)
	}
	if rideThroughCreds.AccessKeyID != testAccessKeyID {
		t.Errorf("ride-through AccessKeyID = %q, want the last-known-good %q", rideThroughCreds.AccessKeyID, testAccessKeyID)
	}
	callsAfterRideThrough := broker.calls()
	if callsAfterRideThrough < 2 {
		t.Fatalf("broker calls after the ride-through = %d, want >= 2 (the due refresh must have actually been attempted and failed before riding through)", callsAfterRideThrough)
	}

	// Immediately call again: must be served from the ride-through cache
	// entry (clamped to true hard expiry by AdjustExpiresBy), not
	// re-attempt a mint on every call during the outage.
	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatalf("Retrieve immediately after ride-through: %v", err)
	}
	if broker.calls() != callsAfterRideThrough {
		t.Errorf("broker calls after an immediate follow-up = %d, want unchanged at %d (ride-through must be cached until hard expiry, not re-attempted every call)", broker.calls(), callsAfterRideThrough)
	}

	// Now wait past the TRUE hard expiry (mintStart+ttl), broker still
	// down — see TestCredentialsCache_FailClosedExpiry for the
	// fail-closed assertion at that point.
	time.Sleep(time.Until(mintStart.Add(ttl)) + 700*time.Millisecond)
	if _, err := cache.Retrieve(ctx); err == nil {
		t.Error("expected an error once truly past hard expiry with the broker still down — ride-through must not extend forever")
	}
}

// TestCredentialsCache_FailClosedExpiry proves the required "fail-closed
// expiry" behavior at the OTHER boundary: once a credential's TRUE hard
// expiry has genuinely passed (not merely the proactive refresh window —
// see TestCredentialsCache_RideThroughBrokerBlipUntilHardExpiry for that
// distinction) and a refresh is still failing, Retrieve must return a clear
// error — never silently fabricate, zero-out, or indefinitely reuse an
// expired credential.
func TestCredentialsCache_FailClosedExpiry(t *testing.T) {
	const ttl = 2 * time.Second
	broker, cache := compressedCache(t, 1500*time.Millisecond, func(n int) (int, string) {
		if n == 1 {
			return http.StatusOK, successBody(time.Now().Add(ttl), int64(ttl.Seconds()))
		}
		// Every mint attempt after the first fails persistently — the
		// broker never recovers in this test, so ride-through (proven
		// separately above) must eventually exhaust into a hard failure.
		return http.StatusInternalServerError, "broker down"
	})
	ctx := context.Background()
	mintStart := time.Now()

	if _, err := cache.Retrieve(ctx); err != nil {
		t.Fatalf("initial Retrieve: %v", err)
	}
	if broker.calls() != 1 {
		t.Fatalf("broker calls after first mint = %d, want 1", broker.calls())
	}

	// Sleep past the TRUE hard expiry (not just the proactive window) with
	// the broker down throughout — ride-through has nothing left to extend.
	time.Sleep(time.Until(mintStart.Add(ttl)) + 700*time.Millisecond)

	creds, err := cache.Retrieve(ctx)
	if err == nil {
		t.Fatalf("expected an error once truly past hard expiry and the broker is still down, got credentials: %+v", creds)
	}
	if !strings.Contains(err.Error(), "failed to refresh cached credentials") && !strings.Contains(err.Error(), "mint failed") {
		t.Errorf("error = %q, want it to clearly indicate a refresh/mint failure (fail-closed, not a silent empty credential)", err.Error())
	}
	if creds.HasKeys() {
		t.Errorf("credentials returned alongside an error must not carry usable keys: %+v", creds)
	}
}

// ---------------------------------------------------------------------------
// Constants sanity (documents the TTL-floor derivation, catches accidental drift)
// ---------------------------------------------------------------------------

func TestConstants_RefreshPolicyDerivedFromTTLFloor(t *testing.T) {
	if sessionTTLSeconds != 900 {
		t.Fatalf("sessionTTLSeconds = %d, want 900 (credential_session.schema.json ttl_seconds const)", sessionTTLSeconds)
	}
	if proactiveExpiryWindow != 5*time.Minute {
		t.Errorf("proactiveExpiryWindow = %v, want 5m (900s/3 — 'refresh at ~2/3 TTL' / 'proactive when remaining <=1/3 TTL')", proactiveExpiryWindow)
	}
	if expiryWindowJitterFrac != 0.5 {
		t.Errorf("expiryWindowJitterFrac = %v, want 0.5 (STS-PLAN.md/C-sdk.md C.1 bullet 1, Go-specific figure)", expiryWindowJitterFrac)
	}
	if mintMaxAttempts != 3 {
		t.Errorf("mintMaxAttempts = %d, want 3 (1 initial + 2 retries — C.1 bullet 3 'mint retry 2x')", mintMaxAttempts)
	}
}

// ---------------------------------------------------------------------------
// API-key bootstrap (design §4.11)
// ---------------------------------------------------------------------------

// testAPIKey matches the real key shape from design §4.2 exactly ("hlx_" +
// 43 base64url characters, 47 total) so redactAPIKeys's pattern — which is
// anchored to that exact length — exercises the real matching logic rather
// than a shorter placeholder that would happen to not match. Deliberately
// low-entropy (repeated 'A', not random) so it reads as an obvious fixture
// and never trips a secret scanner's entropy heuristic on this source file.
const testAPIKey = "hlx_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testAPIKeyBrokerConfig(endpoint string) BrokerConfig {
	return BrokerConfig{
		APIEndpoint: endpoint,
		CustomerID:  "customer-test-1",
		Region:      testRegion,
		APIKey:      testAPIKey,
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	}
}

// TestProvider_BuildRequest_APIKeySentNoSigV4 is acceptance question 1's
// request-shape half: an API-key-bootstrapped mint must carry
// "Authorization: HLX-API-Key <key>" and NEVER a SigV4 signature or a
// session token on the mint request itself.
func TestProvider_BuildRequest_APIKeySentNoSigV4(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})

	p, err := NewProvider(testAPIKeyBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	req := broker.lastRequest()
	if req == nil {
		t.Fatal("broker never received a request")
	}
	wantAuth := "HLX-API-Key " + testAPIKey
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Errorf("Authorization = %q, want %q", got, wantAuth)
	}
	if strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		t.Error("Authorization header must not be a SigV4 signature for an API-key bootstrap")
	}
	if req.Header.Get("X-Amz-Security-Token") != "" {
		t.Error("API-key-bootstrapped mint request must not carry X-Amz-Security-Token")
	}
	if req.ContentLength > 0 {
		t.Errorf("ContentLength = %d, want 0", req.ContentLength)
	}
}

// TestNewProvider_APIKeyAloneIsSufficient proves BrokerConfig.APIKey alone
// (no AWSAccessKeyID/AWSSecretAccessKey at all) is a valid, complete
// bootstrap — the two credential paths are genuine alternatives, not
// APIKey-as-an-addition-to-static-keys.
func TestNewProvider_APIKeyAloneIsSufficient(t *testing.T) {
	cfg := testAPIKeyBrokerConfig("https://example.invalid")
	if cfg.AWSAccessKeyID != "" || cfg.AWSSecretAccessKey != "" {
		t.Fatal("test fixture must not set static keys")
	}
	if _, err := NewProvider(cfg); err != nil {
		t.Fatalf("NewProvider with APIKey alone: %v", err)
	}
}

// TestNewProvider_APIKeyTrimmed is the self-attack answer for "a key with
// surrounding whitespace or a trailing newline from an env file": it is
// trimmed, not rejected, and the TRIMMED value is what reaches the wire —
// never a raw value carrying a newline (which net/http's header writer
// would itself refuse to send as "invalid header field value").
func TestNewProvider_APIKeyTrimmed(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})

	cfg := testAPIKeyBrokerConfig(broker.server.URL)
	cfg.APIKey = "  " + testAPIKey + "\n"

	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	req := broker.lastRequest()
	if req == nil {
		t.Fatal("broker never received a request")
	}
	wantAuth := "HLX-API-Key " + testAPIKey
	if got := req.Header.Get("Authorization"); got != wantAuth {
		t.Errorf("Authorization = %q, want trimmed %q", got, wantAuth)
	}
}

// TestNewProvider_APIKeyWhitespaceOnlyRejected is the companion to the
// trim behavior above: a key that is NOTHING BUT whitespace trims to empty,
// which must fail the same "no credentials" validation as a truly absent
// APIKey — not silently send an empty Authorization value.
func TestNewProvider_APIKeyWhitespaceOnlyRejected(t *testing.T) {
	cfg := BrokerConfig{
		APIEndpoint: "https://example.invalid",
		Region:      testRegion,
		APIKey:      "   \t  ",
	}
	_, err := NewProvider(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "requires AWSAccessKeyID and AWSSecretAccessKey (or APIKey)") {
		t.Errorf("error = %q, want the no-bootstrap-credentials message", err.Error())
	}
}

// TestAllowsAPIKeyTransport is a direct unit test of the transport guard,
// including the self-attack question ("https://localhost.evil.example: is
// the guard fooled?" — no, https is unconditionally allowed) and its
// bypass-test counterpart (a plain-http lookalike host must still be
// rejected — the hostname match is exact, never a substring/suffix match).
func TestAllowsAPIKeyTransport(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		{"https://api-go.helix.tools", true},
		{"https://anything-at-all.example", true},
		{"https://localhost.evil.example", true}, // https is always safe transport, regardless of host
		{"http://localhost", true},
		{"http://localhost:8080", true},
		{"http://127.0.0.1", true},
		{"http://127.0.0.1:9090", true},
		{"http://evil.example", false},
		{"http://localhost.evil.example", false}, // bypass attempt: NOT exactly "localhost"
		{"http://evil.example.localhost", false}, // bypass attempt: "localhost" as a suffix, not the whole host
		{"http://1270.0.1", false},
		{"not a url at all \x00", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			got := allowsAPIKeyTransport(tc.endpoint)
			if got != tc.want {
				t.Errorf("allowsAPIKeyTransport(%q) = %v, want %v", tc.endpoint, got, tc.want)
			}
		})
	}
}

// TestNewProvider_RefusesInsecureEndpointForAPIKey is the NEGATIVE CONTROL
// for the transport guard: see broker_test.go's companion comment in the
// PR description for the revert-and-watch-it-fail evidence. With the guard
// in place, constructing a Provider for an API key against a plain-http,
// non-loopback endpoint must fail fast, with NO network attempt.
func TestNewProvider_RefusesInsecureEndpointForAPIKey(t *testing.T) {
	// A non-loopback, non-TLS host that is NOT network-reachable: this
	// proves the guard fires at construction, before any dial is attempted
	// — NewProvider performs no network I/O, so a DNS/connection failure
	// here would mean the guard let the request through.
	_, err := NewProvider(BrokerConfig{
		APIEndpoint: "http://credentials-guard-test.invalid",
		Region:      testRegion,
		APIKey:      testAPIKey,
	})
	if err == nil {
		t.Fatal("expected a transport-guard error, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to send a Helix API key") {
		t.Errorf("error = %q, want transport-guard message", err.Error())
	}
}

// TestMintError_FriendlyMessages is acceptance question 4: every one of the
// five server responses must surface the EXACT design §4.11 message, via
// Error(), and — because none of these five codes is in the retryable set
// (429/5xx) — must do so after exactly ONE broker call, never a retry
// storm (see also TestProvider_Retrieve_APIKeyRevoked_NoRetryStorm for the
// "race a refresh" self-attack angle on the revoked case specifically).
func TestMintError_FriendlyMessages(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		code       string
		wantMsg    string
	}{
		{
			name:       "401_key_rejected",
			statusCode: http.StatusUnauthorized,
			code:       "invalid credentials",
			wantMsg:    "Helix API key was rejected. Create a new key in the Helix portal under API Keys.",
		},
		{
			name:       "403_revoked",
			statusCode: http.StatusForbidden,
			code:       "api key revoked",
			wantMsg:    "This Helix API key has been revoked. Create a new key in the Helix portal under API Keys.",
		},
		{
			name:       "403_expired",
			statusCode: http.StatusForbidden,
			code:       "api_key_expired",
			wantMsg:    "This Helix API key has expired. Create a new key in the Helix portal under API Keys.",
		},
		{
			name:       "403_not_enabled",
			statusCode: http.StatusForbidden,
			code:       "feature not enabled: sts_broker",
			wantMsg:    "API keys are not enabled for this account yet. Keep using your AWS access keys, or contact Helix support.",
		},
		{
			name:       "403_retired",
			statusCode: http.StatusForbidden,
			code:       "static_credentials_retired",
			wantMsg:    "AWS access keys have been retired for this account. Configure apiKey (Helix API key) instead.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newFakeBroker(t, func(int) (int, string) {
				return tc.statusCode, errorBody(tc.code, "server detail that must not leak verbatim", "req-"+tc.name)
			})
			p, err := NewProvider(testAPIKeyBrokerConfig(broker.server.URL))
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			_, err = p.Retrieve(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tc.wantMsg {
				t.Errorf("Error() = %q, want exactly %q", err.Error(), tc.wantMsg)
			}
			if broker.calls() != 1 {
				t.Errorf("broker calls = %d, want 1 (a 401/403 must never be retried — no retry storm)", broker.calls())
			}

			mErr, ok := err.(*MintError)
			if !ok {
				t.Fatalf("err type = %T, want *MintError", err)
			}
			if !mErr.Friendly {
				t.Error("Friendly = false, want true for a mapped design-§4.11 message")
			}
		})
	}
}

// TestMintError_401FriendlyMessage_OnlyForAPIKeyBootstrap proves the 401
// mapping is scoped to the API-key bootstrap path: a SigV4-bootstrapped
// (static-key) mint that gets a plain 401 keeps its EXISTING generic error
// text unchanged — this PR must not alter today's static/SigV4 error
// behavior for any existing caller.
func TestMintError_401FriendlyMessage_OnlyForAPIKeyBootstrap(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusUnauthorized, `{"error": "unauthorized"}`
	})
	p, err := NewProvider(testBrokerConfig(broker.server.URL)) // static/SigV4 bootstrap
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "Helix API key") {
		t.Errorf("Error() = %q, must NOT use the API-key-specific message for a static-bootstrapped mint", err.Error())
	}
	mErr, ok := err.(*MintError)
	if !ok {
		t.Fatalf("err type = %T, want *MintError", err)
	}
	if mErr.Friendly {
		t.Error("Friendly = true, want false — this is today's existing unmapped static-bootstrap 401")
	}
}

// TestMintError_RedactsLeakedKeyInMessage is defense-in-depth for
// requirement 5 ("the key never appears in ... error text"): if a server
// bug ever echoed the submitted raw key back in an error body, the SDK
// must scrub it before it ever reaches a caller's error text or logs.
func TestMintError_RedactsLeakedKeyInMessage(t *testing.T) {
	leaking := "rejected key " + testAPIKey + " was not recognized"
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusForbidden, errorBody("some_other_code", leaking, "req-leak")
	})
	p, err := NewProvider(testAPIKeyBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("Error() = %q, leaked the raw API key", err.Error())
	}
	if !strings.Contains(err.Error(), "hlx_<redacted>") {
		t.Errorf("Error() = %q, want the redacted placeholder in place of the leaked key", err.Error())
	}
}

// TestProvider_Retrieve_APIKeyRevoked_NoRetryStorm is the self-attack
// answer for "a refresh that races a revoke": a freshly-constructed
// Provider (no prior successful mint to ride through on) that gets 403
// "api key revoked" on its FIRST Retrieve surfaces that exact message
// immediately — one broker call, no retry, nothing resembling an infinite
// retry loop.
func TestProvider_Retrieve_APIKeyRevoked_NoRetryStorm(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusForbidden, errorBody("api key revoked", "revoked", "req-revoke-1")
	})
	p, err := NewProvider(testAPIKeyBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	wantMsg := "This Helix API key has been revoked. Create a new key in the Helix portal under API Keys."
	if err.Error() != wantMsg {
		t.Errorf("Error() = %q, want %q", err.Error(), wantMsg)
	}
	if broker.calls() != 1 {
		t.Fatalf("broker calls = %d, want exactly 1 (no retry storm on a revoked key)", broker.calls())
	}

	// A second, independent Retrieve (simulating a later refresh attempt
	// after the cache's window elapses) must surface the SAME message
	// again, not something different and not panic/hang.
	_, err = p.Retrieve(context.Background())
	if err == nil || err.Error() != wantMsg {
		t.Errorf("second Retrieve error = %v, want the same revoked message", err)
	}
	if broker.calls() != 2 {
		t.Errorf("broker calls after second Retrieve = %d, want 2 (one call per Retrieve, still no retry storm)", broker.calls())
	}
}

// TestProvider_Retrieve_APIKey_MultipleCallsAlwaysMintWithKey is acceptance
// question 5's "and use them for every later call" half, at the Provider
// level: a Provider configured with ONLY an API key must mint via the key
// EVERY time Retrieve is invoked (as aws.CredentialsCache does on every
// due refresh) — never fall back to a different bootstrap on a later call.
func TestProvider_Retrieve_APIKey_MultipleCallsAlwaysMintWithKey(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusOK, successBody(time.Now().Add(15*time.Minute), 900)
	})
	p, err := NewProvider(testAPIKeyBrokerConfig(broker.server.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if _, err := p.Retrieve(context.Background()); err != nil {
			t.Fatalf("Retrieve #%d: %v", i, err)
		}
		req := broker.lastRequest()
		wantAuth := "HLX-API-Key " + testAPIKey
		if got := req.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("call #%d: Authorization = %q, want %q", i, got, wantAuth)
		}
	}
	if broker.calls() != 3 {
		t.Errorf("broker calls = %d, want 3", broker.calls())
	}
}

// ---------------------------------------------------------------------------
// Mode-resolution warnings (design §4.11's "exactly one warning" rule)
// ---------------------------------------------------------------------------

// captureWarnings temporarily redirects warningWriter to a buffer, mirroring
// producer.deprecationWriter's test pattern, and returns a restore func.
func captureWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := warningWriter
	warningWriter = &buf
	t.Cleanup(func() { warningWriter = prev })
	return &buf
}

func countOccurrences(s, substr string) int {
	return strings.Count(s, substr)
}

// TestSelectProvider_Warnings covers acceptance questions 2 and 3's warning
// half ("exactly one warning is emitted"), for both directions of the
// conflict, plus a negative control proving the capture mechanism itself
// can observe ZERO warnings when only one credential type is configured
// (i.e. the assertion isn't vacuously true for any output).
func TestSelectProvider_Warnings(t *testing.T) {
	t.Run("both_set_no_mode_warns_once_static_ignored", func(t *testing.T) {
		buf := captureWarnings(t)
		cfg := types.Config{
			Region:             testRegion,
			APIKey:             testAPIKey,
			AWSAccessKeyID:     "AKIATESTKEY",
			AWSSecretAccessKey: "testSecret",
		}
		if _, err := SelectProvider("https://api-go.helix.tools", cfg); err != nil {
			t.Fatalf("SelectProvider: %v", err)
		}
		if n := countOccurrences(buf.String(), warnStaticKeysIgnoredAPIKeySet); n != 1 {
			t.Errorf("warning count = %d, want exactly 1 (output: %q)", n, buf.String())
		}
	})

	t.Run("static_mode_plus_key_warns_once_key_ignored", func(t *testing.T) {
		buf := captureWarnings(t)
		cfg := types.Config{
			Region:             testRegion,
			APIKey:             testAPIKey,
			AWSAccessKeyID:     "AKIATESTKEY",
			AWSSecretAccessKey: "testSecret",
			CredentialMode:     types.CredentialModeStatic,
		}
		if _, err := SelectProvider("https://api-go.helix.tools", cfg); err != nil {
			t.Fatalf("SelectProvider: %v", err)
		}
		if n := countOccurrences(buf.String(), warnAPIKeyIgnoredStaticMode); n != 1 {
			t.Errorf("warning count = %d, want exactly 1 (output: %q)", n, buf.String())
		}
	})

	t.Run("sts_mode_plus_both_warns_once_static_ignored", func(t *testing.T) {
		buf := captureWarnings(t)
		cfg := types.Config{
			Region:             testRegion,
			APIKey:             testAPIKey,
			AWSAccessKeyID:     "AKIATESTKEY",
			AWSSecretAccessKey: "testSecret",
			CredentialMode:     types.CredentialModeSTS,
		}
		if _, err := SelectProvider("https://api-go.helix.tools", cfg); err != nil {
			t.Fatalf("SelectProvider: %v", err)
		}
		if n := countOccurrences(buf.String(), warnStaticKeysIgnoredAPIKeySet); n != 1 {
			t.Errorf("warning count = %d, want exactly 1 (output: %q)", n, buf.String())
		}
	})

	// Negative control: proves the capture mechanism actually detects
	// ABSENCE, not just presence — a config with only one credential type
	// set must emit NO warning at all.
	t.Run("negative_control_single_credential_no_warning", func(t *testing.T) {
		buf := captureWarnings(t)
		cfg := types.Config{
			Region: testRegion,
			APIKey: testAPIKey,
		}
		if _, err := SelectProvider("https://api-go.helix.tools", cfg); err != nil {
			t.Fatalf("SelectProvider: %v", err)
		}
		if buf.String() != "" {
			t.Errorf("warnings = %q, want none when only one credential type is configured", buf.String())
		}
	})
}

// ---------------------------------------------------------------------------
// No silent fallback from a failed API-key mint to static keys
// ---------------------------------------------------------------------------

// TestSelectProvider_NoSilentFallback_OnMintFailure is acceptance questions
// 4 and 5's "never falls back to static" half: even when VALID static keys
// are also configured, a failed API-key mint must surface the broker's
// error — never silently retry with, or switch to, the static keys.
func TestSelectProvider_NoSilentFallback_OnMintFailure(t *testing.T) {
	broker := newFakeBroker(t, func(int) (int, string) {
		return http.StatusForbidden, errorBody("api key revoked", "revoked", "req-no-fallback")
	})

	cfg := types.Config{
		Region:             testRegion,
		APIKey:             testAPIKey,
		AWSAccessKeyID:     "AKIAVALIDSTATICKEY1",
		AWSSecretAccessKey: "aValidStaticSecretAccessKeyThatWouldWork12",
	}
	_ = captureWarnings(t) // silence the expected "static keys ignored" warning from this test's output

	provider, err := SelectProvider(broker.server.URL, cfg)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}

	_, err = provider.Retrieve(context.Background())
	if err == nil {
		t.Fatal("expected the mint failure to surface, got nil — looks like a silent fallback occurred")
	}
	// provider is the *aws.CredentialsCache SelectProvider returns, so
	// aws-sdk-go-v2's own cache wraps the underlying *MintError with its
	// own "failed to refresh cached credentials, ..." prefix (the same
	// wrapping every other error type already gets at this layer) — assert
	// the exact design §4.11 text is PRESENT, not that it's the entire
	// string.
	wantMsg := "This Helix API key has been revoked. Create a new key in the Helix portal under API Keys."
	if !strings.Contains(err.Error(), wantMsg) {
		t.Errorf("Error() = %q, want it to contain %q", err.Error(), wantMsg)
	}

	// Exactly one request must have reached the broker, and it must be the
	// API-key mint — never a second, SigV4-signed attempt using the valid
	// static keys.
	if broker.calls() != 1 {
		t.Fatalf("broker calls = %d, want 1 (no fallback retry with static keys)", broker.calls())
	}
	req := broker.lastRequest()
	if strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		t.Error("the single broker request must never be SigV4-signed — that would mean a fallback attempt occurred")
	}
}

// ---------------------------------------------------------------------------
// Secret redaction in string representations (requirement 5)
// ---------------------------------------------------------------------------

func TestBrokerConfig_String_RedactsAPIKey(t *testing.T) {
	cfg := testAPIKeyBrokerConfig("https://api-go.helix.tools")
	cfg.AWSAccessKeyID = "AKIA-VISIBLE-NOT-SECRET"
	cfg.AWSSecretAccessKey = "aStaticSecretThatMustNeverBePrinted12345"

	out := fmt.Sprintf("%v", cfg)
	if strings.Contains(out, testAPIKey) {
		t.Fatalf("String() = %q, leaked the raw API key", out)
	}
	if strings.Contains(out, cfg.AWSSecretAccessKey) {
		t.Fatalf("String() = %q, leaked the raw AWS secret access key", out)
	}
	if n := strings.Count(out, "<redacted>"); n != 2 {
		t.Errorf("String() = %q, want exactly 2 redacted placeholders (APIKey and AWSSecretAccessKey), got %d", out, n)
	}
	// Negative control: a non-secret field must still be visible — proves
	// String() targets specific fields rather than redacting everything
	// (which would make the "leaked" assertions above vacuously true).
	if !strings.Contains(out, "AKIA-VISIBLE-NOT-SECRET") {
		t.Errorf("String() = %q, want the non-secret AWSAccessKeyID still visible", out)
	}

	outPlus := fmt.Sprintf("%+v", cfg)
	if strings.Contains(outPlus, testAPIKey) || strings.Contains(outPlus, cfg.AWSSecretAccessKey) {
		t.Fatalf("%%+v = %q, leaked a secret", outPlus)
	}
}

func TestProvider_String_RedactsAPIKey(t *testing.T) {
	p, err := NewProvider(testAPIKeyBrokerConfig("https://api-go.helix.tools"))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	out := fmt.Sprintf("%v", p)
	if strings.Contains(out, testAPIKey) {
		t.Fatalf("String() = %q, leaked the raw API key", out)
	}

	outPlus := fmt.Sprintf("%+v", p)
	if strings.Contains(outPlus, testAPIKey) {
		t.Fatalf("%%+v = %q, leaked the raw API key", outPlus)
	}
}
