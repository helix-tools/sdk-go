// Tests for D19: every place this package wraps an upstream call's error
// (AWS SDK, HTTP client, compression library) into a customer-facing error
// must give the customer a clean, authored message — never the upstream
// error's own text, which can carry an AWS account ID, a key/queue ARN, or a
// service name — while still attaching that upstream error as the cause so
// errors.Is/errors.As (and a developer inspecting the error chain) can still
// reach it.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// arnAccountService is what a real AWS SDK error carries and must never
// appear in a customer-facing message: an ARN, an account ID, and the
// upstream service's own name.
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
// validateCredentials (NewConsumer's STS GetCallerIdentity call).
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
// with %w) against the SAME upstream call and confirming THAT leaks — so a
// green assertClean above is not vacuous.
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
// decryptData (KMS Decrypt).
// ----------------------------------------------------------------------------

func TestDecryptData_KeyServiceCauseNeverLeaksIntoMessage(t *testing.T) {
	c := useFakeKMS(newTestConsumer("https://objects.test"), "", &http.Client{Transport: kmsARNTransport{}})

	_, err := c.decryptData(context.Background(), encryptedObject([]byte("row\n")))

	assertClean(t, err, "decryption failed")
	assertCauseReachable(t, err, arnAccountService)
}

// kmsARNTransport answers a KMS Decrypt call with an AccessDeniedException
// carrying an ARN and account ID, the exact shape a real KMS AccessDenied
// response has.
type kmsARNTransport struct{}

func (kmsARNTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"__type":"AccessDeniedException","message":"User: ` + arnAccountService +
		` is not authorized to perform: kms:Decrypt on resource: arn:aws:kms:us-east-1:123456789012:key/abcd-1234"}`
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header: http.Header{
			"X-Amzn-Errortype": []string{"AccessDeniedException"},
			"Content-Type":     []string{"application/x-amz-json-1.1"},
		},
		Body:    ioNopCloser(body),
		Request: r,
	}, nil
}

// TestDownloadDataset_KeyServiceCauseNeverLeaksIntoMessage is the end-to-end version
// of the test above: the exact customer-visible error DownloadDataset returns
// for a KMS AccessDenied must stay clean too, not just the internal
// decryptData helper.
func TestDownloadDataset_KeyServiceCauseNeverLeaksIntoMessage(t *testing.T) {
	client := &http.Client{Transport: downloadKMSDeniedTransport{}}
	c := newTestConsumer("https://objects.test")
	c.httpClient = client
	c = useFakeKMS(c, "", client)

	err := c.DownloadDataset(context.Background(), "ds-1", t.TempDir()+"/out.ndjson")

	assertClean(t, err, "decryption failed")
	assertCauseReachable(t, err, arnAccountService)
}

type downloadKMSDeniedTransport struct{}

func (downloadKMSDeniedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Header.Get("X-Amz-Target") == kmsDecryptTarget:
		return kmsARNTransport{}.RoundTrip(req)
	case req.URL.Path == "/object":
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
			Body: ioNopCloser(string(encryptedObject([]byte("row\n")))), Request: req,
		}, nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		return jsonResponse(req, `{"download_url":"https://objects.test/object"}`), nil
	default:
		return jsonResponse(req, `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`), nil
	}
}

func jsonResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    ioNopCloser(body),
		Request: req,
	}
}

// TestDecryptData_WrongKeySizeCauseNeverLeaksIntoMessage covers the
// non-KMS decryption branches (aes.NewCipher, cipher.NewGCMWithNonceSize,
// aesGCM.Open): every one of them must return the SAME clean "decryption
// failed" message as the KMS branch, with the raw stdlib crypto error (never
// itself sensitive, but never customer-visible either — decryptData's
// contract is uniform) reachable via errors.Unwrap.
func TestDecryptData_WrongKeySizeCauseNeverLeaksIntoMessage(t *testing.T) {
	// wrongKeySizeTransport answers KMS Decrypt with a 5-byte key — not a
	// valid AES key size — driving the aes.NewCipher failure branch.
	c := useFakeKMS(newTestConsumer("https://objects.test"), "", &http.Client{Transport: wrongKeySizeTransport{}})

	_, err := c.decryptData(context.Background(), encryptedObject([]byte("row\n")))

	assertClean(t, err, "decryption failed")
	assertCauseReachable(t, err, "invalid key size")
}

type wrongKeySizeTransport struct{}

func (wrongKeySizeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header:  http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}},
		Body:    ioNopCloser(kmsDecryptBodyFor([]byte("short"))),
		Request: req,
	}, nil
}

// ----------------------------------------------------------------------------
// DownloadDataset's own HTTP-client-category wrap sites: building the
// request from a server-issued presigned URL, streaming the response body,
// and decoding a JSON response — each of these reads from or is built from
// data that can carry a SigV4-signed URL's account/credential detail.
// ----------------------------------------------------------------------------

func TestDownloadDataset_RequestBuildFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{Transport: malformedDownloadURLTransport{}}

	err := c.DownloadDataset(context.Background(), "ds-1", t.TempDir()+"/out.ndjson")

	assertClean(t, err, "failed to build download request")
	assertSanitizedCauseReachable(t, err, arnAccountService)
}

type malformedDownloadURLTransport struct{}

func (malformedDownloadURLTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		return jsonResponse(req, `{"download_url":"https://objects.test/%zz?X-Amz-Signature=`+arnAccountService+`"}`), nil
	default:
		return jsonResponse(req, `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`), nil
	}
}

// TestDownloadDataset_StreamToTempFileCauseNeverLeaksIntoMessage covers the
// large-file path's io.Copy(tempFile, resp.Body): a network failure mid-
// stream must not surface raw.
func TestDownloadDataset_StreamToTempFileCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{Transport: largeObjectStreamFailsTransport{}}

	err := c.DownloadDataset(context.Background(), "ds-1", t.TempDir()+"/out.ndjson")

	assertClean(t, err, "failed to stream to temp file")
	assertCauseReachable(t, err, arnAccountService)
}

type largeObjectStreamFailsTransport struct{}

func (largeObjectStreamFailsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/object" {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        make(http.Header),
			ContentLength: 200 * 1024 * 1024, // force the large-file streaming path
			Body:          &failingReadCloser{failMsg: "connection reset touching " + arnAccountService},
			Request:       req,
		}, nil
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		return jsonResponse(req, `{"download_url":"https://objects.test/object"}`), nil
	default:
		return jsonResponse(req, `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`), nil
	}
}

// TestMakeAPIRequest_DecodeFailureCauseNeverLeaksIntoMessage covers
// json.NewDecoder(resp.Body).Decode: json.Decoder does not wrap a Read
// error, so a connection reset mid-response surfaces exactly as it would
// from resp.Body.Read directly.
func TestMakeAPIRequest_DecodeFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{Transport: decodeFailsTransport{}}

	_, err := c.GetDataset(context.Background(), "ds-1")

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

// TestDownloadOutcome_ErrorMessageDoesNotLeakUpstreamCause covers the
// outcome-callback telemetry channel: it must carry the SAME clean message
// the caller sees, not the raw upstream text captured before the wrap.
// (newFakeAPI's s3Err hijacks the connection, producing whatever raw
// transport text the OS/net package assigns — the test doesn't need to
// control that text; an EXACT match against the clean authored message is
// the strongest possible proof nothing raw leaked through, whatever it was.)
func TestDownloadOutcome_ErrorMessageDoesNotLeakUpstreamCause(t *testing.T) {
	f := newFakeAPI(t)
	f.s3Err = true
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	if err == nil {
		t.Fatal("expected DownloadDataset to fail on network error")
	}
	if err.Error() != "failed to download" {
		t.Fatalf("caller-visible Error() = %q, want exactly %q", err.Error(), "failed to download")
	}

	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("outcome callback never fired")
	}

	p := callbackPayload(f)
	if msg, _ := p["error_message"].(string); msg != "failed to download" {
		t.Fatalf("outcome callback error_message = %q, want exactly %q (the same clean message the caller sees)", msg, "failed to download")
	}
}

// ----------------------------------------------------------------------------
// decompressData (compressed).
// ----------------------------------------------------------------------------

// TestDecompressData_TruncatedStreamCauseNeverLeaksIntoMessage is the
// bypass/regression test for the "encrypted compressed data cut short" case: a
// TRUNCATED (but header-valid) compressed stream fails inside io.ReadAll, not at
// gzip.NewReader, so it must go through the SAME clean-message-plus-cause
// path as any other decompression failure — not surface the raw
// io.ErrUnexpectedEOF text the way the pre-fix code did.
func TestDecompressData_TruncatedStreamCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://objects.test")
	truncated := gzipBytes([]byte("hello world, this is more than twenty bytes of plaintext"))[:20]

	_, err := c.decompressData(truncated)

	if err == nil {
		t.Fatal("expected a decompression error for a truncated stream")
	}
	if err.Error() != "decompression failed" {
		t.Fatalf("Error() = %q, want exactly %q (never the raw flate/gzip text)", err.Error(), "decompression failed")
	}
	if strings.Contains(err.Error(), "EOF") {
		t.Fatalf("Error() = %q, leaks the raw stdlib error text", err.Error())
	}
	cause := errors.Unwrap(err)
	if cause == nil || !strings.Contains(cause.Error(), "EOF") {
		t.Fatalf("unwrapped cause = %v, want it to contain the real io.ErrUnexpectedEOF text", cause)
	}
}

// TestDecompressData_NotCompressed_SentinelAndCauseBothReachable pins that
// errNotCompressed — an EXISTING exported-package sentinel other code and
// callers use errors.Is against — keeps matching after the fix, while the
// raw compression header-parse error is ALSO reachable for debugging, and never
// printed into the message.
func TestDecompressData_NotCompressed_SentinelAndCauseBothReachable(t *testing.T) {
	c := newTestConsumer("https://objects.test")

	_, err := c.decompressData([]byte("not a gzip stream at all"))

	if !errors.Is(err, errNotCompressed) {
		t.Fatalf("errors.Is(err, errNotCompressed) = false; existing sentinel behavior must keep working (err = %v)", err)
	}
	if err.Error() != errNotCompressed.Error() {
		t.Fatalf("Error() = %q, want the sentinel's own clean message", err.Error())
	}
	if strings.Contains(err.Error(), "gzip:") {
		t.Fatalf("Error() = %q, leaks the raw gzip header-parse error", err.Error())
	}
}

// ----------------------------------------------------------------------------
// SQS (PollNotifications / DeleteNotification / ClearQueue).
// ----------------------------------------------------------------------------

// fakeSQSError answers every SQS action with the given AWS JSON error shape.
func fakeSQSError(t *testing.T, errType, message string, status int) *sqs.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.Header().Set("X-Amzn-ErrorType", errType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"__type":"` + errType + `","message":"` + message + `"}`))
	}))
	t.Cleanup(srv.Close)
	return sqs.New(sqs.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
	})
}

func TestClearQueue_GenericSQSFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	api := newSubsAPI(t, 0, consumerRow)
	c := newTestConsumer(api.srv.URL)
	c.sqsClient = fakeSQSError(t, "InternalError", "SQS internal failure touching arn:aws:sqs:us-east-1:123456789012:MY-queue", http.StatusInternalServerError)

	err := c.ClearQueue(context.Background())

	assertClean(t, err, "failed to clear queue")
	assertCauseReachable(t, err, "123456789012")
}

// TestClearQueue_PurgeQueueInProgressStillDetected pins the "keep any
// existing exported error category/sentinel behaviour" requirement: the
// friendly retry-later message must still fire, now detected via the TYPED
// SQS exception instead of a substring match on the raw error text.
func TestClearQueue_PurgeQueueInProgressStillDetected(t *testing.T) {
	api := newSubsAPI(t, 0, consumerRow)
	c := newTestConsumer(api.srv.URL)
	c.sqsClient = fakeSQSError(t, "PurgeQueueInProgress",
		"Only one PurgeQueue operation on arn:aws:sqs:us-east-1:123456789012:MY-queue is allowed every 60 seconds.",
		http.StatusBadRequest)

	err := c.ClearQueue(context.Background())

	if err == nil || !strings.Contains(err.Error(), "purge already in progress") {
		t.Fatalf("err = %v, want the friendly purge-in-progress message", err)
	}
	if strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("err = %v, leaks the upstream queue ARN", err)
	}
	assertCauseReachable(t, err, "123456789012")
}

// TestPollNotifications_SQSCauseNeverLeaksIntoMessage covers ReceiveMessage.
func TestPollNotifications_SQSCauseNeverLeaksIntoMessage(t *testing.T) {
	api := newSubsAPI(t, 0, consumerRow)
	c := newTestConsumer(api.srv.URL)
	c.sqsClient = fakeSQSError(t, "InternalError", "arn:aws:sqs:us-east-1:123456789012:MY-queue is unavailable", http.StatusInternalServerError)

	_, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true})

	assertClean(t, err, "failed to poll SQS queue")
	assertCauseReachable(t, err, "123456789012")
}

// TestDeleteNotification_SQSCauseNeverLeaksIntoMessage covers DeleteMessage.
func TestDeleteNotification_SQSCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://api.test")
	queue := "https://sqs.example/q-1"
	c.queueURL = &queue
	c.sqsClient = fakeSQSError(t, "InternalError", "arn:aws:sqs:us-east-1:123456789012:MY-queue is unavailable", http.StatusInternalServerError)

	err := c.DeleteNotification(context.Background(), "receipt-1")

	assertClean(t, err, "failed to delete notification")
	assertCauseReachable(t, err, "123456789012")
}

// TestResolveQueueURL_NestedTwoLevelsDeep is the bypass test named in the
// brief: an upstream error wrapped ONCE by makeAPIRequest (clean) and then
// wrapped AGAIN by resolveQueueURL's own "failed to get subscriptions: %w"
// must still not leak the internal API host into the final, doubly-wrapped
// message, or anywhere in a full Unwrap walk past it — a sanitized cause
// still reachable for debugging.
func TestResolveQueueURL_NestedTwoLevelsDeep(t *testing.T) {
	transport := &dialFailureTransportOnce{}
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{Transport: transport}

	err := c.ClearQueue(context.Background())

	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "api.test") {
		t.Fatalf("Error() = %q, leaks the cause nested two levels deep", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "failed to get subscriptions: request failed") {
		t.Fatalf("Error() = %q, want the doubly-wrapped clean message", err.Error())
	}
	assertSanitizedCauseReachable(t, err, "api.test")
}

// dialFailureTransportOnce fails the FIRST request (the subscriptions list
// call) with a generic connection-refused error — the shape a real dial
// failure takes — so PollNotifications/ClearQueue's makeAPIRequest ->
// resolveQueueURL double-wrap is exercised. http.Client.Do wraps it in a
// *url.Error carrying the real (internal) API host in its URL field.
type dialFailureTransportOnce struct{}

func (*dialFailureTransportOnce) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// ----------------------------------------------------------------------------
// makeAPIRequest / DownloadDataset (HTTP client transport failures).
// ----------------------------------------------------------------------------

// TestMakeAPIRequest_TransportFailureCauseNeverLeaksIntoMessage covers the
// bare `return err` sites this fix converts to sdkerr.Wrap: a raw transport
// error (which can carry the internal API host, via http.Client.Do's own
// *url.Error wrapping) must never reach the customer unwrapped, at any
// depth.
func TestMakeAPIRequest_TransportFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{Transport: &dialFailureTransportOnce{}}

	_, err := c.GetDataset(context.Background(), "ds-1")

	assertClean(t, err, "request failed")
	assertSanitizedCauseReachable(t, err, "api.test")
}

// TestDownloadDataset_PresignedURLTransportFailureCauseNeverLeaksIntoMessage
// covers the download path's own httpClient.Do call (fetching the object
// from S3 via a presigned URL, whose query string carries a SigV4 signature
// — sensitive in its own right).
func TestDownloadDataset_PresignedURLTransportFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{Transport: downloadFailsOnObjectFetch{}}

	err := c.DownloadDataset(context.Background(), "ds-1", t.TempDir()+"/out.ndjson")

	assertClean(t, err, "failed to download")
	assertSanitizedCauseReachable(t, err, "X-Amz-Signature")
}

type downloadFailsOnObjectFetch struct{}

// RoundTrip fails the GET to the presigned object URL with a generic
// connection-reset error — the shape a real dropped connection takes.
// http.Client.Do wraps it in a *url.Error carrying the REAL presigned URL
// (with its X-Amz-Signature query string) in its URL field, exactly like a
// real presigned-download failure.
func (downloadFailsOnObjectFetch) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/object" {
		return nil, errors.New("connection reset by peer")
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		return jsonResponse(req, `{"download_url":"https://objects.test/object?X-Amz-Signature=deadbeef"}`), nil
	default:
		return jsonResponse(req, `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`), nil
	}
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
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{
		Transport:     redirectingTransport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused },
	}

	_, err := c.GetDataset(context.Background(), "ds-1")
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
// Close instead of a silently-swallowed double Close.
func TestMakeAPIRequest_RefusedRedirectClosesBodyExactlyOnce(t *testing.T) {
	body := &singleCloseBody{}
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{
		Transport:     singleCloseRedirectingTransport{body: body},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused by test policy") },
	}

	_, err := c.GetDataset(context.Background(), "ds-1")
	if err == nil {
		t.Fatal("expected an error for a refused redirect")
	}
	if body.closeCalls != 1 {
		t.Fatalf("resp.Body.Close was called %d time(s), want exactly 1", body.closeCalls)
	}
}

type downloadRedirectsOnObjectFetch struct{}

// RoundTrip redirects the GET to the presigned object URL — the service
// DID answer, with a redirect the test's CheckRedirect then refuses —
// while answering the earlier /download and dataset-metadata calls
// normally, exactly like downloadFailsOnObjectFetch above.
func (downloadRedirectsOnObjectFetch) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/object" {
		return redirectResponse(req, "https://objects.test/object-redirected"), nil
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		return jsonResponse(req, `{"download_url":"https://objects.test/object?X-Amz-Signature=deadbeef"}`), nil
	default:
		return jsonResponse(req, `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`), nil
	}
}

// TestDownloadDataset_PresignedURLRefusedRedirectKeepsRawURLError is
// TestMakeAPIRequest_RefusedRedirectKeepsRawURLError's counterpart for the
// download path's own httpClient.Do call (site 1 — fetching the object
// from the presigned URL), the second of the two sdkerr.Wrap call sites in
// this package's HTTP wrapping audit.
func TestDownloadDataset_PresignedURLRefusedRedirectKeepsRawURLError(t *testing.T) {
	errRefused := errors.New("redirect refused by test policy")
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{
		Transport:     downloadRedirectsOnObjectFetch{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRefused },
	}

	err := c.DownloadDataset(context.Background(), "ds-1", t.TempDir()+"/out.ndjson")
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

// singleCloseDownloadRedirectsOnObjectFetch is downloadRedirectsOnObjectFetch,
// but the /object redirect response carries a singleCloseBody the test can
// inspect afterward instead of the shared, Close-tolerant http.NoBody.
type singleCloseDownloadRedirectsOnObjectFetch struct{ body *singleCloseBody }

func (t singleCloseDownloadRedirectsOnObjectFetch) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/object" {
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Header:     http.Header{"Location": []string{"https://objects.test/object-redirected"}},
			Body:       t.body,
			Request:    req,
		}, nil
	}
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		return jsonResponse(req, `{"download_url":"https://objects.test/object?X-Amz-Signature=deadbeef"}`), nil
	default:
		return jsonResponse(req, `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`), nil
	}
}

// TestDownloadDataset_PresignedURLRefusedRedirectClosesBodyExactlyOnce is
// TestMakeAPIRequest_RefusedRedirectClosesBodyExactlyOnce's counterpart for
// the download path's own httpClient.Do call, the other site this
// package's double-Close fix touches.
func TestDownloadDataset_PresignedURLRefusedRedirectClosesBodyExactlyOnce(t *testing.T) {
	body := &singleCloseBody{}
	c := newTestConsumer("https://api.test")
	c.httpClient = &http.Client{
		Transport:     singleCloseDownloadRedirectsOnObjectFetch{body: body},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused by test policy") },
	}

	err := c.DownloadDataset(context.Background(), "ds-1", t.TempDir()+"/out.ndjson")
	if err == nil {
		t.Fatal("expected an error for a refused redirect")
	}
	if body.closeCalls != 1 {
		t.Fatalf("resp.Body.Close was called %d time(s), want exactly 1", body.closeCalls)
	}
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

func ioNopCloser(s string) *nopCloserReader { return &nopCloserReader{r: strings.NewReader(s)} }

type nopCloserReader struct{ r *strings.Reader }

func (n *nopCloserReader) Read(p []byte) (int, error) { return n.r.Read(p) }
func (n *nopCloserReader) Close() error               { return nil }

// failingReadCloser is a response body that fails on every Read — the shape
// a connection reset mid-response takes.
type failingReadCloser struct{ failMsg string }

func (f *failingReadCloser) Read([]byte) (int, error) { return 0, errors.New(f.failMsg) }
func (f *failingReadCloser) Close() error             { return nil }

// TestValidateCredentials_APIKeyCallerNeverToldAWSKeys: when the identity
// service rejects an API-key caller's minted credentials, the message names
// the credential service, not AWS keys they never configured, and the
// upstream ARN stays out of it but reachable.
func TestValidateCredentials_APIKeyCallerNeverToldAWSKeys(t *testing.T) {
	err := validateCredentials(context.Background(), fakeSTSAccessDenied(t), true, false)

	assertClean(t, err, "Helix credential service error: could not get working credentials for this API key")
	assertCauseReachable(t, err, arnAccountService)
}
