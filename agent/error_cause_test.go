// Tests for D19: agent's HTTP client call must give the customer a clean,
// authored message on failure — never the upstream transport error's own
// text — while still attaching it as the cause so errors.Unwrap/errors.As
// can reach it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const arnAccountService = "arn:aws:iam::123456789012:user/test"

// arnTransport fails every request with a raw transport error carrying an
// ARN and account ID, mimicking what a real cloud SDK/HTTP failure can
// leak. http.Client.Do wraps this directly in a *url.Error, so the ARN sits
// one level BELOW the *url.Error's own URL field — exactly the
// "unrecognized descendant" shape sdkerr.SanitizeCause must discard, which
// the generic connection-refused shape this test used to use could never
// catch (see TestClientDo_NegativeControl for the pre-fix leak this
// reproduces).
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
	leaks := []string{"agents.test", arnAccountService, "123456789012"}
	if containsAny(err.Error(), leaks) {
		t.Fatalf("Error() = %q, leaks the upstream transport error", err.Error())
	}
	if dump := fmt.Sprintf("%+v", err); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks the upstream transport error", dump)
	}
	depth := 0
	for e := err; e != nil; e = errors.Unwrap(e) {
		depth++
		if depth > 10 {
			t.Fatal("Unwrap chain did not terminate within 10 hops")
		}
		if containsAny(e.Error(), leaks) {
			t.Fatalf("chain node #%d (%T).Error() = %q, leaks the upstream transport error", depth, e, e.Error())
		}
	}
	if depth < 2 {
		t.Fatalf("errors.Unwrap(err) = nil, want a sanitized cause still reachable for debugging")
	}
}

func containsAny(s string, substrings []string) bool {
	for _, sub := range substrings {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestClientDo_RefusedRedirectKeepsRawURLError is the coverage the brief
// asks for outside the credential broker: when the caller's own httpClient
// refuses a redirect via CheckRedirect, http.Client.Do returns the
// (non-nil) Response alongside the (non-nil) error together — the service
// DID answer, so this is not a genuine no-response transport failure and
// must never be run through SanitizeCause. A caller's errors.As(err,
// *url.Error) must behave exactly as it did before SanitizeCause existed,
// with its Err field unchanged — mirroring
// credentials.TestProvider_Mint_RefusedRedirectKeepsRawURLError.
func TestClientDo_RefusedRedirectKeepsRawURLError(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/", http.StatusFound)
	}))
	defer server.Close()

	errRefused := errors.New("redirect refused by test policy")
	hc := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused },
	}
	c := NewClient(server.URL, "jwt", WithHTTPClient(hc))

	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("expected an error for a refused redirect")
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, *url.Error) = false, want true for a response-bearing refused redirect (err=%v)", err)
	}
	if !errors.Is(err, errRefused) {
		t.Error("errors.Is(err, errRefused) = false, want the refusal reachable as the cause")
	}
}

// singleCloseBody errors if Close is called more than once — unlike the
// real httptest-server body TestClientDo_RefusedRedirectKeepsRawURLError
// above exercises, which net/http's own connection-reuse machinery
// tolerates closing twice. net/http has already closed the response body
// itself by the time a refused-redirect error comes back (Client.Do's own
// doc: "even then the returned Response.Body is already closed"), so a
// wrap site closing it again on top of that is a double Close that this
// type makes caller-visible instead of silently swallowed.
type singleCloseBody struct{ closeCalls int }

func (b *singleCloseBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *singleCloseBody) Close() error {
	b.closeCalls++
	if b.closeCalls > 1 {
		return fmt.Errorf("Close called %d times, want at most 1", b.closeCalls)
	}
	return nil
}

type singleCloseRedirectingTransport struct{ body *singleCloseBody }

func (t singleCloseRedirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusFound,
		Status:     "302 Found",
		Header:     http.Header{"Location": []string{"https://agents.test/redirected"}},
		Body:       t.body,
		Request:    req,
	}, nil
}

// TestClientDo_RefusedRedirectClosesBodyExactlyOnce is the regression test
// for the double-Close fix: net/http already closes resp.Body itself
// before returning a refused-redirect error, so do's own explicit Close
// call on that branch — removed by this fix — must never run. A second
// Close call on singleCloseBody surfaces as an error from Close instead of
// a silently-swallowed double Close.
func TestClientDo_RefusedRedirectClosesBodyExactlyOnce(t *testing.T) {
	body := &singleCloseBody{}
	hc := &http.Client{
		Transport:     singleCloseRedirectingTransport{body: body},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused by test policy") },
	}
	c := NewClient("https://agents.test", "jwt", WithHTTPClient(hc))

	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("expected an error for a refused redirect")
	}
	if body.closeCalls != 1 {
		t.Fatalf("resp.Body.Close was called %d time(s), want exactly 1", body.closeCalls)
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
func (f *failingReadCloser) Close() error             { return nil }

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
