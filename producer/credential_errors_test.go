package producer

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/types"
)

// testAPIKeyForErrors is shaped like a real key ("hlx_" + 43 characters) but
// is not one.
const testAPIKeyForErrors = "hlx_credentialErrorTestKeyNotARealCredential1"

// TestNewProducer_APIKeyCredentialServiceUnreachable: an API-key caller (no
// AWS keys configured) whose credential service cannot be reached is told
// exactly that, never that their AWS credentials are invalid.
func TestNewProducer_APIKeyCredentialServiceUnreachable(t *testing.T) {
	isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")

	p, err := NewProducer(types.Config{
		APIEndpoint: "http://127.0.0.1:1", // closed port: no response ever arrives
		APIKey:      testAPIKeyForErrors,
		CustomerID:  "cust-1",
	})

	want := "could not reach the Helix credential service: credential mint request failed before a response"
	if err == nil || p != nil {
		t.Fatalf("NewProducer = %v, %v; want nil and an error", p, err)
	}
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "AWS") {
		t.Errorf("Error() = %q mentions AWS; an API-key caller configured no AWS keys", err.Error())
	}
	if !errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
		t.Error("errors.Is(err, ErrCredentialServiceUnreachable) = false, want true")
	}
	// The raw *net.OpError (which carries the closed port's host:port in
	// its own Error() text) must no longer be reachable anywhere in the
	// chain, nor in a full %+v dump of it.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		t.Error("errors.As(*net.OpError) = true; the raw connection error (with host:port) must no longer be reachable")
	}
	if dump := fmt.Sprintf("%+v", err); strings.Contains(dump, "127.0.0.1") {
		t.Errorf("%%+v = %q, leaks the closed port's host", dump)
	}
}

// TestNewProducer_APIKeyRejectedSurfacesFriendlyMessage: when the credential
// service answers and rejects the key, its customer-facing message reaches
// the caller instead of a generic AWS-credentials error.
func TestNewProducer_APIKeyRejectedSurfacesFriendlyMessage(t *testing.T) {
	isolateAWSEnv(t, fakeIdentityServer(t).URL, "http://127.0.0.1:1")
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"invalid api key","request_id":"r-1"}}`))
	}))
	t.Cleanup(service.Close)

	_, err := NewProducer(types.Config{
		APIEndpoint: service.URL,
		APIKey:      testAPIKeyForErrors,
		CustomerID:  "cust-1",
	})

	want := "Helix API key was rejected. Create a new key in the Helix portal under API Keys."
	if err == nil || err.Error() != want {
		t.Fatalf("NewProducer error = %v, want %q", err, want)
	}
}
