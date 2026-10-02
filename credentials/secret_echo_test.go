package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
