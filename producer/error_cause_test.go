// Tests for D19: every place this package wraps an upstream call's error
// (AWS SDK, HTTP client, compression library) into a customer-facing error
// must give the customer a clean, authored message — never the upstream
// error's own text, which can carry an AWS account ID, a key/queue ARN, or a
// service name — while still attaching that upstream error as the cause so
// errors.Is/errors.As (and a developer inspecting the error chain) can still
// reach it.
package producer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const arnAccountService = "arn:aws:iam::123456789012:user/test"

func assertClean(t *testing.T, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want an error")
	}
	if err.Error() != wantMsg {
		t.Fatalf("Error() = %q, want exactly %q", err.Error(), wantMsg)
	}
	if strings.Contains(err.Error(), arnAccountService) || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the upstream cause", err.Error())
	}
}

func assertCauseReachable(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	cause := errors.Unwrap(err)
	if cause == nil {
		t.Fatal("errors.Unwrap(err) = nil, want the original upstream error reachable")
	}
	if !strings.Contains(cause.Error(), wantSubstring) {
		t.Fatalf("unwrapped cause = %q, want it to contain %q", cause.Error(), wantSubstring)
	}
}

// assertSanitizedCauseReachable is assertCauseReachable's counterpart for
// the two call sites sdkerr.SanitizeCause is explicitly applied at: (a) a
// genuine no-response transport failure (http.Client.Do returning a nil
// *http.Response — an unreachable host, never a response-bearing error,
// which keeps using assertCauseReachable above), and (b) a request-
// construction failure (http.NewRequestWithContext failing on a malformed
// presigned URL) — a DIFFERENT path that never even reaches
// httpClient.Do, but is sanitized for the same reason: the URL it was
// given carries a SigV4 signature/credential scope. Either way, the cause
// stays non-nil (for debugging) but its own Error() text — at every depth
// the chain goes to — must never contain the host/URL substring the
// pre-fix code used to leak.
func assertSanitizedCauseReachable(t *testing.T, err error, neverContains string) {
	t.Helper()
	depth := 0
	for e := err; e != nil; e = errors.Unwrap(e) {
		depth++
		if depth > 1 && strings.Contains(e.Error(), neverContains) {
			t.Fatalf("chain node #%d (%T).Error() = %q, leaks %q", depth, e, e.Error(), neverContains)
		}
	}
	if depth < 2 {
		t.Fatalf("errors.Unwrap(err) = nil, want a sanitized cause still reachable for debugging")
	}
}

// ----------------------------------------------------------------------------
// validateCredentials (NewProducer's STS GetCallerIdentity call).
// ----------------------------------------------------------------------------

func fakeSTSAccessDenied(t *testing.T) *sts.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <Error>
    <Type>Sender</Type>
    <Code>AccessDenied</Code>
    <Message>User: ` + arnAccountService + ` is not authorized to perform: sts:GetCallerIdentity</Message>
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

func TestValidateCredentials_UpstreamCauseNeverLeaksIntoMessage(t *testing.T) {
	err := validateCredentials(context.Background(), fakeSTSAccessDenied(t), false, true)

	assertClean(t, err, "invalid AWS credentials")
	assertCauseReachable(t, err, arnAccountService)
}

// TestValidateCredentials_NegativeControl proves the fabricated STS response
// really does carry the ARN, by reproducing the pre-fix behavior (fmt.Errorf
// with %w) against the SAME upstream call and confirming THAT leaks.
func TestValidateCredentials_NegativeControl(t *testing.T) {
	stsClient := fakeSTSAccessDenied(t)
	_, rawErr := stsClient.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if rawErr == nil {
		t.Fatal("expected the fake STS server to return AccessDenied")
	}

	legacy := fmt.Errorf("invalid AWS credentials: %w", rawErr)
	if !strings.Contains(legacy.Error(), arnAccountService) {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}

// ----------------------------------------------------------------------------
// encryptData (KMS Encrypt).
// ----------------------------------------------------------------------------

func fakeKMSAccessDeniedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"User: ` + arnAccountService +
			` is not authorized to perform: kms:Encrypt on resource: arn:aws:kms:us-east-1:123456789012:key/abcd-1234"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEncryptData_KMSCauseNeverLeaksIntoMessage(t *testing.T) {
	kmsServer := fakeKMSAccessDeniedServer(t)
	p := newTestProducerWithKMS("https://api.test", kmsServer.URL)

	_, err := p.encryptData(context.Background(), []byte("compressed-bytes"))

	assertClean(t, err, "encryption failed")
	assertCauseReachable(t, err, arnAccountService)
}

// ----------------------------------------------------------------------------
// resolveEncryptionKeyID (GET /v1/self/producer-config).
// ----------------------------------------------------------------------------

func TestResolveEncryptionKeyID_APICauseNeverLeaksIntoMessage(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusForbidden, `{"error":"AccessDenied for `+arnAccountService+`"}`)

	_, err := newTestProducer(api.server.URL).resolveEncryptionKeyID(context.Background())

	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "uploads will fail") {
		t.Fatalf("Error() = %q, want the authored message unchanged", err.Error())
	}
	if strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("Error() = %q, leaks the raw upstream API error", err.Error())
	}
	assertClean(t, err, errEncryptionKeyUnresolved)
	assertCauseReachable(t, err, arnAccountService)
}

// ----------------------------------------------------------------------------
// UploadDataset's presigned-URL PUT — the bypass test named in the brief:
// an upstream error nested two levels deep must still not leak.
// ----------------------------------------------------------------------------

// TestUploadDataset_PresignedUploadCauseNestedTwoLevelsDeep drives the real
// UploadDataset path end-to-end: uploadToPresignedURL's own sdkerr.Wrap
// (level one) is re-wrapped by UploadDataset's
// "dataset record created but upload failed: %w" (level two, an ordinary
// fmt.Errorf — safe ONLY because level one is already clean). The presigned
// PUT URL itself carries a signed-URL-shaped ARN in its query string (via
// http.Client.Do's own *url.Error wrapping, exactly as a real presigned
// link would) and must not surface anywhere in the final, doubly-wrapped
// message OR anywhere in a full Unwrap walk past it.
func TestUploadDataset_PresignedUploadCauseNestedTwoLevelsDeep(t *testing.T) {
	var postBody map[string]any
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/datasets" && r.Method == http.MethodPost:
			raw := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(raw)
			_ = json.Unmarshal(raw, &postBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "ds-nested",
				"upload_url": "https://s3.example.invalid/bucket/key?X-Amz-Signature=" + arnAccountService,
				"s3_key":     "datasets/nested/data.ndjson.gz",
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	kmsServer := newFakeKMSServer(t, nil)
	defer kmsServer.Close()

	p := newTestProducerWithKMS(apiServer.URL, kmsServer.URL)
	p.httpClient = &http.Client{Transport: failingPUTTransport{}}

	_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("nested-test"))

	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), arnAccountService) || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the cause nested two levels deep", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "dataset record created but upload failed: failed to upload to presigned URL") {
		t.Fatalf("Error() = %q, want the doubly-wrapped clean message", err.Error())
	}
	assertSanitizedCauseReachable(t, err, arnAccountService)
}

// failingPUTTransport lets the POST to the API server through (it never sees
// this transport — apiServer is a real httptest server reached directly) and
// fails only the PUT to the presigned URL, with a generic connection-reset
// error (the shape a real dropped connection takes) — http.Client.Do wraps
// it in a *url.Error carrying the real presigned PUT URL (with its
// signed-URL-shaped ARN) in the URL field, exactly like a real presigned
// upload failure.
type failingPUTTransport struct{}

func (failingPUTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut {
		return nil, errors.New("connection reset by peer")
	}
	return http.DefaultTransport.RoundTrip(req)
}

// ----------------------------------------------------------------------------
// makeAPIRequest (HTTP client transport failures).
// ----------------------------------------------------------------------------

// TestMakeAPIRequest_TransportFailureCauseNeverLeaksIntoMessage covers the
// no-response case: arnTransport's connection-refused error gets wrapped by
// http.Client.Do in a *url.Error carrying the real (internal) API host in
// its URL field, which must never surface — at any Unwrap depth — even
// though a sanitized cause stays reachable for debugging.
func TestMakeAPIRequest_TransportFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{Transport: arnTransport{}}

	err := p.makeAPIRequest(context.Background(), http.MethodGet, "/v1/datasets/ds-1", nil, nil)

	assertClean(t, err, "request failed")
	assertSanitizedCauseReachable(t, err, "api.test")
}

// redirectResponse builds the *http.Response http.Client.Do sees for a 3xx
// with a Location header — used below to drive its CheckRedirect callback
// without a real httptest server (net/http itself reads and closes the
// body before deciding whether to follow it; http.NoBody is enough).
func redirectResponse(req *http.Request, location string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusFound,
		Status:     "302 Found",
		Header:     http.Header{"Location": []string{location}},
		Body:       http.NoBody,
		Request:    req,
	}
}

type redirectingTransport struct{}

func (redirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return redirectResponse(req, "https://api.test/redirected"), nil
}

// TestMakeAPIRequest_RefusedRedirectKeepsRawURLError: when the caller's own
// httpClient refuses a redirect via CheckRedirect, http.Client.Do returns
// the (non-nil) Response alongside the (non-nil) error together — the
// service DID answer, so this is not a genuine no-response transport
// failure and must never be run through SanitizeCause. Mirrors
// credentials.TestProvider_Mint_RefusedRedirectKeepsRawURLError.
func TestMakeAPIRequest_RefusedRedirectKeepsRawURLError(t *testing.T) {
	errRefused := errors.New("redirect refused by test policy")
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{
		Transport:     redirectingTransport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused },
	}

	err := p.makeAPIRequest(context.Background(), http.MethodGet, "/v1/datasets/ds-1", nil, nil)
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

// singleCloseBody errors if Close is called more than once — unlike
// http.NoBody (what redirectResponse above uses), which tolerates a second
// Close silently. net/http has already closed the response body itself by
// the time a refused-redirect error comes back (Client.Do's own doc: "even
// then the returned Response.Body is already closed"), so a wrap site
// closing it again on top of that is a double Close that this type makes
// caller-visible instead of silently swallowed.
type singleCloseBody struct{ closeCalls int }

func (b *singleCloseBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *singleCloseBody) Close() error {
	b.closeCalls++
	if b.closeCalls > 1 {
		return fmt.Errorf("Close called %d times, want at most 1", b.closeCalls)
	}
	return nil
}

// singleCloseRedirectingTransport is redirectingTransport, but with a
// caller-supplied body the test can inspect afterward instead of the
// shared, Close-tolerant http.NoBody.
type singleCloseRedirectingTransport struct{ body *singleCloseBody }

func (t singleCloseRedirectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusFound,
		Status:     "302 Found",
		Header:     http.Header{"Location": []string{"https://api.test/redirected"}},
		Body:       t.body,
		Request:    req,
	}, nil
}

// TestMakeAPIRequest_RefusedRedirectClosesBodyExactlyOnce is the regression
// test for the double-Close fix: net/http already closes resp.Body itself
// before returning a refused-redirect error, so makeAPIRequest's own
// explicit Close call on that branch — removed by this fix — must never
// run. A second Close call on singleCloseBody surfaces as an error from
// Close, not a leaked raw resp.Body.Close() success, so this fails loudly
// if the double-Close regresses.
func TestMakeAPIRequest_RefusedRedirectClosesBodyExactlyOnce(t *testing.T) {
	body := &singleCloseBody{}
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{
		Transport:     singleCloseRedirectingTransport{body: body},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused by test policy") },
	}

	err := p.makeAPIRequest(context.Background(), http.MethodGet, "/v1/datasets/ds-1", nil, nil)
	if err == nil {
		t.Fatal("expected an error for a refused redirect")
	}
	if body.closeCalls != 1 {
		t.Fatalf("resp.Body.Close was called %d time(s), want exactly 1", body.closeCalls)
	}
}

type arnTransport struct{}

func (arnTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// TestMakeAPIRequest_DecodeFailureCauseNeverLeaksIntoMessage covers
// json.NewDecoder(resp.Body).Decode: json.Decoder does not wrap a Read
// error, so a connection reset mid-response surfaces exactly as it would
// from resp.Body.Read directly.
func TestMakeAPIRequest_DecodeFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{Transport: decodeFailsTransport{}}

	var out struct{ ID string }
	err := p.makeAPIRequest(context.Background(), http.MethodGet, "/v1/datasets/ds-1", nil, &out)

	assertClean(t, err, "failed to decode response")
	assertCauseReachable(t, err, arnAccountService)
}

type decodeFailsTransport struct{}

func (decodeFailsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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
func (f *failingReadCloser) Close() error             { return nil }

// ----------------------------------------------------------------------------
// uploadToPresignedURL's request-build failure (a malformed presigned URL).
// ----------------------------------------------------------------------------

// TestUploadToPresignedURL_RequestBuildFailureCauseNeverLeaksIntoMessage
// covers http.NewRequestWithContext failing on a malformed presigned URL —
// the URL itself carries a SigV4 signature/credential scope, so the raw
// *url.Error must never reach the caller.
func TestUploadToPresignedURL_RequestBuildFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	p := newTestProducer("https://api.test")

	err := p.uploadToPresignedURL(context.Background(), "https://s3.example.invalid/%zz?X-Amz-Signature="+arnAccountService, []byte("data"))

	assertClean(t, err, "failed to create upload request")
	assertSanitizedCauseReachable(t, err, arnAccountService)
}

// TestUploadToPresignedURL_RefusedRedirectKeepsRawURLError is
// TestMakeAPIRequest_RefusedRedirectKeepsRawURLError's counterpart for
// uploadToPresignedURL's own httpClient.Do call, the other sdkerr.Wrap
// call site this package's HTTP wrapping audit covers.
func TestUploadToPresignedURL_RefusedRedirectKeepsRawURLError(t *testing.T) {
	errRefused := errors.New("redirect refused by test policy")
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{
		Transport:     redirectingTransport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused },
	}

	err := p.uploadToPresignedURL(context.Background(), "https://s3.example.invalid/key?X-Amz-Signature=deadbeef", []byte("data"))
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

// TestUploadToPresignedURL_RefusedRedirectClosesBodyExactlyOnce is
// TestMakeAPIRequest_RefusedRedirectClosesBodyExactlyOnce's counterpart for
// uploadToPresignedURL's own httpClient.Do call, the other site this
// package's double-Close fix touches.
func TestUploadToPresignedURL_RefusedRedirectClosesBodyExactlyOnce(t *testing.T) {
	body := &singleCloseBody{}
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{
		Transport:     singleCloseRedirectingTransport{body: body},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused by test policy") },
	}

	err := p.uploadToPresignedURL(context.Background(), "https://s3.example.invalid/key?X-Amz-Signature=deadbeef", []byte("data"))
	if err == nil {
		t.Fatal("expected an error for a refused redirect")
	}
	if body.closeCalls != 1 {
		t.Fatalf("resp.Body.Close was called %d time(s), want exactly 1", body.closeCalls)
	}
}

// TestValidateCredentials_APIKeyCallerNeverToldAWSKeys: when the identity
// service rejects an API-key caller's minted credentials, the message names
// the credential service, not AWS keys they never configured, and the
// upstream ARN stays out of it but reachable.
func TestValidateCredentials_APIKeyCallerNeverToldAWSKeys(t *testing.T) {
	err := validateCredentials(context.Background(), fakeSTSAccessDenied(t), true, false)

	assertClean(t, err, "Helix credential service error: could not get working credentials for this API key")
	assertCauseReachable(t, err, arnAccountService)
}
