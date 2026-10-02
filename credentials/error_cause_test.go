// Tests for D19: the credential broker's HTTP client call must give the
// customer a clean, authored message on failure — never the upstream
// transport error's own text (which can carry a presigned/internal URL,
// account detail, etc.) — while still attaching it as the cause so
// errors.Unwrap/errors.As can reach it.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
)

const arnAccountService = "arn:aws:iam::123456789012:user/test"

// arnTransport fails every request with a raw transport error carrying an
// ARN and account ID, mimicking what a real cloud SDK/HTTP failure can leak.
type arnTransport struct{}

func (arnTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: connection refused talking to a host serving %s", arnAccountService)
}

func TestMint_TransportFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	cfg := testBrokerConfig("https://broker.test")
	cfg.HTTPClient = &http.Client{Transport: arnTransport{}}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if strings.Contains(err.Error(), arnAccountService) || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the upstream transport error", err.Error())
	}
	if !strings.Contains(err.Error(), "credentials: mint request failed") {
		t.Fatalf("Error() = %q, want it to contain the clean authored message", err.Error())
	}
	// No response arrived, so the failure is marked as an unreachable
	// credential service for NewConsumer/NewProducer to report as such.
	if !errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
		t.Error("errors.Is(err, ErrCredentialServiceUnreachable) = false for a transport failure, want true")
	}
	// Unwrap through mintWithRetry's own "mint failed after N attempts" wrap
	// (safe: it only embeds the already-clean inner message) down to the raw
	// transport error, which must still carry the ARN for debugging.
	inner := errors.Unwrap(err)
	if inner == nil {
		t.Fatal("errors.Unwrap(err) = nil at the first layer")
	}
	cause := errors.Unwrap(inner)
	if cause == nil || !strings.Contains(cause.Error(), arnAccountService) {
		t.Fatalf("unwrapped cause = %v, want the original ARN-carrying transport error reachable", cause)
	}
}

// TestMint_NegativeControl proves the fabricated transport error really does
// carry the ARN, by reproducing the pre-fix behavior (fmt.Errorf with %w)
// against the SAME transport and confirming THAT leaks.
func TestMint_NegativeControl(t *testing.T) {
	_, rawErr := (&http.Client{Transport: arnTransport{}}).Do(mustRequest(t))
	if rawErr == nil {
		t.Fatal("expected the fake transport to fail")
	}

	legacy := fmt.Errorf("credentials: mint request failed: %w", rawErr)
	if !strings.Contains(legacy.Error(), arnAccountService) {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}

// truncatedBodyTransport returns a successful status line but a body reader
// that fails mid-read, exercising the io.ReadAll(resp.Body) wrap site.
type truncatedBodyTransport struct{}

func (truncatedBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &failingReadCloser{failMsg: "connection reset touching " + arnAccountService},
		Request:    req,
	}, nil
}

type failingReadCloser struct{ failMsg string }

func (f *failingReadCloser) Read([]byte) (int, error) { return 0, errors.New(f.failMsg) }
func (f *failingReadCloser) Close() error              { return nil }

func TestMint_ResponseBodyReadFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	cfg := testBrokerConfig("https://broker.test")
	cfg.HTTPClient = &http.Client{Transport: truncatedBodyTransport{}}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = p.Retrieve(context.Background())

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if strings.Contains(err.Error(), arnAccountService) {
		t.Fatalf("Error() = %q, leaks the upstream read error", err.Error())
	}
	if !strings.Contains(err.Error(), "credentials: failed to read mint response") {
		t.Fatalf("Error() = %q, want it to contain the clean authored message", err.Error())
	}
	// A response did arrive (only its body failed), so this is not an
	// unreachable credential service.
	if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
		t.Error("errors.Is(err, ErrCredentialServiceUnreachable) = true after a response arrived, want false")
	}
	inner := errors.Unwrap(err)
	if inner == nil {
		t.Fatal("errors.Unwrap(err) = nil at the first layer")
	}
	cause := errors.Unwrap(inner)
	if cause == nil || !strings.Contains(cause.Error(), arnAccountService) {
		t.Fatalf("unwrapped cause = %v, want the original read error reachable", cause)
	}
}

func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://broker.test"+MintPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
