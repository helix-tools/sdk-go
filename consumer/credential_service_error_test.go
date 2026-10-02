package consumer

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

// assertServiceError checks err names the credential service, carries want
// exactly, never mentions AWS keys, and keeps the raw *MintError reachable.
func assertServiceError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("NewConsumer error = nil, want a credential service error")
	}
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
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
}

// TestNewConsumer_APIKeyServiceErrorSurfacesServiceMessage: an API-key
// caller whose credential service answers 500 sees the service's error, not
// "invalid AWS credentials" (they configured no AWS keys).
func TestNewConsumer_APIKeyServiceErrorSurfacesServiceMessage(t *testing.T) {
	identity, identityCalls := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, calls := credentialServiceAnswering(t, http.StatusInternalServerError,
		`{"error":{"code":"internal_error","message":"something went wrong","request_id":"r-1"}}`)

	_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

	assertServiceError(t, err, "Helix credential service error (internal_error): something went wrong")
	if got := calls.Load(); got != 3 {
		t.Errorf("credential service called %d time(s), want 3 (a 500 is retried)", got)
	}
	if got := identityCalls.Load(); got != 0 {
		t.Errorf("identity service called %d time(s), want 0", got)
	}
}

// TestNewConsumer_APIKeyServiceUnavailableAfterRetries: a 503 with an untyped
// body that persists through every retry still names the credential service.
func TestNewConsumer_APIKeyServiceUnavailableAfterRetries(t *testing.T) {
	identity, _ := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, calls := credentialServiceAnswering(t, http.StatusServiceUnavailable, `upstream unavailable`)

	_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

	assertServiceError(t, err, "Helix credential service error: upstream unavailable")
	if got := calls.Load(); got != 3 {
		t.Errorf("credential service called %d time(s), want 3", got)
	}
}

// TestNewConsumer_APIKeyServiceErrorEmptyBody: an error with no body at all
// still names the credential service, with the HTTP status as the detail.
func TestNewConsumer_APIKeyServiceErrorEmptyBody(t *testing.T) {
	identity, _ := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, _ := credentialServiceAnswering(t, http.StatusBadGateway, ``)

	_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

	assertServiceError(t, err, "Helix credential service error: HTTP 502")
}

// TestNewConsumer_APIKeyUnmapped4xxSurfacesServiceMessage: a 403 with no
// mapped customer-facing message (e.g. insufficient_scope) is the service's
// decision about the API key, not an AWS-key problem.
func TestNewConsumer_APIKeyUnmapped4xxSurfacesServiceMessage(t *testing.T) {
	identity, _ := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, calls := credentialServiceAnswering(t, http.StatusForbidden,
		`{"error":{"code":"insufficient_scope","message":"key lacks consumer scope","request_id":"r-2"}}`)

	_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

	assertServiceError(t, err, "Helix credential service error (insufficient_scope): key lacks consumer scope")
	if got := calls.Load(); got != 1 {
		t.Errorf("credential service called %d time(s), want 1 (a 403 is not retried)", got)
	}
}

// TestNewConsumer_APIKeyServiceErrorEchoingKeyIsScrubbed: a service error
// body that repeats the caller's API key (in the message and the code) never
// shows the key in the new message.
func TestNewConsumer_APIKeyServiceErrorEchoingKeyIsScrubbed(t *testing.T) {
	identity, _ := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, _ := credentialServiceAnswering(t, http.StatusInternalServerError,
		`{"error":{"code":"bad_`+testAPIKeyForErrors+`","message":"failed for key `+testAPIKeyForErrors+` (HLX-API-Key `+testAPIKeyForErrors+`)"}}`)

	_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

	if err == nil {
		t.Fatal("NewConsumer error = nil, want a credential service error")
	}
	if !strings.HasPrefix(err.Error(), "Helix credential service error (") {
		t.Errorf("Error() = %q, want the credential service error", err.Error())
	}
	if strings.Contains(err.Error(), testAPIKeyForErrors) || strings.Contains(err.Error(), "credentialErrorTestKey") {
		t.Errorf("Error() = %q leaks the API key", err.Error())
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("Error() = %q, want the echoed key replaced with a redaction marker", err.Error())
	}
}

// TestNewConsumer_STSStaticKeysServiceErrorSurfacesServiceMessage: a
// static-key STS-mode caller whose credential service answers 500 also sees
// the service's error — its AWS keys were never judged.
func TestNewConsumer_STSStaticKeysServiceErrorSurfacesServiceMessage(t *testing.T) {
	identity, _ := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, _ := credentialServiceAnswering(t, http.StatusInternalServerError,
		`{"error":{"code":"internal_error","message":"something went wrong"}}`)

	_, err := NewConsumer(types.Config{
		APIEndpoint:        service.URL,
		AWSAccessKeyID:     "AKIDTESTCONSUMER",
		AWSSecretAccessKey: "fake-secret",
		CredentialMode:     types.CredentialModeSTS,
		CustomerID:         "cons-1",
	})

	assertServiceError(t, err, "Helix credential service error (internal_error): something went wrong")
}

// TestNewConsumer_STSStaticKeysRejectedKeepsMessage: a static-key STS-mode
// caller whose AWS keys the credential service rejects keeps the existing
// "invalid AWS credentials" message — for them that is the right advice.
func TestNewConsumer_STSStaticKeysRejectedKeepsMessage(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			identity, _ := countingIdentityServer(t, http.StatusOK)
			isolateConsumerAWSEnv(t, identity.URL)
			service, _ := credentialServiceAnswering(t, status,
				`{"error":{"code":"unauthorized","message":"signature does not match"}}`)

			_, err := NewConsumer(types.Config{
				APIEndpoint:        service.URL,
				AWSAccessKeyID:     "AKIDTESTCONSUMER",
				AWSSecretAccessKey: "fake-secret",
				CredentialMode:     types.CredentialModeSTS,
				CustomerID:         "cons-1",
			})

			if err == nil || err.Error() != "invalid AWS credentials" {
				t.Fatalf("NewConsumer error = %v, want exactly %q", err, "invalid AWS credentials")
			}
		})
	}
}

// wantKeyCallerServiceFailure is what an API-key caller sees when getting
// working credentials failed for a reason with no more specific message.
const wantKeyCallerServiceFailure = "Helix credential service error: could not get working credentials for this API key"

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

// TestNewConsumer_APIKeyTruncatedServiceErrorNamesService: an API-key caller
// whose credential service answers 500/502 with a body that breaks off
// mid-read sees the service's status, never "invalid AWS credentials" — and a
// key the cut-off body echoes never reaches the message.
func TestNewConsumer_APIKeyTruncatedServiceErrorNamesService(t *testing.T) {
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
			identity, identityCalls := countingIdentityServer(t, http.StatusOK)
			isolateConsumerAWSEnv(t, identity.URL)
			service, calls := credentialServiceTruncating(t, tc.status, tc.body)

			_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

			assertServiceError(t, err, tc.want)
			if err != nil && strings.Contains(err.Error(), "credentialErrorTestKey") {
				t.Errorf("Error() = %q leaks the API key", err.Error())
			}
			if got := calls.Load(); got != 3 {
				t.Errorf("credential service called %d time(s), want 3 (a %d is retried)", got, tc.status)
			}
			if got := identityCalls.Load(); got != 0 {
				t.Errorf("identity service called %d time(s), want 0", got)
			}
		})
	}
}

// TestNewConsumer_APIKeyOtherCredentialFailureNamesService: an API-key caller
// whose credentials fail for a reason with no specific message — the service
// answered with a redirect (refused), or with a 2xx that is not valid — sees
// a credential service error, never "invalid AWS credentials".
func TestNewConsumer_APIKeyOtherCredentialFailureNamesService(t *testing.T) {
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
		{"2xx missing fields", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity, identityCalls := countingIdentityServer(t, http.StatusOK)
			isolateConsumerAWSEnv(t, identity.URL)
			service := httptest.NewServer(tc.handler)
			t.Cleanup(service.Close)

			_, err := NewConsumer(types.Config{APIEndpoint: service.URL, APIKey: testAPIKeyForErrors, CustomerID: "cons-1"})

			if err == nil || err.Error() != wantKeyCallerServiceFailure {
				t.Fatalf("NewConsumer error = %v, want exactly %q", err, wantKeyCallerServiceFailure)
			}
			if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
				t.Error("the service answered, so it is not unreachable")
			}
			if errors.Unwrap(err) == nil {
				t.Error("errors.Unwrap(err) = nil; the underlying failure must stay reachable for debugging")
			}
			if got := identityCalls.Load(); got != 0 {
				t.Errorf("identity service called %d time(s), want 0", got)
			}
		})
	}
}

// TestNewConsumer_STSStaticKeysTruncatedServiceError: for a static-key
// STS-mode caller a truncated 500 is a credential service error, exactly like
// an intact 500; a truncated 401/403 is still the service rejecting their AWS
// keys, so it keeps "invalid AWS credentials".
func TestNewConsumer_STSStaticKeysTruncatedServiceError(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusInternalServerError, "Helix credential service error: HTTP 500"},
		{http.StatusUnauthorized, "invalid AWS credentials"},
		{http.StatusForbidden, "invalid AWS credentials"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			identity, _ := countingIdentityServer(t, http.StatusOK)
			isolateConsumerAWSEnv(t, identity.URL)
			service, _ := credentialServiceTruncating(t, tc.status, `{"error":{"code":"unauth`)

			_, err := NewConsumer(types.Config{
				APIEndpoint:        service.URL,
				AWSAccessKeyID:     "AKIDTESTCONSUMER",
				AWSSecretAccessKey: "fake-secret",
				CredentialMode:     types.CredentialModeSTS,
				CustomerID:         "cons-1",
			})

			if err == nil || err.Error() != tc.want {
				t.Fatalf("NewConsumer error = %v, want exactly %q", err, tc.want)
			}
		})
	}
}

// TestNewConsumer_STSStaticKeysMalformed2xxKeepsMessage: the generic
// fall-through is unchanged for a static-key caller.
func TestNewConsumer_STSStaticKeysMalformed2xxKeepsMessage(t *testing.T) {
	identity, _ := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service, _ := credentialServiceAnswering(t, http.StatusOK, `not json`)

	_, err := NewConsumer(types.Config{
		APIEndpoint:        service.URL,
		AWSAccessKeyID:     "AKIDTESTCONSUMER",
		AWSSecretAccessKey: "fake-secret",
		CredentialMode:     types.CredentialModeSTS,
		CustomerID:         "cons-1",
	})

	if err == nil || err.Error() != "invalid AWS credentials" {
		t.Fatalf("NewConsumer error = %v, want exactly %q", err, "invalid AWS credentials")
	}
}
