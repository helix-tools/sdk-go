// Tests for D19: agent's HTTP client call must give the customer a clean,
// authored message on failure — never the upstream transport error's own
// text — while still attaching it as the cause so errors.Unwrap/errors.As
// can reach it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const arnAccountService = "arn:aws:iam::123456789012:user/test"

// arnTransport fails every request with a raw transport error carrying an
// ARN and account ID, mimicking what a real cloud SDK/HTTP failure can leak.
type arnTransport struct{}

func (arnTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: connection refused talking to a host serving %s", arnAccountService)
}

func TestClientDo_TransportFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	c := NewClient("https://agents.test", "jwt", WithHTTPClient(&http.Client{Transport: arnTransport{}}))

	_, err := c.Me(context.Background())

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if err.Error() != "agent: request failed" {
		t.Fatalf("Error() = %q, want exactly %q", err.Error(), "agent: request failed")
	}
	if strings.Contains(err.Error(), arnAccountService) || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the upstream transport error", err.Error())
	}
	cause := errors.Unwrap(err)
	if cause == nil || !strings.Contains(cause.Error(), arnAccountService) {
		t.Fatalf("unwrapped cause = %v, want the original ARN-carrying transport error reachable", cause)
	}
}

// truncatedBodyTransport returns a successful status line but a body reader
// that fails mid-read, exercising the io.ReadAll(resp.Body) wrap site.
type truncatedBodyTransport struct{}

func (truncatedBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       &failingReadCloser{failMsg: "connection reset touching " + arnAccountService},
		Request:    req,
	}, nil
}

type failingReadCloser struct{ failMsg string }

func (f *failingReadCloser) Read([]byte) (int, error) { return 0, errors.New(f.failMsg) }
func (f *failingReadCloser) Close() error              { return nil }

func TestClientDo_ResponseBodyReadFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	c := NewClient("https://agents.test", "jwt", WithHTTPClient(&http.Client{Transport: truncatedBodyTransport{}}))

	_, err := c.Me(context.Background())

	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if err.Error() != "agent: read response" {
		t.Fatalf("Error() = %q, want exactly %q", err.Error(), "agent: read response")
	}
	if strings.Contains(err.Error(), arnAccountService) {
		t.Fatalf("Error() = %q, leaks the upstream read error", err.Error())
	}
	cause := errors.Unwrap(err)
	if cause == nil || !strings.Contains(cause.Error(), arnAccountService) {
		t.Fatalf("unwrapped cause = %v, want the original read error reachable", cause)
	}
}

// TestClientDo_NegativeControl proves the fabricated transport error really
// does carry the ARN, by reproducing the pre-fix behavior (fmt.Errorf with
// %w) and confirming THAT leaks.
func TestClientDo_NegativeControl(t *testing.T) {
	_, rawErr := (&http.Client{Transport: arnTransport{}}).Do(mustRequest(t))
	if rawErr == nil {
		t.Fatal("expected the fake transport to fail")
	}

	legacy := fmt.Errorf("agent: request failed: %w", rawErr)
	if !strings.Contains(legacy.Error(), arnAccountService) {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}

func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://agents.test/v1/agents/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
