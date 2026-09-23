// Tests pinning that every Helix API request Producer builds carries the
// SDK-identifying User-Agent header (v2.15.0, see internal/useragent),
// that it is excluded from the SigV4 SignedHeaders set, and that the one
// deliberate exception — the presigned S3 upload PUT, whose signature
// this SDK does not control — is left untouched (mirrors the consumer
// package's analogous presigned-download exclusion).
package producer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/helix-tools/sdk-go/v2/internal/useragent"
)

// wireFormatRe is the exact contract the api lane parses: the FIRST
// token of User-Agent must be "helix-sdk-go/<semver-without-v>",
// optionally followed by " (go/<runtime.Version()>)".
var wireFormatRe = regexp.MustCompile(`^helix-sdk-go/\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?( \(go/go[0-9.]+(rc\d+)?\))?$`)

// TestMakeAPIRequest_SetsUserAgent is the core positive case: every
// makeAPIRequest call (the builder behind ListMyDatasets,
// GetDatasetSubscribers, UpdateDataset, ...) must set the exact SDK
// User-Agent value.
func TestMakeAPIRequest_SetsUserAgent(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"datasets":[],"total_count":0,"page":1,"limit":100,"total_pages":0}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	if _, err := p.ListMyDatasets(context.Background()); err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}

	if !wireFormatRe.MatchString(got) {
		t.Errorf("User-Agent = %q, does not match wire contract %s", got, wireFormatRe.String())
	}
	if want := useragent.String(); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

// TestMakeAPIRequest_UserAgentNotInSignedHeaders proves the new header
// never joins the SigV4 SignedHeaders set — same rationale as the
// consumer package's analogous test.
func TestMakeAPIRequest_UserAgentNotInSignedHeaders(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"datasets":[],"total_count":0,"page":1,"limit":100,"total_pages":0}`))
	}))
	defer server.Close()

	p := newTestProducerWithCredentials(server.URL, staticCredsProviderForTests())
	if _, err := p.ListMyDatasets(context.Background()); err != nil {
		t.Fatalf("ListMyDatasets: %v", err)
	}

	if captured == nil {
		t.Fatal("server never received a request")
	}
	if ua := captured.Header.Get("User-Agent"); !wireFormatRe.MatchString(ua) {
		t.Fatalf("User-Agent = %q, does not match wire contract %s", ua, wireFormatRe.String())
	}
	signed := signedHeadersOfProducer(t, captured.Header.Get("Authorization"))
	if containsHeaderProducer(signed, "user-agent") {
		t.Errorf("SignedHeaders = %v, must NOT include user-agent", signed)
	}
}

// TestUploadToPresignedURL_ExcludedFromSDKUserAgent pins the deliberate
// scope decision that uploadToPresignedURL (the PUT to a presigned S3
// upload URL) never carries the SDK User-Agent — this SDK does not
// generate that URL's signature and cannot prove decorating it is safe,
// mirroring consumer.DownloadDataset's presigned-GET exclusion.
func TestUploadToPresignedURL_ExcludedFromSDKUserAgent(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	if err := p.uploadToPresignedURL(context.Background(), server.URL, []byte("payload")); err != nil {
		t.Fatalf("uploadToPresignedURL: %v", err)
	}

	if wireFormatRe.MatchString(got) {
		t.Errorf("presigned upload PUT User-Agent = %q, must NOT carry the SDK format (this SDK does not control the presigned URL's signature)", got)
	}
}
