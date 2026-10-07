package sdkerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// errARNLaden is what a real cloud SDK error looks like: an operation
// name, a service name, and an ARN carrying an account ID — exactly the
// kind of text D19 says must never reach a customer-facing message.
var errARNLaden = errors.New("operation error KMS: Decrypt, https response error StatusCode: 400, " +
	"api error AccessDeniedException: User: arn:aws:iam::123456789012:user/test is not authorized " +
	"to perform: kms:Decrypt on resource: arn:aws:kms:us-east-1:123456789012:key/abcd-1234")

func TestWrap_MessageIsCleanCauseIsReachable(t *testing.T) {
	err := Wrap("decryption failed", errARNLaden)

	if err.Error() != "decryption failed" {
		t.Fatalf("Error() = %q, want exactly the clean authored message", err.Error())
	}
	if strings.Contains(err.Error(), "arn:aws:") || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the upstream cause's ARN/account id", err.Error())
	}

	if !errors.Is(err, errARNLaden) {
		t.Fatal("errors.Is(err, cause) = false, want true — the cause must stay reachable")
	}
	if got := errors.Unwrap(err); got != errARNLaden {
		t.Fatalf("errors.Unwrap(err) = %v, want the original cause", got)
	}
}

// TestWrap_NegativeControl is the negative control: build the SAME error the
// way the pre-fix code did (fmt.Errorf with %w) and confirm THAT leaks, so
// the assertions above are proven to fail for the right reason on the old
// shape rather than passing vacuously.
func TestWrap_NegativeControl(t *testing.T) {
	legacy := fmt.Errorf("decryption failed: %w", errARNLaden)

	if !strings.Contains(legacy.Error(), "arn:aws:") {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}

func TestWrap_AsReachesTypedCause(t *testing.T) {
	type typedCause struct{ error }
	cause := typedCause{errors.New("operation error S3: GetObject, arn:aws:s3:::acct-bucket/key")}

	err := Wrap("failed to download", cause)

	var target typedCause
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to reach the typed cause through Wrap")
	}
	if err.Error() != "failed to download" {
		t.Fatalf("Error() = %q, want the clean message unchanged", err.Error())
	}
}

func TestWrapSentinel_KeepsSentinelAndCauseReachable(t *testing.T) {
	sentinel := errors.New("object is not compressed; refusing to return it uncompressed")
	cause := errors.New("gzip: invalid header")

	err := WrapSentinel(sentinel, cause)

	if err.Error() != sentinel.Error() {
		t.Fatalf("Error() = %q, want the sentinel's own clean message", err.Error())
	}
	if strings.Contains(err.Error(), "gzip:") {
		t.Fatalf("Error() = %q, leaks the raw gzip cause text", err.Error())
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is(err, sentinel) = false, want true — existing sentinel behaviour must keep working")
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is(err, cause) = false, want true — the raw cause must stay reachable for debugging")
	}
}

// TestWrapSentinel_NestedTwoLevelsDeep is the bypass test: an upstream error
// wrapped a second time (as happens when a caller re-wraps an already-clean
// error with its own authored prefix via fmt.Errorf("...: %w", err)) must
// still not leak the original cause's text into the visible message, and the
// cause and sentinel must both still be reachable through the deeper chain.
func TestWrapSentinel_NestedTwoLevelsDeep(t *testing.T) {
	sentinel := errors.New("object is not compressed; refusing to return it uncompressed")
	cause := errors.New("gzip: invalid header: arn:aws:s3:::acct-123456789012-bucket/key")

	inner := WrapSentinel(sentinel, cause)
	outer := fmt.Errorf("decompression failed: %w", inner)

	if strings.Contains(outer.Error(), "arn:aws:") || strings.Contains(outer.Error(), "123456789012") {
		t.Fatalf("outer.Error() = %q, leaks the cause nested two levels deep", outer.Error())
	}
	if !errors.Is(outer, sentinel) {
		t.Fatal("errors.Is(outer, sentinel) = false through a second wrap layer")
	}
	if !errors.Is(outer, cause) {
		t.Fatal("errors.Is(outer, cause) = false through a second wrap layer")
	}
}

func TestWrapMarked_MarkerAndCauseReachableMessageUnchanged(t *testing.T) {
	marker := errors.New("category marker")
	err := WrapMarked("mint request failed", marker, errARNLaden)

	if err.Error() != "mint request failed" {
		t.Fatalf("Error() = %q, want exactly the authored message", err.Error())
	}
	if !errors.Is(err, marker) {
		t.Fatal("errors.Is(err, marker) = false, want true")
	}
	if !errors.Is(err, errARNLaden) {
		t.Fatal("errors.Is(err, cause) = false, want true")
	}
	if got := errors.Unwrap(err); got != errARNLaden {
		t.Fatalf("errors.Unwrap(err) = %v, want the original cause (single-error chain unchanged)", got)
	}
	// Still reachable through further %w wrapping, as in mintWithRetry.
	if outer := fmt.Errorf("mint failed after 3 attempts: %w", err); !errors.Is(outer, marker) {
		t.Fatal("errors.Is(outer, marker) = false through a %w wrap, want true")
	}
}

// TestWrap_NeverSanitizesCauseAutomatically pins Wrap's own contract now
// that SanitizeCause is no longer applied inside it: cause is stored
// completely unchanged, even one that looks network-transport-shaped. A
// call site that needs SanitizeCause's guarantee applies it explicitly
// first — sdkerr.Wrap(msg, sdkerr.SanitizeCause(cause)) — see
// TestWrap_NoResponseCauseNeverLeaksHostOrURL below for that explicit path.
func TestWrap_NeverSanitizesCauseAutomatically(t *testing.T) {
	raw := &url.Error{Op: "Post", URL: "https://broker.internal/mint", Err: errors.New("refused")}
	err := Wrap("mint request answered with a refused redirect", raw)

	if err.Error() != "mint request answered with a refused redirect" {
		t.Fatalf("Error() = %q, want the authored message unchanged", err.Error())
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatal("errors.As(err, *url.Error) = false, want true — Wrap must never sanitize on its own")
	}
	if urlErr != raw {
		t.Fatalf("urlErr = %v, want the exact original *url.Error value, unchanged", urlErr)
	}
}

// TestWrap_CarriesNoMarker is the negative control for WrapMarked: a plain
// Wrap must never report a marker, or every wrapped failure would be
// mistaken for an unreachable credential service.
func TestWrap_CarriesNoMarker(t *testing.T) {
	if errors.Is(Wrap("mint request failed", errARNLaden), ErrCredentialServiceUnreachable) {
		t.Fatal("errors.Is(Wrap(...), ErrCredentialServiceUnreachable) = true, want false")
	}
	if errors.Is(WrapMarked("x", nil, errARNLaden), ErrCredentialServiceUnreachable) {
		t.Fatal("a nil marker must match nothing")
	}
}

// ----------------------------------------------------------------------------
// SanitizeCause: the no-response/transport-failure leak the fix closes
// (presigned storage URL, host, query string never reachable through a full
// Unwrap walk), and every relationship it must preserve while doing that
// (context.Canceled, context.DeadlineExceeded, net.Error Timeout/Temporary,
// and a package-level marker wrapped underneath).
// ----------------------------------------------------------------------------

// presignedLikeURL builds a storage-presigned-URL-shaped string with a fake
// SigV4 credential/token/signature query, carrying a bucket name, so any
// node in the chain still showing it is caught by a plain substring check.
const presignedLikeURL = "http://127.0.0.1:1/acme-prod-bucket/datasets/leak-check.ndjson.gz?" +
	"X-Amz-Credential=AKIAFAKECREDENTIALVALUE%2F20261007%2Fus-east-1%2Fs3%2Faws4_request&" +
	"X-Amz-Security-Token=FAKE-SESSION-TOKEN-NOT-REAL&" +
	"X-Amz-Signature=deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// dialPresignedURLFailure performs one real PUT against presignedLikeURL,
// whose port (1) is reserved and refuses every connection, producing a
// genuine *url.Error wrapping a *net.OpError exactly as a real presigned
// storage upload/download would on an unreachable endpoint.
func dialPresignedURLFailure(t *testing.T) error {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, presignedLikeURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	_, rawErr := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if rawErr == nil {
		t.Fatal("expected 127.0.0.1:1 to refuse the connection")
	}
	return rawErr
}

// TestSanitizeCause_NegativeControl_RawDialFailureCarriesTheURL proves the
// raw cause produced by dialPresignedURLFailure really does carry the
// presigned URL (credential, token, signature, bucket) in its own Error()
// text, so TestWrap_NoResponseCauseNeverLeaksHostOrURL below is shown to
// fail for the right reason on the pre-fix shape, not pass vacuously.
func TestSanitizeCause_NegativeControl_RawDialFailureCarriesTheURL(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	for _, want := range []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1"} {
		if !strings.Contains(rawErr.Error(), want) {
			t.Fatalf("negative control did not reproduce the leak: rawErr = %q, want it to contain %q", rawErr.Error(), want)
		}
	}

	// And the exact pre-fix Wrap shape (cause stored directly, no
	// SanitizeCause) leaks it right through Unwrap(), which is the bug
	// this fix closes.
	type preFixWrapped struct {
		msg   string
		cause error
	}
	legacy := &preFixWrapped{msg: "failed to upload to presigned URL", cause: rawErr}
	if !strings.Contains(legacy.cause.Error(), "X-Amz-Signature") {
		t.Fatalf("pre-fix shape did not reproduce the leak: %q", legacy.cause.Error())
	}
}

// TestWrap_NoResponseCauseNeverLeaksHostOrURL is an end-to-end check: a
// real failed PUT to an unreachable presigned-looking URL (127.0.0.1:1, a
// reserved port that always refuses), wrapped the same way
// uploadToPresignedURL wraps it — SanitizeCause applied explicitly, exactly
// as the real call site does for a confirmed resp == nil failure. Error(),
// a full %+v dump, and every single node reachable by repeatedly calling
// errors.Unwrap must be clean — not just the outermost message.
func TestWrap_NoResponseCauseNeverLeaksHostOrURL(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	err := Wrap("failed to upload to presigned URL", SanitizeCause(rawErr))

	if err.Error() != "failed to upload to presigned URL" {
		t.Fatalf("Error() = %q, want the authored message unchanged", err.Error())
	}

	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1", presignedLikeURL}

	if dump := fmt.Sprintf("%+v", err); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks the presigned URL", dump)
	}

	depth := 0
	for e := error(err); e != nil; e = errors.Unwrap(e) {
		depth++
		if depth > 10 {
			t.Fatal("Unwrap chain did not terminate within 10 hops")
		}
		if text := e.Error(); containsAny(text, leaks) {
			t.Fatalf("chain node #%d (%T).Error() = %q, leaks the presigned URL", depth, e, text)
		}
	}
	if depth < 2 {
		t.Fatalf("Unwrap chain depth = %d, want at least 2 (the wrap plus the sanitized cause)", depth)
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

// TestSanitizeCause_PreservesContextCanceled: a caller canceling its own
// context mid-request must still see errors.Is(err, context.Canceled)
// through the sanitized stand-in, exactly as it would through the raw
// *url.Error today.
func TestSanitizeCause_PreservesContextCanceled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, rawErr := http.DefaultClient.Do(req)
	if rawErr == nil {
		t.Fatal("expected the canceled request to fail")
	}

	err := Wrap("request failed", SanitizeCause(rawErr))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false after sanitizing, want true (err=%v)", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("Error() = %q, leaks the server URL", err.Error())
	}
}

// TestSanitizeCause_PreservesContextDeadlineExceeded mirrors the canceled
// test above for a context deadline instead of an explicit cancel.
func TestSanitizeCause_PreservesContextDeadlineExceeded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	_, rawErr := http.DefaultClient.Do(req)
	if rawErr == nil {
		t.Fatal("expected the request to fail once its deadline passed")
	}

	err := Wrap("request failed", SanitizeCause(rawErr))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false after sanitizing, want true (err=%v)", err)
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("Error() = %q, leaks the server URL", err.Error())
	}
}

// TestSanitizeCause_PreservesNetErrorTimeout: an http.Client-level timeout
// (WithHTTPClient(&http.Client{Timeout: ...}), not a context deadline) must
// still answer Timeout() == true via the net.Error interface after
// sanitizing, exactly as the raw *url.Error would today — the self-attack
// case named in the brief.
func TestSanitizeCause_PreservesNetErrorTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	client := &http.Client{Timeout: 50 * time.Millisecond}
	_, rawErr := client.Get(srv.URL)
	if rawErr == nil {
		t.Fatal("expected the client timeout to fire")
	}
	if !strings.Contains(rawErr.Error(), srv.URL) {
		t.Fatalf("negative control did not reproduce the leak: %q", rawErr.Error())
	}

	err := Wrap("request failed", SanitizeCause(rawErr))

	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("errors.As(*net.Error) = false after sanitizing, want true (err=%v)", err)
	}
	if !netErr.Timeout() {
		t.Error("Timeout() = false, want true for a client timeout")
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("Error() = %q, leaks the server URL", err.Error())
	}
	if cause := errors.Unwrap(err); cause == nil || strings.Contains(cause.Error(), srv.URL) {
		t.Fatalf("unwrapped cause = %v, want it reachable but never containing the server URL", cause)
	}
}

// TestSanitizeCause_DiscardsUnrecognizedDescendantUnderneathHostBearingNode
// is the opposite of what an earlier version of this fix asserted: it kept
// whatever a host-bearing node directly wrapped reachable through the
// stand-in's Unwrap(), on the theory that it was always a safe, known shape
// (a context sentinel, a redirect-refusal marker). That "first unrecognized
// descendant" is only ASSUMED safe, though: nothing stops it from being an
// arbitrary error type whose own Error() text embeds the same sensitive
// data the *url.Error was hiding. A
// package-level marker is exactly such an arbitrary type from
// SanitizeCause's point of view (it has no way to tell a safe marker apart
// from an adversarial one), so it must no longer be reachable through the
// sanitized stand-in — only context.Canceled/context.DeadlineExceeded and
// the net.Error Timeout()/Temporary() answer are. (A real redirect-refusal
// marker never reaches SanitizeCause at all: credentials/broker.go detects
// resp != nil and calls plain Wrap instead, passing cause through
// unsanitized — see TestProvider_Mint_RefusedRedirectIsNotUnreachable in
// that package.)
func TestSanitizeCause_DiscardsUnrecognizedDescendantUnderneathHostBearingNode(t *testing.T) {
	marker := errors.New("package: refused for a policy reason")
	raw := &url.Error{Op: "Post", URL: "https://broker.internal/mint?token=SECRET-TOKEN-VALUE", Err: marker}

	sanitized := SanitizeCause(raw)

	if errors.Is(sanitized, marker) {
		t.Fatal("errors.Is(sanitized, marker) = true; an unrecognized descendant of a host-bearing node must no longer be reachable")
	}
	if errors.Unwrap(sanitized) != nil {
		t.Fatalf("errors.Unwrap(sanitized) = %v, want nil — the stand-in exposes nothing further", errors.Unwrap(sanitized))
	}
	if strings.Contains(sanitized.Error(), "SECRET-TOKEN-VALUE") || strings.Contains(sanitized.Error(), "broker.internal") {
		t.Fatalf("Error() = %q, leaks the URL", sanitized.Error())
	}
}

// operationError mimics the shape of an AWS SDK *smithy.OperationError:
// Unwrap() exposes the real cause, but Error() interpolates that cause's
// own text directly, so whatever it wraps must be sanitized BEFORE this
// type's Error() is ever called, not just at its own level.
type operationError struct {
	service, op string
	err         error
}

func (e *operationError) Error() string {
	return fmt.Sprintf("operation error %s: %s, %v", e.service, e.op, e.err)
}
func (e *operationError) Unwrap() error { return e.err }

// TestSanitizeCause_FindsHostBearingNodeInsideGenericWrapper covers the KMS/
// STS/SQS shape: the host-bearing *url.Error is not the outermost error —
// it is buried inside a generic AWS-SDK-style wrapper whose own Error() text
// would otherwise still leak it.
func TestSanitizeCause_FindsHostBearingNodeInsideGenericWrapper(t *testing.T) {
	inner := &url.Error{
		Op:  "Post",
		URL: "https://kms.us-east-1.amazonaws.com/?X-Amz-Signature=deadbeef",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
	}
	wrapped := &operationError{service: "KMS", op: "Decrypt", err: inner}

	sanitized := SanitizeCause(wrapped)

	if strings.Contains(sanitized.Error(), "X-Amz-Signature") || strings.Contains(sanitized.Error(), "kms.us-east-1") {
		t.Fatalf("Error() = %q, leaks the URL/host", sanitized.Error())
	}
	if cause := errors.Unwrap(sanitized); cause != nil && strings.Contains(cause.Error(), "X-Amz-Signature") {
		t.Fatalf("unwrapped cause = %v, leaks the URL", cause)
	}
}

// leakyUnknownWrapper is an error type SanitizeCause has never seen before —
// unlike operationError above, it isn't modelled on any specific cloud SDK
// shape. Its Error() embeds its cause's text, same as operationError's does.
type leakyUnknownWrapper struct{ err error }

func (e *leakyUnknownWrapper) Error() string { return "transport: " + e.err.Error() }
func (e *leakyUnknownWrapper) Unwrap() error { return e.err }

// leakyRoundTripper lets a test hand http.Client.Do an arbitrary error
// directly, exactly as the self-attack describes: "a custom RoundTripper
// returning fmt.Errorf(...) wrapped in another unknown error type."
type leakyRoundTripper struct{ err error }

func (rt *leakyRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, rt.err }

// TestWrap_RoundTripperErrorWithOwnSensitiveText is the self-attack named in
// the brief: the raw RoundTripper error ITSELF — not just the *url.Error
// http.Client wraps it in — carries a signed URL and an ARN-shaped string,
// one level further down than
// TestSanitizeCause_FindsHostBearingNodeInsideGenericWrapper reaches (that
// test's inner error is the plain, textless net.OpError a real dial failure
// produces; this one's is a custom type whose OWN Error() text is the leak).
// An earlier version of this fix kept that custom type reachable via the
// stand-in's Unwrap(), on the assumption that whatever a host-bearing node
// wrapped was always safe; this is the proof that assumption was false. The
// self-attack's second half — errors.Is(err, context.DeadlineExceeded) must
// still be true — is also asserted, so the fix for the first half is shown
// not to have cost the cancellation/deadline relationship it must preserve.
func TestWrap_RoundTripperErrorWithOwnSensitiveText(t *testing.T) {
	const (
		signedURL = "https://acme-prod-bucket.s3.amazonaws.com/datasets/leak.ndjson.gz?X-Amz-Signature=deadbeefdeadbeef"
		arn       = "arn:aws:iam::123456789012:role/internal-upload-role"
	)
	dialErr := fmt.Errorf("dial %s: %w", signedURL, context.DeadlineExceeded)
	unknown := &leakyUnknownWrapper{err: fmt.Errorf("%s: %w", arn, dialErr)}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	client := &http.Client{Transport: &leakyRoundTripper{err: unknown}}
	_, rawErr := client.Do(req) //nolint:bodyclose // Do returns a nil Response on a transport error
	if rawErr == nil {
		t.Fatal("expected the fake transport to fail")
	}
	if !strings.Contains(rawErr.Error(), "X-Amz-Signature") || !strings.Contains(rawErr.Error(), "123456789012") {
		t.Fatalf("negative control did not reproduce the leak: rawErr = %q", rawErr.Error())
	}

	out := Wrap("failed to upload to presigned URL", SanitizeCause(rawErr))

	leaks := []string{"X-Amz-Signature", "acme-prod-bucket", "123456789012", "arn:aws:iam", signedURL}
	if containsAny(out.Error(), leaks) {
		t.Fatalf("Error() = %q, leaks", out.Error())
	}
	if dump := fmt.Sprintf("%+v", out); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks", dump)
	}
	depth := 0
	for e := error(out); e != nil; e = errors.Unwrap(e) {
		depth++
		if depth > 10 {
			t.Fatal("Unwrap chain did not terminate within 10 hops")
		}
		if text := e.Error(); containsAny(text, leaks) {
			t.Fatalf("chain node #%d (%T).Error() = %q, leaks", depth, e, text)
		}
	}

	if !errors.Is(out, context.DeadlineExceeded) {
		t.Fatal("errors.Is(out, context.DeadlineExceeded) = false, want true — the self-attack's deadline relationship must still survive")
	}
}

// TestSanitizeCause_NonNetworkCausePassesThroughUnchanged is the negative
// control for the self-detection itself: a cause with no url.Error/
// net.OpError/net.DNSError/net.AddrError anywhere in its chain (a
// response-bearing API error, in real use) must come back byte-for-byte
// identical, never replaced.
func TestSanitizeCause_NonNetworkCausePassesThroughUnchanged(t *testing.T) {
	if got := SanitizeCause(errARNLaden); got != errARNLaden {
		t.Fatalf("SanitizeCause(non-network cause) = %v, want the exact same value unchanged", got)
	}
	if got := SanitizeCause(nil); got != nil {
		t.Fatalf("SanitizeCause(nil) = %v, want nil", got)
	}
}

// TestSanitizeCause_NegativeControl_JoinedErrorInvisibleToSingleUnwrap
// proves a joined error's branches are genuinely unreachable through
// errors.Unwrap's single-error form alone — the only form the chain walk
// followed before this fix — so
// TestSanitizeCause_FindsHostBearingNodeInsideJoinedError below is shown to
// fail for the right reason on that walk, not pass vacuously.
func TestSanitizeCause_NegativeControl_JoinedErrorInvisibleToSingleUnwrap(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	joined := errors.Join(rawErr, errors.New("some other upload-path failure"))

	if got := errors.Unwrap(joined); got != nil {
		t.Fatalf("errors.Unwrap(joined) = %v, want nil — a joined error exposes its members only through "+
			"Unwrap() []error, never Unwrap() error, which is exactly why a single-error-only walk misses it", got)
	}
}

// walkBothUnwrapForms visits every node reachable from err by following
// both Unwrap() error and Unwrap() []error, calling visit(node, depth) at
// each one. Shared by the joined-error tests below, which all need to
// assert something about every node a real recursive consumer (a
// %+v-style logger, or code deciding what to do with an error) would
// actually see — not just about err's own Error() text.
func walkBothUnwrapForms(t *testing.T, err error, visit func(e error, depth int)) {
	t.Helper()
	var walk func(e error, depth int)
	walk = func(e error, depth int) {
		if e == nil {
			return
		}
		if depth > 10 {
			t.Fatal("Unwrap walk (both forms) did not terminate within 10 hops")
		}
		visit(e, depth)
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, child := range multi.Unwrap() {
				walk(child, depth+1)
			}
			return
		}
		walk(errors.Unwrap(e), depth+1)
	}
	walk(err, 0)
}

// TestSanitizeCause_FindsHostBearingNodeInsideJoinedError: a joined error
// (errors.Join, Unwrap() []error) carrying a presigned-URL-bearing
// *url.Error in one of its branches must be scrubbed exactly as a
// linearly-wrapped one is — Error(), a %+v dump, and a recursive walk that
// follows BOTH Unwrap forms (not just errors.Unwrap) must never show the
// presigned URL — AND (the brief's requirement 2) the safe sibling
// branch joined alongside it must survive completely untouched: reachable
// via errors.Is, and its own message still visible to the same recursive
// walk, not merely "not leaking" by virtue of having been discarded too.
func TestSanitizeCause_FindsHostBearingNodeInsideJoinedError(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	sibling := errors.New("some other upload-path failure")
	joined := errors.Join(rawErr, sibling)

	err := Wrap("failed to upload to presigned URL", SanitizeCause(joined))

	if err.Error() != "failed to upload to presigned URL" {
		t.Fatalf("Error() = %q, want the authored message unchanged", err.Error())
	}
	if !errors.Is(err, sibling) {
		t.Fatal("errors.Is(err, sibling) = false, want true — a safe sibling of a sanitized branch must stay reachable")
	}

	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1", presignedLikeURL}

	if dump := fmt.Sprintf("%+v", err); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks the presigned URL", dump)
	}

	var leakedAt, siblingSeenAt string
	walkBothUnwrapForms(t, err, func(e error, depth int) {
		text := e.Error()
		if leakedAt == "" && containsAny(text, leaks) {
			leakedAt = fmt.Sprintf("depth %d (%T).Error() = %q", depth, e, text)
		}
		if siblingSeenAt == "" && text == sibling.Error() {
			siblingSeenAt = fmt.Sprintf("depth %d", depth)
		}
	})
	if leakedAt != "" {
		t.Fatalf("recursive walk (both Unwrap forms) found a leak: %s", leakedAt)
	}
	if siblingSeenAt == "" {
		t.Fatal("recursive walk (both Unwrap forms) never reached the safe sibling's own message")
	}
}

// TestSanitizeCause_JoinedError_PreservesCancellationSiblings is the
// cancellation/deadline half of the same requirement: errors.Is for
// context.Canceled and context.DeadlineExceeded must survive being joined
// alongside a genuine transport failure, exactly as it survives when they
// appear UNDERNEATH one (TestSanitizeCause_PreservesContextCanceled/
// DeadlineExceeded above) rather than beside it.
func TestSanitizeCause_JoinedError_PreservesCancellationSiblings(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			rawErr := dialPresignedURLFailure(t)
			joined := errors.Join(sentinel, rawErr)

			err := Wrap("failed to upload to presigned URL", SanitizeCause(joined))

			if !errors.Is(err, sentinel) {
				t.Fatalf("errors.Is(err, %v) = false, want true — a cancellation/deadline sibling must survive", sentinel)
			}
			leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1"}
			if containsAny(err.Error(), leaks) {
				t.Fatalf("Error() = %q, leaks the presigned URL", err.Error())
			}
			walkBothUnwrapForms(t, err, func(e error, depth int) {
				if text := e.Error(); containsAny(text, leaks) {
					t.Fatalf("depth %d (%T).Error() = %q, leaks the presigned URL", depth, e, text)
				}
			})
		})
	}
}

// TestSanitizeCause_JoinedError_PreservesSDKMarkerSibling: an SDK-exported
// sentinel (ErrCredentialServiceUnreachable, the one real marker this
// package exports into a cause chain — see WrapSentinel/producer.go's
// mintWithRetry) joined alongside a genuine transport failure must stay
// errors.Is-reachable exactly as a safe sibling does, even though it is
// itself just another error value from SanitizeCause's point of view.
func TestSanitizeCause_JoinedError_PreservesSDKMarkerSibling(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	joined := errors.Join(ErrCredentialServiceUnreachable, rawErr)

	err := Wrap("failed to upload to presigned URL", SanitizeCause(joined))

	if !errors.Is(err, ErrCredentialServiceUnreachable) {
		t.Fatal("errors.Is(err, ErrCredentialServiceUnreachable) = false, want true — the SDK marker sibling must survive")
	}
	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1"}
	if containsAny(err.Error(), leaks) {
		t.Fatalf("Error() = %q, leaks the presigned URL", err.Error())
	}
}

// ----------------------------------------------------------------------------
// Nested join beneath an ordinary wrapper: the three tests above all join
// at the TOP (cause itself is the join). These mirror them for a join
// sitting BENEATH an ordinary single-cause wrapper instead
// (operationError.Unwrap() returns the join — exactly as a real AWS SDK
// *smithy.OperationError could if a retry layer joined per-attempt errors
// together) — the shape an earlier version of sanitizeTree handled wrong by
// descending into the join looking for a host-bearing node and discarding
// the whole wrapper-plus-join branch, losing the safe sibling along with
// it.
// ----------------------------------------------------------------------------

// TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_PreservesSiblingMessage
// is the nested-join counterpart of
// TestSanitizeCause_FindsHostBearingNodeInsideJoinedError: the safe sibling
// joined alongside the transport failure must stay reachable — by its own
// Error() text through a full Unwrap walk, not merely by not leaking — even
// though operationError itself, whose own Error() interpolates its cause's
// text, is discarded along with the join it wraps.
func TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_PreservesSiblingMessage(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	sibling := errors.New("some other upload-path failure")
	joined := errors.Join(rawErr, sibling)
	wrapper := &operationError{service: "S3", op: "PutObject", err: joined}

	sanitized := SanitizeCause(wrapper)

	if !errors.Is(sanitized, sibling) {
		t.Fatal("errors.Is(sanitized, sibling) = false, want true — a safe sibling beneath an ordinary wrapper must survive")
	}

	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1", presignedLikeURL}
	if dump := fmt.Sprintf("%+v", sanitized); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks the presigned URL", dump)
	}

	var leakedAt, siblingSeenAt string
	walkBothUnwrapForms(t, sanitized, func(e error, depth int) {
		text := e.Error()
		if leakedAt == "" && containsAny(text, leaks) {
			leakedAt = fmt.Sprintf("depth %d (%T).Error() = %q", depth, e, text)
		}
		if siblingSeenAt == "" && text == sibling.Error() {
			siblingSeenAt = fmt.Sprintf("depth %d", depth)
		}
	})
	if leakedAt != "" {
		t.Fatalf("recursive walk (both Unwrap forms) found a leak: %s", leakedAt)
	}
	if siblingSeenAt == "" {
		t.Fatal("recursive walk (both Unwrap forms) never reached the safe sibling's own message beneath the wrapper")
	}
}

// TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_PreservesCancellationSiblings
// mirrors TestSanitizeCause_JoinedError_PreservesCancellationSiblings for a
// join nested beneath an ordinary wrapper instead of sitting at the top.
func TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_PreservesCancellationSiblings(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			rawErr := dialPresignedURLFailure(t)
			joined := errors.Join(sentinel, rawErr)
			wrapper := &operationError{service: "S3", op: "PutObject", err: joined}

			sanitized := SanitizeCause(wrapper)

			if !errors.Is(sanitized, sentinel) {
				t.Fatalf("errors.Is(sanitized, %v) = false, want true — a cancellation/deadline sibling beneath an ordinary wrapper must survive", sentinel)
			}
			leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1"}
			walkBothUnwrapForms(t, sanitized, func(e error, depth int) {
				if text := e.Error(); containsAny(text, leaks) {
					t.Fatalf("depth %d (%T).Error() = %q, leaks the presigned URL", depth, e, text)
				}
			})
		})
	}
}

// TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_PreservesSDKMarkerSibling
// mirrors TestSanitizeCause_JoinedError_PreservesSDKMarkerSibling for a join
// nested beneath an ordinary wrapper instead of sitting at the top.
func TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_PreservesSDKMarkerSibling(t *testing.T) {
	rawErr := dialPresignedURLFailure(t)
	joined := errors.Join(ErrCredentialServiceUnreachable, rawErr)
	wrapper := &operationError{service: "S3", op: "PutObject", err: joined}

	sanitized := SanitizeCause(wrapper)

	if !errors.Is(sanitized, ErrCredentialServiceUnreachable) {
		t.Fatal("errors.Is(sanitized, ErrCredentialServiceUnreachable) = false, want true — the SDK marker sibling beneath an ordinary wrapper must survive")
	}
	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1"}
	if containsAny(sanitized.Error(), leaks) {
		t.Fatalf("Error() = %q, leaks the presigned URL", sanitized.Error())
	}
}

// TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_NothingToSanitizeReturnsWrapperUnchanged
// is the negative/boundary case: when a join sits beneath an ordinary
// wrapper but NEITHER of its branches is network-transport-shaped, nothing
// needs discarding at all — the wrapper itself (not just its sanitized
// join) must come back byte-for-byte identical, exactly like
// TestSanitizeCause_NonNetworkCausePassesThroughUnchanged's contract for the
// no-join case.
func TestSanitizeCause_NestedJoinBeneathOrdinaryWrapper_NothingToSanitizeReturnsWrapperUnchanged(t *testing.T) {
	joined := errors.Join(errors.New("first safe failure"), errors.New("second safe failure"))
	wrapper := &operationError{service: "S3", op: "PutObject", err: joined}

	if got := SanitizeCause(wrapper); got != wrapper {
		t.Fatalf("SanitizeCause(wrapper) = %v (%T), want the exact same wrapper value unchanged", got, got)
	}
}

// ----------------------------------------------------------------------------
// Non-comparable safe siblings: nonComparableErr's dynamic type (a struct
// holding a []string, stored by VALUE so no pointer indirection makes it
// comparable again) is itself not comparable, per the Go language spec:
// comparing two interface values with an IDENTICAL dynamic type panics at
// runtime if that type is not comparable, independent of whether the values
// are equal. sanitizeTree's join-branch loop (`sc != child`) and its
// nested-join case (`sanitizedJoin != joinNode`) both used `!=` to detect
// whether a branch changed — but an UNCHANGED branch is returned as itself,
// so the moment a non-comparable error sits anywhere in a join tree,
// comparing it to itself panics, regardless of whether anything in the tree
// actually needed sanitizing. The stdlib's own errors.Is was hardened
// against exactly this shape (it never does a raw == against a
// non-comparable target — see errors.is's targetComparable guard); these
// tests prove sanitizeTree needed the same guard and didn't have it.
// ----------------------------------------------------------------------------

// nonComparableErr is a minimal safe error whose Error() method has a VALUE
// receiver and whose only field is a slice, so a variable of type error
// holding it stores the struct itself (not a pointer behind it), and that
// stored dynamic type is not comparable.
type nonComparableErr struct {
	tags []string
}

func (e nonComparableErr) Error() string {
	return "safe sibling failure: " + strings.Join(e.tags, ",")
}

// seenMessage reports whether an error carrying exactly want's Error() text
// is reachable by walking err's chain through both Unwrap() error and
// Unwrap() []error forms — the same reachability check
// TestSanitizeCause_FindsHostBearingNodeInsideJoinedError uses for its
// comparable sibling, here for one errors.Is itself cannot confirm (see the
// comment above): errors.Is deliberately never does a raw == against a
// non-comparable target, so it would report false for this sibling even
// when sanitizeTree preserves it correctly.
func seenMessage(t *testing.T, err error, want string) bool {
	t.Helper()
	found := false
	walkBothUnwrapForms(t, err, func(e error, _ int) {
		if e.Error() == want {
			found = true
		}
	})
	return found
}

// TestSanitizeCause_NonComparableSiblingInTopLevelJoin is regression (a): a
// non-comparable safe sibling joined directly alongside a genuine transport
// failure at the TOP level — cause itself is the join, the same shape as
// TestSanitizeCause_JoinedError_PreservesSDKMarkerSibling but with a sibling
// type the package cannot assume is comparable. On the pre-fix sanitizeTree
// this panics inside the join loop's `sc != child` comparison the instant it
// reaches the non-comparable sibling (negative control: this test panics,
// rather than fails an assertion, on the pre-fix code).
func TestSanitizeCause_NonComparableSiblingInTopLevelJoin(t *testing.T) {
	sibling := nonComparableErr{tags: []string{"upload-path", "warn"}}
	rawErr := dialPresignedURLFailure(t)
	joined := errors.Join(sibling, rawErr)

	err := Wrap("failed to upload to presigned URL", SanitizeCause(joined))

	if err.Error() != "failed to upload to presigned URL" {
		t.Fatalf("Error() = %q, want the authored message unchanged", err.Error())
	}
	if !seenMessage(t, err, sibling.Error()) {
		t.Fatal("the non-comparable safe sibling's own message is no longer reachable — it must survive untouched")
	}

	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1", presignedLikeURL}
	if dump := fmt.Sprintf("%+v", err); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks the presigned URL", dump)
	}
}

// TestSanitizeCause_NonComparableSiblingInNestedJoin is regression (b): the
// same non-comparable sibling, but joined beneath an ordinary wrapper
// (operationError) rather than at the top — the path that reaches the
// panic via sanitizeTree's joinNode recursion (firstHostBearingOrJoin finds
// the join one level down, sanitizeTree re-enters the join-branch loop from
// there) instead of the top-level multi-error branch. Same negative
// control: panics, pre-fix, inside that re-entered loop.
func TestSanitizeCause_NonComparableSiblingInNestedJoin(t *testing.T) {
	sibling := nonComparableErr{tags: []string{"upload-path", "warn"}}
	rawErr := dialPresignedURLFailure(t)
	joined := errors.Join(sibling, rawErr)
	wrapper := &operationError{service: "S3", op: "PutObject", err: joined}

	sanitized := SanitizeCause(wrapper)

	if !seenMessage(t, sanitized, sibling.Error()) {
		t.Fatal("the non-comparable safe sibling's own message is no longer reachable beneath the wrapper")
	}
	leaks := []string{"X-Amz-Credential", "X-Amz-Security-Token", "X-Amz-Signature", "acme-prod-bucket", "127.0.0.1", presignedLikeURL}
	if dump := fmt.Sprintf("%+v", sanitized); containsAny(dump, leaks) {
		t.Fatalf("%%+v = %q, leaks the presigned URL", dump)
	}
}

// TestSanitizeCause_NonComparableSiblingWithNothingToSanitize is regression
// (c): a join whose branches are BOTH safe — one of them non-comparable —
// with no host-bearing node anywhere in the tree. SanitizeCause should be a
// pure no-op here (nothing needs discarding, so the exact same join value
// comes back — a pointer comparison, always safe regardless of what it
// holds), but the pre-fix join loop still panics on `sc != child` purely
// from comparing the non-comparable branch to itself, proving the crash
// does not depend on a genuine transport failure being present at all —
// only on a non-comparable error sharing a join with anything else.
func TestSanitizeCause_NonComparableSiblingWithNothingToSanitize(t *testing.T) {
	sibling := nonComparableErr{tags: []string{"benign"}}
	joined := errors.Join(sibling, errors.New("some other safe failure"))

	if got := SanitizeCause(joined); got != joined {
		t.Fatalf("SanitizeCause(joined) = %v (%T), want the exact same join value unchanged — nothing here needed sanitizing", got, got)
	}
}

func TestCredentialServiceMessage(t *testing.T) {
	cases := []struct {
		status        int
		code, message string
		want          string
	}{
		{500, "internal_error", "boom", "Helix credential service error (internal_error): boom"},
		{503, "", "upstream unavailable", "Helix credential service error: upstream unavailable"},
		{502, "", "", "Helix credential service error: HTTP 502"},
		{500, "internal_error", "", "Helix credential service error (internal_error): HTTP 500"},
	}
	for _, tc := range cases {
		if got := CredentialServiceMessage(tc.status, tc.code, tc.message); got != tc.want {
			t.Errorf("CredentialServiceMessage(%d, %q, %q) = %q, want %q", tc.status, tc.code, tc.message, got, tc.want)
		}
	}
}

func TestIsRetryableIdentityStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{0, true},   // no response arrived at all
		{-1, true},  // defensive: treated the same as "no response"
		{408, true}, // request timeout
		{429, true}, // too many requests
		{500, true},
		{503, true},
		{599, true},
		// 600 is the negative boundary: statusCode >= 500 is not enough on
		// its own, the range must stop at 599. Before this fix the
		// condition was a bare ">= 500" and this case would wrongly be true.
		{600, false},
		{700, false},
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
	}
	for _, tc := range cases {
		if got := IsRetryableIdentityStatus(tc.status); got != tc.want {
			t.Errorf("IsRetryableIdentityStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestIdentityCheckTemporaryFailureMessage(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{0, "could not verify credentials: the identity check got no response, try again"},
		{503, "could not verify credentials: the identity check answered with a temporary error (HTTP 503), try again"},
		{429, "could not verify credentials: the identity check answered with a temporary error (HTTP 429), try again"},
		{408, "could not verify credentials: the identity check answered with a temporary error (HTTP 408), try again"},
	}
	for _, tc := range cases {
		got := IdentityCheckTemporaryFailureMessage(tc.status)
		if got != tc.want {
			t.Errorf("IdentityCheckTemporaryFailureMessage(%d) = %q, want %q", tc.status, got, tc.want)
		}
		if strings.Contains(got, "AWS") {
			t.Errorf("IdentityCheckTemporaryFailureMessage(%d) = %q, names a cloud provider; must stay provider-neutral", tc.status, got)
		}
	}
}
