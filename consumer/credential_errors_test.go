package consumer

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/types"
)

// testAPIKeyForErrors is shaped like a real key ("hlx_" + 43 characters) but
// is not one.
const testAPIKeyForErrors = "hlx_credentialErrorTestKeyNotARealCredential1"

// countingIdentityServer stands in for the identity service and counts calls;
// status is the HTTP status it answers with.
func countingIdentityServer(t *testing.T, status int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/xml")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>InvalidClientTokenId</Code><Message>bad token</Message></Error><RequestId>r</RequestId></ErrorResponse>`))
			return
		}
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
			`<Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDTEST</UserId><Account>123456789012</Account>` +
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r-1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// isolateConsumerAWSEnv keeps the machine's AWS configuration out of the test
// and points the identity service at identityURL.
func isolateConsumerAWSEnv(t *testing.T, identityURL string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "")
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("AWS_ENDPOINT_URL_STS", identityURL)
	t.Setenv("HELIX_API_ENDPOINT", "")
}

const wantUnreachableMsg = "could not reach the Helix credential service: credential mint request failed before a response"

// TestNewConsumer_APIKeyCredentialServiceUnreachable: an API-key caller (no
// AWS keys configured) whose credential service cannot be reached is told
// exactly that, never that their AWS credentials are invalid.
func TestNewConsumer_APIKeyCredentialServiceUnreachable(t *testing.T) {
	identity, identityCalls := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)

	c, err := NewConsumer(types.Config{
		APIEndpoint: "http://127.0.0.1:1", // closed port: no response ever arrives
		APIKey:      testAPIKeyForErrors,
		CustomerID:  "cons-1",
	})

	if err == nil || c != nil {
		t.Fatalf("NewConsumer = %v, %v; want nil and an error", c, err)
	}
	if err.Error() != wantUnreachableMsg {
		t.Errorf("Error() = %q, want %q", err.Error(), wantUnreachableMsg)
	}
	if strings.Contains(err.Error(), "AWS") {
		t.Errorf("Error() = %q mentions AWS; an API-key caller configured no AWS keys", err.Error())
	}
	if !errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
		t.Error("errors.Is(err, ErrCredentialServiceUnreachable) = false, want true")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("errors.As(*net.OpError) = false; the raw connection error must stay reachable for debugging")
	}
	if got := identityCalls.Load(); got != 0 {
		t.Errorf("identity service called %d time(s), want 0 (no session credentials were obtained)", got)
	}
}

// TestNewConsumer_APIKeyRejectedSurfacesFriendlyMessage: when the credential
// service answers and rejects the key, its customer-facing message reaches
// the caller instead of a generic AWS-credentials error.
func TestNewConsumer_APIKeyRejectedSurfacesFriendlyMessage(t *testing.T) {
	identity, identityCalls := countingIdentityServer(t, http.StatusOK)
	isolateConsumerAWSEnv(t, identity.URL)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"invalid api key","request_id":"r-1"}}`))
	}))
	t.Cleanup(service.Close)

	_, err := NewConsumer(types.Config{
		APIEndpoint: service.URL,
		APIKey:      testAPIKeyForErrors,
		CustomerID:  "cons-1",
	})

	want := "Helix API key was rejected. Create a new key in the Helix portal under API Keys."
	if err == nil || err.Error() != want {
		t.Fatalf("NewConsumer error = %v, want %q", err, want)
	}
	if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
		t.Error("a rejected key is not an unreachable service")
	}
	if got := identityCalls.Load(); got != 0 {
		t.Errorf("identity service called %d time(s), want 0", got)
	}
}

// TestNewConsumer_StaticKeysRejectedKeepsMessage: static AWS keys that the
// identity service rejects keep the existing message, unchanged.
func TestNewConsumer_StaticKeysRejectedKeepsMessage(t *testing.T) {
	identity, identityCalls := countingIdentityServer(t, http.StatusForbidden)
	isolateConsumerAWSEnv(t, identity.URL)

	_, err := NewConsumer(types.Config{
		AWSAccessKeyID:     "AKIDTESTCONSUMER",
		AWSSecretAccessKey: "fake-secret",
		CustomerID:         "cons-1",
	})

	if err == nil || err.Error() != "invalid AWS credentials" {
		t.Fatalf("NewConsumer error = %v, want exactly %q", err, "invalid AWS credentials")
	}
	if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
		t.Error("a rejected static key is not an unreachable credential service")
	}
	if got := identityCalls.Load(); got != 1 {
		t.Errorf("identity service called %d time(s), want 1", got)
	}
}
