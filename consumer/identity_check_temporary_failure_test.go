// Tests for the legacy static-credentials identity check: a failure that
// means the identity check itself could not complete — no HTTP response at
// all, or a status that means "try again" (408, 429, 500-599) — must never
// be reported as "invalid AWS credentials". A provider outage is not a
// credential rejection, and a customer told their credentials are invalid
// during one has no way to tell the two apart without this distinction.
// Unlike every other wrapped error in this package, the temporary-failure
// error carries NO raw upstream detail reachable through errors.Unwrap or
// fmt's "%+v" either — see assertNoUpstreamLeak.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/helix-tools/sdk-go/v2/internal/sdkerr"
	"github.com/helix-tools/sdk-go/v2/types"
)

// fakeSTSStatus answers every GetCallerIdentity call with status, as a
// well-formed AWS STS error body (so the AWS SDK classifies it with a real
// HTTP status instead of treating it as an unparseable response). The body
// carries arnAccountService so assertCauseReachable can confirm the raw
// upstream error stays reachable via errors.Unwrap.
func fakeSTSStatus(t *testing.T, status int) *sts.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <Error>
    <Type>Receiver</Type>
    <Code>ServiceUnavailable</Code>
    <Message>` + arnAccountService + ` please try again</Message>
  </Error>
  <RequestId>test-request-id</RequestId>
</ErrorResponse>`))
	}))
	t.Cleanup(srv.Close)
	return sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
	})
}

// fakeSTSUnreachable points the STS client at a closed port, so the identity
// check gets no HTTP response at all.
func fakeSTSUnreachable(t *testing.T) *sts.Client {
	t.Helper()
	return sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String("http://127.0.0.1:1"),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
	})
}

// fakeSTSUnresolvableHost points the STS client at a hostname under the
// reserved (RFC 2606) .invalid TLD, so the identity check fails at DNS
// resolution — before any connection, let alone any HTTP response, is
// attempted. WithTimeout bounds the lookup so a sandboxed CI resolver that
// behaves differently than expected cannot hang the test.
func fakeSTSUnresolvableHost(t *testing.T) *sts.Client {
	t.Helper()
	return sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String("http://this-host-does-not-exist.invalid"),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
		HTTPClient:       awshttp.NewBuildableClient().WithTimeout(5 * time.Second),
	})
}

// fakeSTSTLSHandshakeFailure points the STS client at an HTTPS endpoint
// backed by a plain TCP listener that never speaks TLS, so the identity
// check fails during the TLS handshake — a connection was made, but still no
// HTTP response of any kind was ever received.
func fakeSTSTLSHandshakeFailure(t *testing.T) *sts.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Hold the connection open briefly without ever speaking
				// TLS, so the client's handshake fails instead of racing a
				// bare connection reset.
				time.Sleep(200 * time.Millisecond)
			}(conn)
		}
	}()
	return sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String("https://" + ln.Addr().String()),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
	})
}

// fakeSTSClientTimeout points the STS client at a listener that accepts the
// TCP connection but never answers, paired with an HTTP client Timeout far
// shorter than that — a client-side timeout that fires on the http.Client
// itself (net.Error.Timeout()), not via the caller's context, so the
// no-response classification must not depend on finding
// context.DeadlineExceeded in the chain.
func fakeSTSClientTimeout(t *testing.T) *sts.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and hold the connection open; never write a response.
			_ = conn
		}
	}()
	return sts.New(sts.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String("http://" + ln.Addr().String()),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
		HTTPClient:       awshttp.NewBuildableClient().WithTimeout(300 * time.Millisecond),
	})
}

// walkChain visits err and then every error reachable by repeatedly
// unwrapping it, following both the single-error Unwrap() error form and
// the multi-error Unwrap() []error form (the one errors.Join and
// sdkerr.WrapSentinel use).
func walkChain(err error, visit func(error)) {
	for err != nil {
		visit(err)
		switch x := err.(type) {
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		case interface{ Unwrap() []error }:
			for _, sub := range x.Unwrap() {
				walkChain(sub, visit)
			}
			return
		default:
			return
		}
	}
}

// assertNoUpstreamLeak is the inverse of assertCauseReachable: it confirms
// that NEITHER err.Error(), NOR anything reachable by walking the FULL
// errors.Unwrap chain, NOR fmt.Sprintf("%+v", err) contains any raw upstream
// marker a cloud SDK error carries — an ARN, an account id, the
// operation/service name, a request id, or a URL. The identity-check
// temporary-failure error is the one error in this package that must stay
// this clean all the way down, unlike every other wrapped error (which
// deliberately keeps its cause reachable for debugging).
func assertNoUpstreamLeak(t *testing.T, err error) {
	t.Helper()
	forbidden := []string{arnAccountService, "123456789012", "STS", "GetCallerIdentity", "RequestId", "http://", "https://"}
	check := func(label, s string) {
		for _, f := range forbidden {
			if strings.Contains(s, f) {
				t.Errorf("%s = %q, leaks upstream marker %q", label, s, f)
			}
		}
	}
	walkChain(err, func(e error) { check("chain", e.Error()) })
	check("%+v", fmt.Sprintf("%+v", err))
}

// TestValidateCredentials_IdentityCheckTemporaryFailure: a static-credentials
// identity check that AWS answered with a retryable status is reported as a
// temporary failure, for both bootstrap kinds — never "invalid AWS
// credentials", and never the raw AWS error text anywhere in the chain. The
// SDK's own *smithyhttp.ResponseError relationship callers may already check
// via errors.As stays discoverable, sanitized down to just the status code.
func TestValidateCredentials_IdentityCheckTemporaryFailure(t *testing.T) {
	statuses := []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadGateway}
	for _, status := range statuses {
		for _, apiKeyConfigured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/apiKey=%v", status, apiKeyConfigured), func(t *testing.T) {
				err := validateCredentials(context.Background(), fakeSTSStatus(t, status), apiKeyConfigured, true)

				assertClean(t, err, sdkerr.IdentityCheckTemporaryFailureMessage(status))
				assertNoUpstreamLeak(t, err)
				if strings.Contains(strings.ToLower(err.Error()), "invalid") {
					t.Errorf("Error() = %q, must not call a provider outage a credential rejection", err.Error())
				}
				if errors.Is(err, sdkerr.ErrCredentialServiceUnreachable) {
					t.Error("this is a static-mode AWS failure, not the Helix credential service")
				}
				var respErr *smithyhttp.ResponseError
				if !errors.As(err, &respErr) {
					t.Fatal("errors.As(err, &respErr) = false, want the response-error relationship preserved")
				}
				if got := respErr.HTTPStatusCode(); got != status {
					t.Errorf("respErr.HTTPStatusCode() = %d, want %d", got, status)
				}
			})
		}
	}
}

// TestValidateCredentials_IdentityCheckTemporaryFailure_SyntheticResponseIsSafeToInspect
// is the self-attack on the sanitized stand-in: a caller that does
// errors.As(err, &respErr) and then inspects the response the way an
// ordinary SDK response error is inspected — reading and closing Body,
// reading Header — must not panic, and must never see any real upstream
// data through either. Before this fix, Body and Header were the zero
// value of http.Response (a nil io.ReadCloser and a nil http.Header), so
// Body.Close() panicked with a nil pointer dereference.
func TestValidateCredentials_IdentityCheckTemporaryFailure_SyntheticResponseIsSafeToInspect(t *testing.T) {
	err := validateCredentials(context.Background(), fakeSTSStatus(t, http.StatusServiceUnavailable), false, true)

	var respErr *smithyhttp.ResponseError
	if !errors.As(err, &respErr) {
		t.Fatal("errors.As(err, &respErr) = false, want the response-error relationship preserved")
	}

	resp := respErr.HTTPResponse()
	if resp == nil || resp.Response == nil {
		t.Fatal("HTTPResponse() returned a nil *http.Response")
	}

	if resp.Header == nil {
		t.Error("Header = nil, want a non-nil empty header a caller can safely read")
	}
	if got := resp.Header.Get("Content-Type"); got != "" {
		t.Errorf(`Header.Get("Content-Type") = %q, want "" — the real upstream header must never surface`, got)
	}
	if len(resp.Header) != 0 {
		t.Errorf("len(Header) = %d, want 0", len(resp.Header))
	}

	if resp.Body == nil {
		t.Fatal("Body = nil, want a non-nil empty reader a caller can safely read and close")
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("io.ReadAll(Body) error = %v, want nil", readErr)
	}
	if len(body) != 0 {
		t.Errorf("Body content = %q, want empty — the real upstream body must never surface", body)
	}
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Errorf("Body.Close() error = %v, want nil", closeErr)
	}

	if resp.Request == nil {
		t.Fatal("Request = nil, want a non-nil stand-in a caller can safely read (e.g. respErr.HTTPResponse().Request.URL)")
	}
	if resp.Request.URL == nil {
		t.Fatal("Request.URL = nil, want a non-nil stand-in URL")
	}
	if got := resp.Request.URL.String(); got != "https://identity.invalid/" {
		t.Errorf("Request.URL = %q, want the neutral stand-in URL, not any real upstream endpoint", got)
	}
	if resp.Request.Method == "" {
		t.Error("Request.Method = \"\", want a non-empty stand-in method")
	}

	assertNoUpstreamLeak(t, err)
}

// TestValidateCredentials_IdentityCheckUnreachable: a static-credentials
// identity check that got no response at all from AWS is also a temporary
// failure, for both bootstrap kinds.
func TestValidateCredentials_IdentityCheckUnreachable(t *testing.T) {
	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(context.Background(), fakeSTSUnreachable(t), apiKeyConfigured, true)

			want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
			if err == nil || err.Error() != want {
				t.Fatalf("Error() = %v, want exactly %q", err, want)
			}
			assertNoUpstreamLeak(t, err)
			if strings.Contains(strings.ToLower(err.Error()), "invalid") {
				t.Errorf("Error() = %q, must not call a provider outage a credential rejection", err.Error())
			}
		})
	}
}

// TestValidateCredentials_IdentityCheckDNSFailureTemporaryFailure is
// TestValidateCredentials_IdentityCheckUnreachable's sibling for a DNS
// resolution failure instead of a closed port — a different "no response at
// all" cause that must classify identically.
func TestValidateCredentials_IdentityCheckDNSFailureTemporaryFailure(t *testing.T) {
	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(context.Background(), fakeSTSUnresolvableHost(t), apiKeyConfigured, true)

			want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
			if err == nil || err.Error() != want {
				t.Fatalf("Error() = %v, want exactly %q", err, want)
			}
			assertNoUpstreamLeak(t, err)
			if strings.Contains(strings.ToLower(err.Error()), "invalid") {
				t.Errorf("Error() = %q, must not call a provider outage a credential rejection", err.Error())
			}
		})
	}
}

// TestValidateCredentials_IdentityCheckTLSHandshakeFailureTemporaryFailure is
// TestValidateCredentials_IdentityCheckUnreachable's sibling for a TLS
// handshake failure — a TCP connection was made, but it never produced an
// HTTP response.
func TestValidateCredentials_IdentityCheckTLSHandshakeFailureTemporaryFailure(t *testing.T) {
	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(context.Background(), fakeSTSTLSHandshakeFailure(t), apiKeyConfigured, true)

			want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
			if err == nil || err.Error() != want {
				t.Fatalf("Error() = %v, want exactly %q", err, want)
			}
			assertNoUpstreamLeak(t, err)
			if strings.Contains(strings.ToLower(err.Error()), "invalid") {
				t.Errorf("Error() = %q, must not call a provider outage a credential rejection", err.Error())
			}
		})
	}
}

// TestValidateCredentials_IdentityCheckClientTimeoutTemporaryFailure is
// TestValidateCredentials_IdentityCheckUnreachable's sibling for a timeout
// enforced by the HTTP client itself (http.Client.Timeout), with an
// uncanceled, non-expiring caller context — covering a client timeout that
// the caller never asked for via their own context. Go's net/http
// implements a client Timeout as an internal context deadline, so
// errors.Is(err, context.DeadlineExceeded) resolves true here too — this
// test is about the customer-facing classification staying temporary, not
// about which internal branch gets there.
func TestValidateCredentials_IdentityCheckClientTimeoutTemporaryFailure(t *testing.T) {
	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(context.Background(), fakeSTSClientTimeout(t), apiKeyConfigured, true)

			want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
			if err == nil || err.Error() != want {
				t.Fatalf("Error() = %v, want exactly %q", err, want)
			}
			assertNoUpstreamLeak(t, err)
			if strings.Contains(strings.ToLower(err.Error()), "invalid") {
				t.Errorf("Error() = %q, must not call a provider outage a credential rejection", err.Error())
			}
		})
	}
}

// TestValidateCredentials_IdentityCheckContextCanceledTemporaryFailure covers
// the no-HTTP-response case that never produces a *smithyhttp.ResponseError
// at all: a context already canceled before GetCallerIdentity can complete.
// AWS SDK Go v2 reports this as *smithy.OperationError wrapping a plain
// "context canceled" error, never a *smithyhttp.ResponseError — so
// classifying it as temporary must not be gated on finding one. A caller
// that loops on errors.Is(err, context.Canceled) to decide whether to retry
// must still see that relationship, even though the message text changed.
func TestValidateCredentials_IdentityCheckContextCanceledTemporaryFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(ctx, fakeSTSUnreachable(t), apiKeyConfigured, true)

			want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
			if err == nil || err.Error() != want {
				t.Fatalf("Error() = %v, want exactly %q", err, want)
			}
			assertNoUpstreamLeak(t, err)
			if !errors.Is(err, context.Canceled) {
				t.Error("errors.Is(err, context.Canceled) = false, want true: a caller retry-loop keys off this")
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Error("errors.Is(err, context.DeadlineExceeded) = true for a canceled (not timed-out) context")
			}
		})
	}
}

// TestValidateCredentials_IdentityCheckDeadlineExceededTemporaryFailure is
// TestValidateCredentials_IdentityCheckContextCanceledTemporaryFailure's
// sibling for a context that expired rather than was explicitly canceled:
// errors.Is(err, context.DeadlineExceeded) must still resolve.
func TestValidateCredentials_IdentityCheckDeadlineExceededTemporaryFailure(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(ctx, fakeSTSUnreachable(t), apiKeyConfigured, true)

			want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
			if err == nil || err.Error() != want {
				t.Fatalf("Error() = %v, want exactly %q", err, want)
			}
			assertNoUpstreamLeak(t, err)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Error("errors.Is(err, context.DeadlineExceeded) = false, want true: a caller retry-loop keys off this")
			}
			if errors.Is(err, context.Canceled) {
				t.Error("errors.Is(err, context.Canceled) = true for a context that expired, not one explicitly canceled")
			}
		})
	}
}

// TestValidateCredentials_IdentityCheckDefinitiveRejectionUnchanged is the
// self-attack boundary: a genuine rejection (401/403, or any other
// non-retryable status, including the out-of-range 600) must keep today's
// "invalid AWS credentials" message exactly as before this fix — a caller
// must not be able to turn a real credential rejection into "try again" by
// any status.
func TestValidateCredentials_IdentityCheckDefinitiveRejectionUnchanged(t *testing.T) {
	// 600 is the negative boundary for the 500-599 temporary range: one
	// past 599, so it must fall through to the unchanged rejection message,
	// not the temporary-failure one.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, 600} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			err := validateCredentials(context.Background(), fakeSTSStatus(t, status), false, true)

			if err == nil || err.Error() != "invalid AWS credentials" {
				t.Fatalf("Error() = %v, want exactly %q", err, "invalid AWS credentials")
			}
		})
	}
}

// TestValidateCredentials_BrokerModeContextCanceledUnchanged is the other
// half of the mode boundary: the SAME canceled-context failure that gets
// reclassified in static mode must produce EXACTLY origin/main's behavior in
// broker/sts mode (isStaticMode false) — the temporary-failure message must
// never appear, and the pre-existing errors.Is(err, context.Canceled)
// relationship (reachable via sdkerr.Wrap's unchanged cause chain, not the
// new classification) must still hold, same as before this entire fix.
func TestValidateCredentials_BrokerModeContextCanceledUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(ctx, fakeSTSUnreachable(t), apiKeyConfigured, false)

			wantMsg := "invalid AWS credentials"
			if apiKeyConfigured {
				wantMsg = sdkerr.KeyCallerServiceFailure
			}
			if err == nil || err.Error() != wantMsg {
				t.Fatalf("Error() = %v, want exactly %q (broker mode must be unaffected by this fix)", err, wantMsg)
			}
			if !errors.Is(err, context.Canceled) {
				t.Error("errors.Is(err, context.Canceled) = false, want true: unchanged from origin/main's sdkerr.Wrap cause chain")
			}
		})
	}
}

// TestValidateCredentials_BrokerModeDeadlineExceededUnchanged is
// TestValidateCredentials_BrokerModeContextCanceledUnchanged's sibling for a
// context that expired instead of being explicitly canceled.
func TestValidateCredentials_BrokerModeDeadlineExceededUnchanged(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(ctx, fakeSTSUnreachable(t), apiKeyConfigured, false)

			wantMsg := "invalid AWS credentials"
			if apiKeyConfigured {
				wantMsg = sdkerr.KeyCallerServiceFailure
			}
			if err == nil || err.Error() != wantMsg {
				t.Fatalf("Error() = %v, want exactly %q (broker mode must be unaffected by this fix)", err, wantMsg)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Error("errors.Is(err, context.DeadlineExceeded) = false, want true: unchanged from origin/main's sdkerr.Wrap cause chain")
			}
		})
	}
}

// TestValidateCredentials_BrokerModeRetryableStatusUnchanged is the
// response-error counterpart: a 503 from the identity check in broker/sts
// mode must keep origin/main's unchanged rejection message and its
// unchanged (unsanitized) errors.As relationship — the sanitized, temporary
// classification this fix adds is scoped to static mode only.
func TestValidateCredentials_BrokerModeRetryableStatusUnchanged(t *testing.T) {
	for _, apiKeyConfigured := range []bool{false, true} {
		t.Run(fmt.Sprintf("apiKey=%v", apiKeyConfigured), func(t *testing.T) {
			err := validateCredentials(context.Background(), fakeSTSStatus(t, http.StatusServiceUnavailable), apiKeyConfigured, false)

			wantMsg := "invalid AWS credentials"
			if apiKeyConfigured {
				wantMsg = sdkerr.KeyCallerServiceFailure
			}
			if err == nil || err.Error() != wantMsg {
				t.Fatalf("Error() = %v, want exactly %q (broker mode must be unaffected by this fix)", err, wantMsg)
			}
			var respErr *smithyhttp.ResponseError
			if !errors.As(err, &respErr) {
				t.Fatal("errors.As(err, &respErr) = false, want true: unchanged from origin/main's sdkerr.Wrap cause chain")
			}
			if got := respErr.HTTPStatusCode(); got != http.StatusServiceUnavailable {
				t.Errorf("respErr.HTTPStatusCode() = %d, want %d", got, http.StatusServiceUnavailable)
			}
		})
	}
}

// TestNewConsumer_StaticKeysIdentityCheckTemporaryFailure is the wiring test:
// full NewConsumer construction, not validateCredentials in isolation. A
// static-credentials caller whose identity check hits a 503 at construction
// sees the temporary-failure message, not "invalid AWS credentials".
func TestNewConsumer_StaticKeysIdentityCheckTemporaryFailure(t *testing.T) {
	identity, identityCalls := countingIdentityServer(t, http.StatusServiceUnavailable)
	isolateConsumerAWSEnv(t, identity.URL)

	_, err := NewConsumer(types.Config{
		AWSAccessKeyID:     "AKIDTESTCONSUMER",
		AWSSecretAccessKey: "fake-secret",
		CustomerID:         "cons-1",
	})

	want := sdkerr.IdentityCheckTemporaryFailureMessage(http.StatusServiceUnavailable)
	if err == nil || err.Error() != want {
		t.Fatalf("NewConsumer error = %v, want exactly %q", err, want)
	}
	assertNoUpstreamLeak(t, err)
	if got := identityCalls.Load(); got == 0 {
		t.Error("identity service never called")
	}
}

// TestNewConsumer_StaticKeysIdentityCheckUnreachable is the wiring test for
// no response at all: full NewConsumer construction against a closed port.
func TestNewConsumer_StaticKeysIdentityCheckUnreachable(t *testing.T) {
	isolateConsumerAWSEnv(t, "http://127.0.0.1:1")

	_, err := NewConsumer(types.Config{
		AWSAccessKeyID:     "AKIDTESTCONSUMER",
		AWSSecretAccessKey: "fake-secret",
		CustomerID:         "cons-1",
	})

	want := sdkerr.IdentityCheckTemporaryFailureMessage(0)
	if err == nil || err.Error() != want {
		t.Fatalf("NewConsumer error = %v, want exactly %q", err, want)
	}
	assertNoUpstreamLeak(t, err)
}
