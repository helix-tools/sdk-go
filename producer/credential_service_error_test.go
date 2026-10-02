package producer

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	stscreds "github.com/helix-tools/sdk-go/v2/credentials"
	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/types"
)

// credentialServiceAnswering stands in for the Helix credential service,
// answering every mint request with status and body, and counts the calls.
func credentialServiceAnswering(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestNewProducer_APIKeyServiceErrorSurfacesServiceMessage: an API-key
// caller whose credential service keeps answering 500 or 503 sees the
// service's error after the retries, not "invalid AWS credentials".
func TestNewProducer_APIKeyServiceErrorSurfacesServiceMessage(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"500 typed", http.StatusInternalServerError,
			`{"error":{"code":"internal_error","message":"something went wrong","request_id":"r-1"}}`,
			"Helix credential service error (internal_error): something went wrong"},
		{"503 untyped", http.StatusServiceUnavailable, `upstream unavailable`,
			"Helix credential service error: upstream unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")
			service, calls := credentialServiceAnswering(t, tc.status, tc.body)

			p, err := NewProducer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cust-1"})

			if err == nil || p != nil {
				t.Fatalf("NewProducer = %v, %v; want nil and an error", p, err)
			}
			if err.Error() != tc.want {
				t.Errorf("Error() = %q, want %q", err.Error(), tc.want)
			}
			if strings.Contains(err.Error(), "AWS") {
				t.Errorf("Error() = %q mentions AWS; the credential service failed, not AWS keys", err.Error())
			}
			if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
				t.Error("the service answered, so it is not unreachable")
			}
			var mintErr *stscreds.MintError
			if !errors.As(err, &mintErr) {
				t.Error("errors.As(*MintError) = false; the service's error must stay reachable for debugging")
			}
			if got := calls.Load(); got != 3 {
				t.Errorf("credential service called %d time(s), want 3", got)
			}
		})
	}
}

// TestNewProducer_APIKeyServiceErrorEchoingKeyIsScrubbed: a service error
// body that repeats the caller's API key never shows it in the new message.
func TestNewProducer_APIKeyServiceErrorEchoingKeyIsScrubbed(t *testing.T) {
	isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")
	service, _ := credentialServiceAnswering(t, http.StatusInternalServerError,
		`{"error":{"code":"bad_`+testAPIKeyForErrors+`","message":"failed for HLX-API-Key `+testAPIKeyForErrors+`"}}`)

	_, err := NewProducer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cust-1"})

	if err == nil || !strings.HasPrefix(err.Error(), "Helix credential service error (") {
		t.Fatalf("NewProducer error = %v, want the credential service error", err)
	}
	if strings.Contains(err.Error(), testAPIKeyForErrors) || strings.Contains(err.Error(), "credentialErrorTestKey") {
		t.Errorf("Error() = %q leaks the API key", err.Error())
	}
}

// TestNewProducer_STSStaticKeys: a static-key STS-mode caller sees the
// service's error for a 500, but keeps "invalid AWS credentials" when the
// service rejects their AWS keys (401/403).
func TestNewProducer_STSStaticKeys(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusInternalServerError, "Helix credential service error (internal_error): boom"},
		{http.StatusUnauthorized, "invalid AWS credentials"},
		{http.StatusForbidden, "invalid AWS credentials"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")
			service, _ := credentialServiceAnswering(t, tc.status, `{"error":{"code":"internal_error","message":"boom"}}`)

			_, err := NewProducer(types.Config{
				APIEndpoint:        service.URL,
				AWSAccessKeyID:     "AKIDTESTPRODUCER",
				AWSSecretAccessKey: "fake-secret",
				CredentialMode:     types.CredentialModeSTS,
				CustomerID:         "cust-1",
			})

			if err == nil || err.Error() != tc.want {
				t.Fatalf("NewProducer error = %v, want exactly %q", err, tc.want)
			}
		})
	}
}

// wantAPIKeyCredentialFailure is what an API-key caller sees when getting
// working credentials failed for a reason with no more specific message.
const wantAPIKeyCredentialFailure = "Helix credential service error: could not get working credentials for this API key"

// credentialServiceTruncating answers every mint request with status and a
// body cut short: it promises 4096 bytes, writes only body, then hangs up, so
// the client fails while reading the body.
func credentialServiceTruncating(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestNewProducer_APIKeyTruncatedServiceErrorNamesService: an API-key caller
// whose credential service answers 500/502 with a body that breaks off
// mid-read sees the service's status, never "invalid AWS credentials" — and a
// key the cut-off body echoes never reaches the message.
func TestNewProducer_APIKeyTruncatedServiceErrorNamesService(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"500", http.StatusInternalServerError, `{"error":{"code":"internal_error"`, "Helix credential service error: HTTP 500"},
		{"502 echoing the key", http.StatusBadGateway, `{"error":{"message":"HLX-API-Key ` + testAPIKeyForErrors, "Helix credential service error: HTTP 502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")
			service, calls := credentialServiceTruncating(t, tc.status, tc.body)

			_, err := NewProducer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cust-1"})

			if err == nil || err.Error() != tc.want {
				t.Fatalf("NewProducer error = %v, want exactly %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "AWS") || strings.Contains(err.Error(), "credentialErrorTestKey") {
				t.Errorf("Error() = %q mentions AWS or leaks the API key", err.Error())
			}
			if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
				t.Error("the service answered, so it is not unreachable")
			}
			var mintErr *stscreds.MintError
			if !errors.As(err, &mintErr) {
				t.Error("errors.As(*MintError) = false; the service's error must stay reachable for debugging")
			}
			if got := calls.Load(); got != 3 {
				t.Errorf("credential service called %d time(s), want 3 (a %d is retried)", got, tc.status)
			}
		})
	}
}

// TestNewProducer_APIKeyOtherCredentialFailureNamesService: an API-key caller
// whose credentials fail for a reason with no specific message — a refused
// redirect, or a 2xx that is not valid — sees a credential service error,
// never "invalid AWS credentials".
func TestNewProducer_APIKeyOtherCredentialFailureNamesService(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"refused redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		}},
		{"malformed 2xx", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`not json`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")
			service := httptest.NewServer(tc.handler)
			t.Cleanup(service.Close)

			_, err := NewProducer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cust-1"})

			if err == nil || err.Error() != wantAPIKeyCredentialFailure {
				t.Fatalf("NewProducer error = %v, want exactly %q", err, wantAPIKeyCredentialFailure)
			}
			if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
				t.Error("the service answered, so it is not unreachable")
			}
			if errors.Unwrap(err) == nil {
				t.Error("errors.Unwrap(err) = nil; the underlying failure must stay reachable for debugging")
			}
		})
	}
}

// TestNewProducer_STSStaticKeysTruncatedServiceError: for a static-key
// STS-mode caller a truncated 500 is a credential service error, exactly like
// an intact 500; a truncated 401/403 is still the service rejecting their AWS
// keys, and a malformed 2xx still falls through — both keep
// "invalid AWS credentials".
func TestNewProducer_STSStaticKeysTruncatedServiceError(t *testing.T) {
	cases := []struct {
		name    string
		service func(t *testing.T) *httptest.Server
		want    string
	}{
		{"truncated 500", func(t *testing.T) *httptest.Server {
			s, _ := credentialServiceTruncating(t, http.StatusInternalServerError, `{"error":`)
			return s
		}, "Helix credential service error: HTTP 500"},
		{"truncated 401", func(t *testing.T) *httptest.Server {
			s, _ := credentialServiceTruncating(t, http.StatusUnauthorized, `{"error":`)
			return s
		}, "invalid AWS credentials"},
		{"truncated 403", func(t *testing.T) *httptest.Server {
			s, _ := credentialServiceTruncating(t, http.StatusForbidden, `{"error":`)
			return s
		}, "invalid AWS credentials"},
		{"malformed 2xx", func(t *testing.T) *httptest.Server {
			s, _ := credentialServiceAnswering(t, http.StatusOK, `not json`)
			return s
		}, "invalid AWS credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")

			_, err := NewProducer(types.Config{
				APIEndpoint:        tc.service(t).URL,
				AWSAccessKeyID:     "AKIDTESTPRODUCER",
				AWSSecretAccessKey: "fake-secret",
				CredentialMode:     types.CredentialModeSTS,
				CustomerID:         "cust-1",
			})

			if err == nil || err.Error() != tc.want {
				t.Fatalf("NewProducer error = %v, want exactly %q", err, tc.want)
			}
		})
	}
}
