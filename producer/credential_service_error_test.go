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
