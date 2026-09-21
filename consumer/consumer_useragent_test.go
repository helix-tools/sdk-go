// Tests pinning that every Helix API request Consumer builds carries the
// SDK-identifying User-Agent header (v2.15.0, see internal/useragent),
// that it is excluded from the SigV4 SignedHeaders set, and that the one
// deliberate exception — the presigned S3 download GET, whose signature
// this SDK does not control — is left untouched.
package consumer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/internal/useragent"
)

// wireFormatRe is the exact contract the api lane parses: the FIRST
// token of User-Agent must be "helix-sdk-go/<semver-without-v>",
// optionally followed by " (go/<runtime.Version()>)".
var wireFormatRe = regexp.MustCompile(`^helix-sdk-go/\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?( \(go/go[0-9.]+(rc\d+)?\))?$`)

// TestMakeAPIRequest_SetsUserAgent is the core positive case: every
// makeAPIRequest call (the builder behind GetDataset, GetDownloadURL,
// ListDatasets, ListSubscriptions, CreateSubscriptionRequest,
// recordOutcome, ...) must set the exact SDK User-Agent value.
func TestMakeAPIRequest_SetsUserAgent(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"ds-1","name":"d"}`))
	}))
	defer server.Close()

	c := newTestConsumer(server.URL)
	if _, err := c.GetDataset(context.Background(), "ds-1"); err != nil {
		t.Fatalf("GetDataset: %v", err)
	}

	if !wireFormatRe.MatchString(got) {
		t.Errorf("User-Agent = %q, does not match wire contract %s", got, wireFormatRe.String())
	}
	if want := useragent.String(); got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
}

// TestMakeAPIRequest_UserAgentNotInSignedHeaders proves the new header
// never joins the SigV4 SignedHeaders set — required because
// aws-sdk-go-v2's signer only ignores "User-Agent" by name during
// signing; a future signer swap or manual SignedHeaders override could
// silently start including it, which would make every real request's
// signature depend on the exact (non-deterministic across Go versions)
// User-Agent string and break in production.
func TestMakeAPIRequest_UserAgentNotInSignedHeaders(t *testing.T) {
	var captured *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"_id":"ds-1","name":"d"}`))
	}))
	defer server.Close()

	c := newTestConsumerWithCredentials(server.URL, staticCredsProvider())
	if _, err := c.GetDataset(context.Background(), "ds-1"); err != nil {
		t.Fatalf("GetDataset: %v", err)
	}

	if captured == nil {
		t.Fatal("server never received a request")
	}
	if ua := captured.Header.Get("User-Agent"); !wireFormatRe.MatchString(ua) {
		t.Fatalf("User-Agent = %q, does not match wire contract %s", ua, wireFormatRe.String())
	}
	signed := signedHeadersOf(t, captured.Header.Get("Authorization"))
	if containsHeader(signed, "user-agent") {
		t.Errorf("SignedHeaders = %v, must NOT include user-agent", signed)
	}
}

// TestDownloadDataset_PresignedGETExcludedFromSDKUserAgent runs the real
// DownloadDataset flow end-to-end against newFakeAPI and asserts the
// split: the two Helix API calls (dataset metadata GET, download-URL
// GET) carry the SDK User-Agent, while the presigned S3 GET does not —
// pinning the deliberate scope decision that this SDK never sets
// User-Agent on a presigned URL it did not generate and cannot prove is
// safe to decorate (see uploadToPresignedURL's producer-side analogue).
func TestDownloadDataset_PresignedGETExcludedFromSDKUserAgent(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestConsumer(f.server.URL)

	dir := t.TempDir()
	outPath := dir + "/out.bin"
	if err := c.DownloadDataset(context.Background(), "ds-1", outPath); err != nil {
		t.Fatalf("DownloadDataset: %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("expected output file to exist: %v", err)
	}

	calls := f.takeCaptured()
	var sawAPICall, sawS3Call bool
	for _, call := range calls {
		switch {
		case strings.HasPrefix(call.Path, "/s3-mock/"):
			sawS3Call = true
			if ua := call.Header.Get("User-Agent"); wireFormatRe.MatchString(ua) {
				t.Errorf("presigned S3 GET User-Agent = %q, must NOT carry the SDK format (this SDK does not control the presigned URL's signature)", ua)
			}
		case strings.HasPrefix(call.Path, "/v1/datasets/"):
			sawAPICall = true
			if ua := call.Header.Get("User-Agent"); !wireFormatRe.MatchString(ua) {
				t.Errorf("Helix API call %s User-Agent = %q, does not match wire contract %s", call.Path, ua, wireFormatRe.String())
			}
		}
	}
	if !sawAPICall {
		t.Fatal("test never observed a /v1/datasets/ call — fakeAPI routing changed?")
	}
	if !sawS3Call {
		t.Fatal("test never observed the /s3-mock/ call — fakeAPI routing changed?")
	}
}
