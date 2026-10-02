package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Echoed secrets in credential-service errors (v2.20.2).
//
// If the credential service ever echoes the request's own Authorization
// header (the raw Helix API key, or a SigV4 signature that can be replayed
// for a few minutes) or session token back in an error body, the SDK must not
// copy it into the error it returns, any error that error wraps, or anything
// it writes out. The echo server below reflects exactly what the SDK sent, so
// the secrets under test are the real ones on the wire, not planted copies.

// shortTestAPIKey is a key the hlx_ shape pattern (redactAPIKeys) does NOT
// match, so only exact-value scrubbing can keep it out of an error.
const shortTestAPIKey = "hlx_shortTESTkeyEXAMPLE"

// echoBroker answers every mint with status and an error envelope whose
// message, code and request_id each echo the request's Authorization and
// X-Amz-Security-Token header values. It records the last request's headers.
type echoBroker struct {
	server *httptest.Server
	raw    bool

	mu      sync.Mutex
	headers http.Header
}

func newEchoBroker(t *testing.T, status int, raw bool) *echoBroker {
	t.Helper()
	b := &echoBroker{raw: raw}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.headers = r.Header.Clone()
		b.mu.Unlock()

		echo := "rejected " + r.Header.Get("Authorization") + " token " + r.Header.Get("X-Amz-Security-Token")
		if b.raw {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(echo))
			return
		}
		var envelope mintErrorResponse
		envelope.Message = echo
		envelope.Error.Code = echo
		envelope.Error.Message = echo
		envelope.Error.RequestID = echo
		body, _ := json.Marshal(envelope)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(b.server.Close)
	return b
}

// sentSecrets returns every secret value the SDK put on the last request:
// the whole Authorization value, the SigV4 signature inside it, and the
// session token. apiKey is added by the caller when the bootstrap used one.
func (b *echoBroker) sentSecrets(t *testing.T) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.headers == nil {
		t.Fatal("broker never received a request")
	}
	auth := b.headers.Get("Authorization")
	if auth == "" {
		t.Fatal("request carried no Authorization header")
	}
	secrets := []string{auth}
	if i := strings.LastIndex(auth, "Signature="); i >= 0 {
		secrets = append(secrets, auth[i+len("Signature="):])
	}
	if token := b.headers.Get("X-Amz-Security-Token"); token != "" {
		secrets = append(secrets, token)
	}
	return secrets
}

// errorChain returns err and every error it wraps, following both
// Unwrap() error and Unwrap() []error.
func errorChain(err error) []error {
	var out []error
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		out = append(out, e)
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
	return out
}

// captureOutput runs fn with os.Stdout, os.Stderr and warningWriter all
// redirected, and returns everything written to them.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	var warnings bytes.Buffer
	oldStdout, oldStderr, oldWarn := os.Stdout, os.Stderr, warningWriter
	os.Stdout, os.Stderr, warningWriter = w, w, &warnings
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()
	defer func() {
		os.Stdout, os.Stderr, warningWriter = oldStdout, oldStderr, oldWarn
	}()
	fn()
	_ = w.Close()
	return string(<-done) + warnings.String()
}

func assertNoSecrets(t *testing.T, err error, output string, secrets []string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a mint error")
	}
	chain := errorChain(err)
	for _, secret := range secrets {
		for i, e := range chain {
			if strings.Contains(e.Error(), secret) {
				t.Errorf("error chain[%d] (%T) leaks %q: %q", i, e, secret, e.Error())
			}
		}
		var me *MintError
		if errors.As(err, &me) {
			for name, field := range map[string]string{"Code": me.Code, "Message": me.Message, "RequestID": me.RequestID} {
				if strings.Contains(field, secret) {
					t.Errorf("MintError.%s leaks %q: %q", name, secret, field)
				}
			}
		}
		if strings.Contains(output, secret) {
			t.Errorf("captured output leaks %q: %q", secret, output)
		}
	}
}

func TestMint_EchoedSecretsNeverReachErrorsOrOutput(t *testing.T) {
	cases := []struct {
		name   string
		cfg    func(endpoint string) BrokerConfig
		apiKey string
	}{
		{"sigv4", testBrokerConfig, ""},
		{"api key", testAPIKeyBrokerConfig, testAPIKey},
		{"short api key", func(endpoint string) BrokerConfig {
			cfg := testAPIKeyBrokerConfig(endpoint)
			cfg.APIKey = shortTestAPIKey
			return cfg
		}, shortTestAPIKey},
	}
	for _, tc := range cases {
		for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
			for _, raw := range []bool{false, true} {
				name := tc.name + "/" + http.StatusText(status)
				if raw {
					name += "/raw body"
				}
				t.Run(name, func(t *testing.T) {
					broker := newEchoBroker(t, status, raw)
					p, err := NewProvider(tc.cfg(broker.server.URL))
					if err != nil {
						t.Fatalf("NewProvider: %v", err)
					}
					p.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

					var retrieveErr error
					output := captureOutput(t, func() {
						_, retrieveErr = p.Retrieve(context.Background())
					})

					secrets := broker.sentSecrets(t)
					if tc.apiKey != "" {
						secrets = append(secrets, tc.apiKey)
					}
					assertNoSecrets(t, retrieveErr, output, secrets)
					if !strings.Contains(retrieveErr.Error(), "<redacted>") {
						t.Errorf("Error() = %q, want the echoed secret replaced by <redacted>", retrieveErr.Error())
					}
				})
			}
		}
	}
}

// TestMint_EchoedSecrets_LatchUnchanged pins that scrubbing does not change
// which failures latch: a 401 still latches, an unmapped 403 and a 500 never
// do, even when every field of the body echoes a secret.
func TestMint_EchoedSecrets_LatchUnchanged(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusUnauthorized:        true,
		http.StatusForbidden:           false,
		http.StatusInternalServerError: false,
	} {
		broker := newEchoBroker(t, status, false)
		p, err := NewProvider(testBrokerConfig(broker.server.URL))
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		p.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		_, err = p.Retrieve(context.Background())
		if got := isLatchableMintError(err); got != want {
			t.Errorf("status %d: latchable = %v, want %v", status, got, want)
		}
	}
}

// TestRequestSecrets_IncludesSessionToken covers the session-token half: the
// mint request never carries one today (static bootstrap signs with no
// token, the API-key path sends none), so it is checked on a request built
// here — any token a request does carry is scrubbed, longest value first.
func TestRequestSecrets_IncludesSessionToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.example.test"+MintPath, nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/x, SignedHeaders=host, Signature=abc123")
	req.Header.Set("X-Amz-Security-Token", "sessionTOKENexample")

	got := scrubSecrets("a=AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/x, SignedHeaders=host, Signature=abc123 s=abc123 t=sessionTOKENexample",
		requestSecrets(req, ""))
	if want := "a=<redacted> s=<redacted> t=<redacted>"; got != want {
		t.Errorf("scrubSecrets = %q, want %q", got, want)
	}
	if got := scrubSecrets("nothing secret here", requestSecrets(httptest.NewRequest(http.MethodPost, "/", nil), "")); got != "nothing secret here" {
		t.Errorf("scrubSecrets with no secrets = %q, want the input unchanged", got)
	}
}

// A 200 whose fields are otherwise valid but whose expiration echoes the
// request's Authorization value: the parse error, and every error it wraps,
// must not carry the header (the API key, or a replayable SigV4 signature).
func TestMint_200EchoedExpiration_NeverLeaks(t *testing.T) {
	cases := []struct {
		name   string
		cfg    func(endpoint string) BrokerConfig
		apiKey string
	}{
		{"sigv4", testBrokerConfig, ""},
		{"api key", testAPIKeyBrokerConfig, testAPIKey},
		{"short api key", func(endpoint string) BrokerConfig {
			cfg := testAPIKeyBrokerConfig(endpoint)
			cfg.APIKey = shortTestAPIKey
			return cfg
		}, shortTestAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var auth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				auth = r.Header.Get("Authorization")
				mu.Unlock()
				body, _ := json.Marshal(mintSuccessResponse{
					AccessKeyID: "ASIATEMP", SecretAccessKey: "tempSecret", SessionToken: "tempToken",
					Expiration: r.Header.Get("Authorization"), TTLSeconds: 900, Region: testRegion,
				})
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			t.Cleanup(server.Close)

			p, err := NewProvider(tc.cfg(server.URL))
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			var retrieveErr error
			output := captureOutput(t, func() {
				_, retrieveErr = p.Retrieve(context.Background())
			})

			mu.Lock()
			secrets := []string{auth}
			mu.Unlock()
			if i := strings.LastIndex(auth, "Signature="); i >= 0 {
				secrets = append(secrets, auth[i+len("Signature="):])
			}
			if tc.apiKey != "" {
				secrets = append(secrets, tc.apiKey)
			}
			assertNoSecrets(t, retrieveErr, output, secrets)
			if !strings.Contains(retrieveErr.Error(), "unparseable expiration") || !strings.Contains(retrieveErr.Error(), "<redacted>") {
				t.Errorf("Error() = %q, want the unparseable-expiration error with the echo replaced by <redacted>", retrieveErr.Error())
			}
		})
	}
}

// Classification reads the RAW response: an API key that happens to equal
// the 403's error.code must not stop the rejection from latching.
func TestMint_SecretEqualToLatchCode_StillLatches(t *testing.T) {
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		writeMintError(w, http.StatusForbidden, "api_key_revoked", "forbidden")
	}))
	t.Cleanup(server.Close)

	cfg := testAPIKeyBrokerConfig(server.URL)
	cfg.APIKey = "api_key_revoked"
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, err1 := p.Retrieve(context.Background())
	_, err2 := p.Retrieve(context.Background())
	if !isLatchableMintError(err1) {
		t.Errorf("first error latchable = false, want true: %v", err1)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("mint calls = %d, want 1 (second Retrieve must return the latched rejection)", calls)
	}
	if err2 == nil || err2.Error() != err1.Error() {
		t.Errorf("second error = %v, want the latched %v", err2, err1)
	}
	var me *MintError
	if !errors.As(err1, &me) || strings.Contains(me.Code, cfg.APIKey) {
		t.Errorf("surfaced MintError.Code = %q, want the echoed key scrubbed", me.Code)
	}
}

// Classification reads the RAW response: an API key that happens to appear in
// a 429's "retry after N seconds" must not change the parsed wait.
func TestMint_SecretInsideRetryAfterMessage_WaitUnchanged(t *testing.T) {
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			writeMintError(w, http.StatusTooManyRequests, "rate_limited", "retry after 5 seconds")
			return
		}
		body, _ := json.Marshal(mintSuccessResponse{
			AccessKeyID: "ASIATEMP", SecretAccessKey: "tempSecret", SessionToken: "tempToken",
			Expiration: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), TTLSeconds: 900, Region: testRegion,
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	cfg := testAPIKeyBrokerConfig(server.URL)
	cfg.APIKey = "5"
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	var waits []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(waits) != 1 || waits[0] != 5*time.Second {
		t.Errorf("waits = %v, want exactly [5s]", waits)
	}
}

// writeMintError answers with status and an error envelope carrying code and
// message, the shape helix-tools/api returns.
func writeMintError(w http.ResponseWriter, status int, code, message string) {
	var envelope mintErrorResponse
	envelope.Error.Code = code
	envelope.Error.Message = message
	envelope.Error.RequestID = "req-test-1"
	body, _ := json.Marshal(envelope)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// scrubError keeps an error (and its cause chain) untouched when it carries
// no secret, and drops the cause when it does — the cause would still hold
// the raw value.
func TestScrubError(t *testing.T) {
	_, cause := time.Parse(time.RFC3339, "not-a-time")
	clean := fmt.Errorf("credentials: broker returned unparseable expiration %q: %w", "not-a-time", cause)
	if got := scrubError(clean, []string{"s3cret"}); got != clean {
		t.Errorf("scrubError(no secret) = %v, want the same error", got)
	}
	var pe *time.ParseError
	if !errors.As(scrubError(clean, []string{"s3cret"}), &pe) {
		t.Error("scrubError(no secret) lost the *time.ParseError cause")
	}

	_, cause = time.Parse(time.RFC3339, "x s3cret y")
	leaky := fmt.Errorf("credentials: broker returned unparseable expiration %q: %w", "x s3cret y", cause)
	got := scrubError(leaky, []string{"s3cret"})
	for i, e := range errorChain(got) {
		if strings.Contains(e.Error(), "s3cret") {
			t.Errorf("chain[%d] (%T) leaks: %q", i, e, e.Error())
		}
	}
	if !strings.Contains(got.Error(), "unparseable expiration") || !strings.Contains(got.Error(), "<redacted>") {
		t.Errorf("scrubError(secret) = %q, want the message with <redacted>", got.Error())
	}
	if !strings.Contains(scrubError(errors.New("key "+testAPIKey), nil).Error(), "hlx_<redacted>") {
		t.Error("scrubError did not apply the hlx_ pattern redaction")
	}
}

// A 200 whose JSON cannot be decoded, with an API key that the decoder's
// error text happens to contain (it quotes a non-integer number literal),
// never surfaces the key.
func TestMint_200MalformedJSONEchoingKey_NeverLeaks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_key_id":"a","secret_access_key":"b","session_token":"c","expiration":"2026-10-02T00:00:00Z","ttl_seconds":555777.5,"region":"us-east-1"}`))
	}))
	t.Cleanup(server.Close)
	cfg := testAPIKeyBrokerConfig(server.URL)
	cfg.APIKey = "555777.5"
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, err = p.Retrieve(context.Background())
	assertNoSecrets(t, err, "", []string{"555777.5"})
	if err != nil && !strings.Contains(err.Error(), "malformed mint response JSON") {
		t.Errorf("Error() = %q, want the malformed-JSON error", err.Error())
	}
}
