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
	"net/http"
	"net/http/httptest"
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
	err := validateCredentials(context.Background(), fakeSTSAccessDenied(t), false)

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
// fmt.Errorf — safe ONLY because level one is already clean). A transport
// error carrying an ARN must still not surface anywhere in the final,
// doubly-wrapped message the customer sees.
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

	_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), NewUploadOptions("nested-test"))

	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), arnAccountService) || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the cause nested two levels deep", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "dataset record created but upload failed: failed to upload to presigned URL") {
		t.Fatalf("Error() = %q, want the doubly-wrapped clean message", err.Error())
	}
	inner := errors.Unwrap(err)
	if inner == nil {
		t.Fatal("errors.Unwrap(err) = nil at the first layer")
	}
	cause := errors.Unwrap(inner)
	if cause == nil || !strings.Contains(cause.Error(), arnAccountService) {
		t.Fatalf("unwrapped cause = %v, want the original ARN-carrying transport error reachable", cause)
	}
}

// failingPUTTransport lets the POST to the API server through (it never sees
// this transport — apiServer is a real httptest server reached directly) and
// fails only the PUT to the presigned URL, with a raw transport error
// carrying a signed-URL-shaped ARN.
type failingPUTTransport struct{}

func (failingPUTTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut {
		return nil, fmt.Errorf("dial tcp: connection reset talking to %s", req.URL.String())
	}
	return http.DefaultTransport.RoundTrip(req)
}

// ----------------------------------------------------------------------------
// makeAPIRequest (HTTP client transport failures).
// ----------------------------------------------------------------------------

func TestMakeAPIRequest_TransportFailureCauseNeverLeaksIntoMessage(t *testing.T) {
	p := newTestProducer("https://api.test")
	p.httpClient = &http.Client{Transport: arnTransport{}}

	err := p.makeAPIRequest(context.Background(), http.MethodGet, "/v1/datasets/ds-1", nil, nil)

	assertClean(t, err, "request failed")
	assertCauseReachable(t, err, arnAccountService)
}

type arnTransport struct{}

func (arnTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: connection refused talking to a host serving %s", arnAccountService)
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
func (f *failingReadCloser) Close() error              { return nil }

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
	assertCauseReachable(t, err, arnAccountService)
}
