// Tests for the v2 SDK outcome callback in DownloadDataset.
//
// The flow under test (see consumer.go:DownloadDataset):
//  1. Server-side recorder writes a `dataset_download_events` row at
//     URL-issue time and returns its event_id in the response.
//  2. The SDK download proceeds; outcome (success or per-phase error)
//     is reported back via POST /v1/datasets/:id/download-events with
//     a fire-and-forget call.
//  3. Callback failure NEVER affects the caller's experience.
//
// These tests pin every observable contract: which fields land in the
// callback, what error_category each failure phase produces, the
// filesystem-path scrubbing in error_message, and the swallow-the-
// callback-error guarantee. Mirrors the 11 jest cases in
// sdk/typescript/test/downloadOutcomeCallback.test.ts.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// capturedCall records one inbound HTTP request to the fake API. Tests
// inspect captured POST bodies to assert the callback's wire shape.
type capturedCall struct {
	Method  string
	Path    string
	Payload map[string]any
}

// fakeAPI is a per-test httptest.Server that routes by URL pattern. The
// `urlInfo` factory lets each test pick its own happy/sad URL response,
// and `callbackErr` makes the callback POST return 500 to test the
// best-effort guarantee.
type fakeAPI struct {
	t           *testing.T
	server      *httptest.Server
	mu          sync.Mutex
	captured    []capturedCall
	urlInfo     func() *DownloadURLInfo // nil ⇒ no event_id (older API)
	dataset     map[string]any          // metadata response
	datasetErr  bool                    // when true, GET /v1/datasets/:id returns 500
	s3Status    int                     // status code for the signed-URL fetch
	s3Body      []byte                  // body for the signed-URL fetch
	s3Err       bool                    // when true, signed-URL fetch hangs up
	callbackErr bool                    // when true, POST download-events returns 500
}

// captured returns a snapshot of every inbound request recorded so far.
func (f *fakeAPI) takeCaptured() []capturedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedCall, len(f.captured))
	copy(out, f.captured)
	return out
}

// newFakeAPI spins up a server that handles three routes:
//
//	GET  /v1/datasets/{id}                  → dataset metadata
//	GET  /v1/datasets/{id}/download         → DownloadURLInfo (download_url
//	                                           pointed at this same server)
//	GET  /s3-mock/{id}                      → fake signed-URL body
//	POST /v1/datasets/{id}/download-events  → outcome callback (captured)
func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:        t,
		s3Status: http.StatusOK,
		s3Body:   []byte("hello world"), // 11 bytes default
		dataset: map[string]any{
			"_id":      "ds-1",
			"name":     "Test Dataset",
			"metadata": map[string]any{
				"compression_enabled": false,
				"encryption_enabled":  false,
			},
		},
	}
	f.urlInfo = func() *DownloadURLInfo {
		return &DownloadURLInfo{
			DownloadURL: f.server.URL + "/s3-mock/ds-1",
			ExpiresAt:   "2026-05-04T23:00:00Z",
			FileName:    "data.csv.gz",
			FileSize:    1024,
			EventID:     "evt-test-1",
		}
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// handle is the per-server router. It captures each call so tests can
// assert downstream behavior without relying on global mocks.
func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	// Record the call.
	var payload map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &payload)
	}
	// Use EscapedPath so a %2F in the dataset_id remains visible to the
	// assertion (URL.Path would decode it back to "/").
	f.mu.Lock()
	f.captured = append(f.captured, capturedCall{
		Method:  r.Method,
		Path:    r.URL.EscapedPath(),
		Payload: payload,
	})
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/s3-mock/"):
		if f.s3Err {
			// Hijack and close so the consumer's http.Get sees a transport
			// error (matches the TS axios "ECONNRESET" path).
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijacker", http.StatusInternalServerError)
				return
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(f.s3Status)
		_, _ = w.Write(f.s3Body)

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/download"):
		info := f.urlInfo()
		if info == nil {
			// Simulate older API — no event_id field.
			info = &DownloadURLInfo{
				DownloadURL: f.server.URL + "/s3-mock/ds-1",
				ExpiresAt:   "2026-05-04T23:00:00Z",
			}
		}
		writeJSON(w, info)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/download-events"):
		if f.callbackErr {
			http.Error(w, "callback API down", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"event_id": payload["event_id"],
			"status":   payload["status"],
		})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/datasets/"):
		if f.datasetErr {
			http.Error(w, "metadata server boom", http.StatusInternalServerError)
			return
		}
		writeJSON(w, f.dataset)

	default:
		http.Error(w, "unexpected route: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// newTestConsumer builds a *Consumer wired to the given API endpoint
// without calling NewConsumer (which would hit STS). Same-package access
// to unexported fields is intentional — there's no other clean way to
// inject a test endpoint while keeping NewConsumer's STS validation in
// production.
func newTestConsumer(endpoint string) *Consumer {
	awsCfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
	}
	return &Consumer{
		APIEndpoint: endpoint,
		CustomerID:  "test-customer",
		Region:      "us-east-1",
		awsConfig:   awsCfg,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// waitForCallback polls the captured calls until at least one POST to a
// /download-events path appears (or the deadline elapses). The outcome
// callback is fire-and-forget in a goroutine, so tests must wait for it
// before asserting the captured payload. 2s is generous for a localhost
// httptest round-trip.
func waitForCallback(f *fakeAPI, want int, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		count := 0
		for _, c := range f.takeCaptured() {
			if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/download-events") {
				count++
			}
		}
		if count >= want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// callbackPayload returns the body of the first /download-events POST
// captured by the fake API (or nil if none has fired yet).
func callbackPayload(f *fakeAPI) map[string]any {
	for _, c := range f.takeCaptured() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/download-events") {
			return c.Payload
		}
	}
	return nil
}

// ============================================================
// Happy path — success outcome callback
// ============================================================

// TestDownloadOutcome_SuccessCallback pins the success-path payload:
// status, bytes_downloaded, sdk_version, sdk_language, duration_ms.
func TestDownloadOutcome_SuccessCallback(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	if err := c.DownloadDataset(context.Background(), "ds-1", out); err != nil {
		t.Fatalf("DownloadDataset failed: %v", err)
	}

	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("outcome callback never fired")
	}

	calls := f.takeCaptured()
	var post *capturedCall
	for i := range calls {
		if calls[i].Method == http.MethodPost && strings.HasSuffix(calls[i].Path, "/download-events") {
			post = &calls[i]
			break
		}
	}
	if post == nil {
		t.Fatal("no callback POST captured")
	}

	if post.Path != "/v1/datasets/ds-1/download-events" {
		t.Errorf("callback path = %q, want /v1/datasets/ds-1/download-events", post.Path)
	}

	p := post.Payload
	if p["event_id"] != "evt-test-1" {
		t.Errorf("event_id = %v, want evt-test-1", p["event_id"])
	}
	if p["status"] != "success" {
		t.Errorf("status = %v, want success", p["status"])
	}
	if got, ok := p["bytes_downloaded"].(float64); !ok || got != 11 {
		t.Errorf("bytes_downloaded = %v, want 11", p["bytes_downloaded"])
	}
	if p["sdk_language"] != "go" {
		t.Errorf("sdk_language = %v, want go", p["sdk_language"])
	}
	if v, _ := p["sdk_version"].(string); v == "" {
		t.Error("sdk_version is empty")
	}
	if _, ok := p["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms = %v, want number", p["duration_ms"])
	}
	if _, present := p["error_category"]; present {
		t.Errorf("error_category should be absent on success, got %v", p["error_category"])
	}
	if _, present := p["error_message"]; present {
		t.Errorf("error_message should be absent on success, got %v", p["error_message"])
	}
}

// TestDownloadOutcome_DatasetIDEncoded verifies special-character
// dataset IDs are URL-encoded in the callback path.
func TestDownloadOutcome_DatasetIDEncoded(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	if err := c.DownloadDataset(context.Background(), "company-rb-ds-foo/bar", out); err != nil {
		t.Fatalf("DownloadDataset failed: %v", err)
	}

	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("outcome callback never fired")
	}

	for _, c := range f.takeCaptured() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/download-events") {
			want := "/v1/datasets/company-rb-ds-foo%2Fbar/download-events"
			if c.Path != want {
				t.Errorf("callback path = %q, want %q", c.Path, want)
			}
			return
		}
	}
	t.Fatal("no callback POST captured")
}

// TestDownloadOutcome_SuccessZeroBytes pins the bug being fixed by
// dropping `omitempty` from BytesDownloaded: a legitimate 0-byte
// successful download (empty dataset, empty NDJSON file, decompressed-
// to-empty payload) must record bytes_downloaded=0 on the wire so the
// producer dashboard can distinguish "0 bytes downloaded" from "no SDK
// telemetry received yet". With omitempty in place, this test would
// fail because the field would be stripped from the JSON payload.
//
// Mirrors the rationale of the parallel-session DurationMs flake fix
// (commit 45b765d) for the bytes_downloaded field.
func TestDownloadOutcome_SuccessZeroBytes(t *testing.T) {
	f := newFakeAPI(t)
	f.s3Body = []byte{} // legal empty dataset — 0 bytes transferred
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	if err := c.DownloadDataset(context.Background(), "ds-1", out); err != nil {
		t.Fatalf("DownloadDataset failed on legal 0-byte payload: %v", err)
	}

	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("outcome callback never fired")
	}

	p := callbackPayload(f)
	if p["status"] != "success" {
		t.Errorf("status = %v, want success", p["status"])
	}
	// THE assertion this test exists for: bytes_downloaded must be
	// present in the payload as the literal number 0, not absent. Using
	// the two-value type assertion lets us tell apart "missing key"
	// (ok=false) from "key=0" (ok=true, got=0).
	got, ok := p["bytes_downloaded"].(float64)
	if !ok {
		t.Fatalf("bytes_downloaded missing from payload — omitempty regression? payload=%v", p)
	}
	if got != 0 {
		t.Errorf("bytes_downloaded = %v, want 0 (legal empty-payload success)", got)
	}
	// duration_ms must also be present (DurationMs flake fix is what
	// inspired this one — keep both invariants locked in this test).
	if _, ok := p["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing — DurationMs omitempty regression?")
	}
}

// ============================================================
// No event_id — callback skipped
// ============================================================

// TestDownloadOutcome_NoEventID_NoCallback ensures the SDK no-ops the
// callback when an older API doesn't surface event_id.
func TestDownloadOutcome_NoEventID_NoCallback(t *testing.T) {
	f := newFakeAPI(t)
	f.urlInfo = func() *DownloadURLInfo { return nil } // no event_id
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	if err := c.DownloadDataset(context.Background(), "ds-1", out); err != nil {
		t.Fatalf("DownloadDataset failed: %v", err)
	}

	// Give any rogue goroutine plenty of time to fire — we expect none.
	time.Sleep(200 * time.Millisecond)

	for _, c := range f.takeCaptured() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/download-events") {
			t.Fatalf("callback fired but should not have: %+v", c)
		}
	}
}

// ============================================================
// Error paths — error_category per phase
// ============================================================

// TestDownloadOutcome_NetworkFetchError verifies that a transport-level
// failure during the S3 fetch tags the callback with
// error_category=network_fetch.
func TestDownloadOutcome_NetworkFetchError(t *testing.T) {
	f := newFakeAPI(t)
	f.s3Err = true
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	if err == nil {
		t.Fatal("expected DownloadDataset to fail on network error")
	}

	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("outcome callback never fired")
	}

	p := callbackPayload(f)
	if p["status"] != "error" {
		t.Errorf("status = %v, want error", p["status"])
	}
	if p["error_category"] != "network_fetch" {
		t.Errorf("error_category = %v, want network_fetch", p["error_category"])
	}
	if _, ok := p["error_message"].(string); !ok {
		t.Errorf("error_message missing or wrong type: %v", p["error_message"])
	}
	if _, ok := p["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing")
	}
	// bytes_downloaded is now serialized unconditionally — see
	// RecordOutcomeRequest doc comment. On network_fetch failure the
	// pipeline returns before any bytes are read, so the zero value
	// reaches the wire as bytes_downloaded=0 (NOT absent). The schema's
	// "persisted only when non-zero" rule applies server-side, not on
	// the wire.
	if got, ok := p["bytes_downloaded"].(float64); !ok || got != 0 {
		t.Errorf("bytes_downloaded = %v, want 0 (omitempty was dropped to fix legal 0-byte success races)", p["bytes_downloaded"])
	}
}

// TestDownloadOutcome_DiskWriteError verifies that a disk-write failure
// tags the callback with error_category=disk_write. We force the
// failure by pointing outputPath at an unwritable directory (a path
// with a NUL byte is portable across OSes — os.WriteFile rejects it).
func TestDownloadOutcome_DiskWriteError(t *testing.T) {
	f := newFakeAPI(t)
	c := newTestConsumer(f.server.URL)

	// A path under a non-existent directory triggers os.WriteFile's
	// "no such file or directory" error — exercises ErrorCategoryDiskWrite.
	out := filepath.Join(t.TempDir(), "nonexistent-dir", "out.bin")
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	if err == nil {
		t.Fatal("expected DownloadDataset to fail on disk write")
	}

	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("outcome callback never fired")
	}

	p := callbackPayload(f)
	if p["error_category"] != "disk_write" {
		t.Errorf("error_category = %v, want disk_write", p["error_category"])
	}
	if msg, _ := p["error_message"].(string); msg == "" {
		t.Error("error_message empty on disk_write failure")
	}
}

// TestDownloadOutcome_MetadataFetchError_NoCallback verifies that a
// metadata-fetch failure (which happens BEFORE the signed-url fetch)
// does not fire a callback, since no event_id has been captured yet.
func TestDownloadOutcome_MetadataFetchError_NoCallback(t *testing.T) {
	f := newFakeAPI(t)
	f.datasetErr = true // fail the GET /v1/datasets/:id call
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	if err == nil {
		t.Fatal("expected DownloadDataset to fail")
	}
	if !strings.Contains(err.Error(), "metadata server boom") &&
		!strings.Contains(err.Error(), "failed to get dataset metadata") {
		t.Errorf("expected metadata-server error, got: %v", err)
	}

	// No callback row to update — the server-side recorder never ran
	// because the consumer never asked for a download URL.
	time.Sleep(200 * time.Millisecond)
	for _, c := range f.takeCaptured() {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/download-events") {
			t.Fatalf("callback fired but should not have: %+v", c)
		}
	}
}

// ============================================================
// Privacy: filesystem paths scrubbed from error_message
// ============================================================

// TestSanitize_StripsUsersPath asserts /Users/<name>/ paths are
// redacted in error_message before sending. Direct unit test on
// sanitizeErrorMessage so we don't have to fake an OS-specific file
// permission error.
func TestSanitize_StripsUsersPath(t *testing.T) {
	in := "EACCES: permission denied, open '/Users/alice/secret/data.bin'"
	got := sanitizeErrorMessage(in)
	if !strings.Contains(got, "/Users/<redacted>") {
		t.Errorf("expected /Users/<redacted> in %q", got)
	}
	if strings.Contains(got, "/Users/alice") {
		t.Errorf("did not strip /Users/alice from %q", got)
	}
}

// TestSanitize_StripsHomePath asserts the Linux equivalent of the
// /Users/ scrub.
func TestSanitize_StripsHomePath(t *testing.T) {
	in := "EACCES at /home/bob/private/file"
	got := sanitizeErrorMessage(in)
	if !strings.Contains(got, "/home/<redacted>") {
		t.Errorf("expected /home/<redacted> in %q", got)
	}
	if strings.Contains(got, "/home/bob") {
		t.Errorf("did not strip /home/bob from %q", got)
	}
}

// TestSanitize_CapsAt500Chars ensures error_message is truncated before
// it ever reaches the wire (server-side cap is also 500).
func TestSanitize_CapsAt500Chars(t *testing.T) {
	huge := strings.Repeat("X", 5000)
	got := sanitizeErrorMessage(huge)
	if len(got) != errorMessageMaxChars {
		t.Errorf("len(sanitized) = %d, want %d", len(got), errorMessageMaxChars)
	}
}

// ============================================================
// Best-effort guarantee — callback failure swallowed
// ============================================================

// TestDownloadOutcome_CallbackFailure_NotPropagatedOnSuccess pins the
// best-effort contract: even if the callback POST returns 500, the
// download itself returns nil.
func TestDownloadOutcome_CallbackFailure_NotPropagatedOnSuccess(t *testing.T) {
	f := newFakeAPI(t)
	f.callbackErr = true
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	if err != nil {
		t.Fatalf("DownloadDataset returned %v; callback failure must NOT propagate", err)
	}

	// The callback was attempted, just rejected.
	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("expected the callback POST to be attempted")
	}

	// Verify the file was actually written (download path completed).
	if _, statErr := os.Stat(out); statErr != nil {
		t.Errorf("download output missing: %v", statErr)
	}
}

// TestDownloadOutcome_CallbackFailure_OriginalErrorPropagates verifies
// that when both the download AND the callback fail, the caller sees
// the download error (not the callback error) — a debugging-quality
// guarantee.
func TestDownloadOutcome_CallbackFailure_OriginalErrorPropagates(t *testing.T) {
	f := newFakeAPI(t)
	f.s3Err = true       // download fails (network_fetch)
	f.callbackErr = true // callback also fails
	c := newTestConsumer(f.server.URL)

	out := filepath.Join(t.TempDir(), "out.bin")
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	if err == nil {
		t.Fatal("expected DownloadDataset to fail")
	}
	// Caller sees the network error, not "callback API down".
	if strings.Contains(err.Error(), "callback") {
		t.Errorf("error leaked callback failure to caller: %v", err)
	}

	// The callback was attempted at least once.
	if !waitForCallback(f, 1, 2*time.Second) {
		t.Fatal("expected the callback POST to be attempted")
	}
}

// ----------------------------------------------------------------------
// Sanity check: the generated download URL contains the test server's
// host. This isn't part of the 11 cases but pins the test fixture so a
// future regression in fakeAPI surfaces with a clear failure.
func TestFakeAPI_GeneratesLocalDownloadURL(t *testing.T) {
	f := newFakeAPI(t)
	info := f.urlInfo()
	if !strings.HasPrefix(info.DownloadURL, f.server.URL) {
		t.Fatalf("download_url must point at fake server, got %s", info.DownloadURL)
	}
}

// Compile-time check that ErrorCategory values match the JSON Schema
// enum spelling exactly. A typo here would be caught at the API boundary
// but it's cheaper to fail fast in test.
var _ = func() bool {
	want := map[ErrorCategory]string{
		ErrorCategorySignedURLFetch: "signed_url_fetch",
		ErrorCategoryMetadataFetch:  "metadata_fetch",
		ErrorCategoryNetworkFetch:   "network_fetch",
		ErrorCategoryKMSDecrypt:     "kms_decrypt",
		ErrorCategoryDecompress:     "decompress",
		ErrorCategoryDiskWrite:      "disk_write",
		ErrorCategoryUnknown:        "unknown",
	}
	for k, v := range want {
		if string(k) != v {
			panic(fmt.Sprintf("ErrorCategory %q != %q", k, v))
		}
	}
	return true
}()
