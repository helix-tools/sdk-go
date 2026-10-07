package producer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helix-tools/sdk-go/v2/types"
)

// producerConfigAPI is a fake Helix API for the whole producer path: it serves
// GET /v1/self/producer-config with a scripted answer, then the dataset
// create (POST), the presigned PUT and the follow-up GET UploadDataset makes.
// It records every call so a test can count them.
type producerConfigAPI struct {
	mu sync.Mutex

	configStatus int
	configBody   string
	configHang   bool
	configCut    bool
	configDelay  time.Duration

	configCalls []*http.Request
	postBodies  []map[string]any
	puts        int

	inFlight    int32
	maxInFlight int32

	release     chan struct{}
	releaseOnce sync.Once

	server *httptest.Server
}

func newProducerConfigAPI(t *testing.T, status int, body string) *producerConfigAPI {
	t.Helper()
	a := &producerConfigAPI{configStatus: status, configBody: body, release: make(chan struct{})}
	release := a.release

	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == producerConfigPath:
			inFlight := atomic.AddInt32(&a.inFlight, 1)
			defer atomic.AddInt32(&a.inFlight, -1)
			for {
				max := atomic.LoadInt32(&a.maxInFlight)
				if inFlight <= max || atomic.CompareAndSwapInt32(&a.maxInFlight, max, inFlight) {
					break
				}
			}

			a.mu.Lock()
			a.configCalls = append(a.configCalls, r.Clone(context.Background()))
			status, body, hang, cut, delay := a.configStatus, a.configBody, a.configHang, a.configCut, a.configDelay
			a.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if hang {
				select {
				case <-r.Context().Done():
					return
				case <-release:
					// Released deliberately (releaseHang): fall through and
					// serve the (possibly since-updated) configured response,
					// instead of leaving the caller with an empty one.
				}
			}
			if cut {
				// Send a complete, valid object but promise more bytes than
				// that, then drop the connection: only the read error can
				// refuse it.
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"encryption_key_id":"key-cut"}`))
				w.(http.Flusher).Flush()
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			if status == http.StatusFound {
				http.Redirect(w, r, "/redirected-config", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case r.Method == http.MethodGet && r.URL.Path == "/redirected-config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"encryption_key_id":"key-behind-a-redirect"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/datasets":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			a.mu.Lock()
			a.postBodies = append(a.postBodies, body)
			a.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"` + a.server.URL + `/put","s3_key":"datasets/x/data.ndjson.gz"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/put":
			a.mu.Lock()
			a.puts++
			a.mu.Unlock()
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/datasets/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ds-1","name":"n"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(func() {
		a.releaseHang()
		a.server.Close()
	})
	return a
}

// releaseHang unblocks every producer-config request currently waiting on
// configHang, and every one that starts waiting after this call — giving a
// test deterministic control over when a stalled lookup is allowed to
// finish, instead of racing a fixed delay against it. Safe to call more than
// once (idempotent) and already called by this fixture's own cleanup, so a
// test may also call it early without double-closing the channel.
func (a *producerConfigAPI) releaseHang() {
	a.releaseOnce.Do(func() { close(a.release) })
}

func (a *producerConfigAPI) configRequest(i int) *http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configCalls[i]
}

func (a *producerConfigAPI) postBody(i int) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.postBodies[i]
}

func (a *producerConfigAPI) counts() (configCalls, posts, puts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.configCalls), len(a.postBodies), a.puts
}

// setConfigResponse changes what producer-config answers from this point on
// — e.g. simulating an outage ending — safe to call concurrently with
// in-flight requests.
func (a *producerConfigAPI) setConfigResponse(status int, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.configStatus, a.configBody = status, body
}

// maxConcurrentConfigCalls returns the highest number of producer-config
// requests this server ever had in flight at the same instant.
func (a *producerConfigAPI) maxConcurrentConfigCalls() int32 {
	return atomic.LoadInt32(&a.maxInFlight)
}

// keyServiceRecorder is a fake key service that records the KeyId of every
// Encrypt call and answers with a fixed ciphertext blob.
type keyServiceRecorder struct {
	mu     sync.Mutex
	keyIDs []string
}

func newKeyServiceRecorder(t *testing.T) (*keyServiceRecorder, *httptest.Server) {
	t.Helper()
	k := &keyServiceRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			KeyID string `json:"KeyId"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		k.mu.Lock()
		k.keyIDs = append(k.keyIDs, req.KeyID)
		k.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"CiphertextBlob":"` + fakeKMSCiphertextBlobB64 + `","KeyId":"` + req.KeyID + `"}`))
	}))
	t.Cleanup(srv.Close)
	return k, srv
}

func (k *keyServiceRecorder) calls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keyIDs...)
}

// fakeIdentityServer answers the credential check NewProducer makes before
// anything else.
func fakeIdentityServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
			`<Arn>arn:aws:iam::123456789012:user/test</Arn><UserId>AIDTEST</UserId><Account>123456789012</Account>` +
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>r-1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newProducerThroughConstructor builds a Producer with the real NewProducer,
// with every AWS endpoint pointed at a local fake and no shared AWS config
// read from the machine running the test. It returns the Producer and
// everything NewProducer printed.
func newProducerThroughConstructor(t *testing.T, apiURL, keyServiceURL string) (*Producer, string) {
	t.Helper()

	isolateAWSEnv(t, fakeIdentityServer(t).URL, keyServiceURL)

	var p *Producer
	out := captureStdout(t, func() {
		var err error
		p, err = NewProducer(testProducerConfig(apiURL))
		if err != nil {
			t.Fatalf("NewProducer: %v", err)
		}
	})
	return p, out
}

// testAccessKeyID is a placeholder access key id; it is not a real key.
const testAccessKeyID = "AKIDTESTPRODUCER"

func testProducerConfig(apiURL string) types.Config {
	return types.Config{
		APIEndpoint:        apiURL,
		AWSAccessKeyID:     testAccessKeyID,
		AWSSecretAccessKey: "fake-secret",
		CustomerID:         "cust-1",
		Region:             "us-east-1",
	}
}

// isolateAWSEnv points the identity and key services at local fakes and keeps
// any AWS configuration on the machine running the test out of the picture.
func isolateAWSEnv(t *testing.T, identityURL, keyServiceURL string) {
	t.Helper()

	empty := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("AWS_ENDPOINT_URL_STS", identityURL)
	t.Setenv("AWS_ENDPOINT_URL_KMS", keyServiceURL)
}

// captureStdout runs fn and returns what it printed to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	closed := false
	defer func() {
		os.Stdout = orig
		if !closed {
			_ = w.Close()
		}
	}()

	fn()

	_ = w.Close()
	closed = true
	os.Stdout = orig
	return <-done
}

// TestNewProducer_UsesTheAPIEncryptionKey is acceptance question G1, driven
// through the real NewProducer and the real UploadDataset: construction makes
// exactly one signed GET /v1/self/producer-config, its encryption_key_id is the
// KeyId of the Encrypt call, and the dataset create carries no bucket.
func TestNewProducer_UsesTheAPIEncryptionKey(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusOK, `{"encryption_key_id":"key-from-the-api"}`)
	keys, keyService := newKeyServiceRecorder(t)

	p, out := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

	if strings.Contains(out, "Warning") {
		t.Errorf("NewProducer printed a warning on a healthy configuration: %q", out)
	}
	if p.KMSKeyID != "key-from-the-api" {
		t.Fatalf("KMSKeyID = %q, want the value the API returned", p.KMSKeyID)
	}
	if p.BucketName != "" {
		t.Errorf("BucketName = %q, want empty: the platform owns the destination", p.BucketName)
	}

	configCalls, _, _ := api.counts()
	if configCalls != 1 {
		t.Fatalf("NewProducer made %d producer-config request(s), want exactly 1", configCalls)
	}
	auth := api.configRequest(0).Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "Credential="+testAccessKeyID+"/") || !strings.Contains(auth, "/execute-api/aws4_request") {
		t.Errorf("producer-config request is not signed with the caller's credentials: Authorization=%q", auth)
	}

	if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("api-key-upload")); err != nil {
		t.Fatalf("UploadDataset: %v", err)
	}

	if got := keys.calls(); len(got) != 1 || got[0] != "key-from-the-api" {
		t.Fatalf("Encrypt KeyId(s) = %q, want exactly [key-from-the-api]", got)
	}
	configCalls, posts, puts := api.counts()
	if configCalls != 1 {
		t.Errorf("producer-config requested %d times after an upload, want once (at construction)", configCalls)
	}
	if posts != 1 || puts != 1 {
		t.Fatalf("posts=%d puts=%d, want one record and one upload", posts, puts)
	}
	for _, key := range []string{"s3_bucket_name", "s3_bucket"} {
		if v, present := api.postBody(0)[key]; present {
			t.Errorf("dataset create carries %s=%v; it must not send a bucket", key, v)
		}
	}
}

// TestNewProducer_UnresolvedKeyFailsUploadsClosed is acceptance question G2:
// whatever goes wrong with the producer-config call, the Producer is still
// built, a warning is printed, and the first upload fails before a record is
// created, before the key service is called and before any byte is PUT.
//
// A definitive case (the API cleanly answered "no key configured": a 404)
// fails with today's fixed message and is never retried: UploadDataset's
// own ensureEncryptionKeyID call sees the cached definitive answer and
// makes no further producer-config request. A non-definitive case (the API
// gave no usable answer: a transport failure, a timeout, a non-200 other
// than 404, a malformed/oversized body, a redirect, or a clean 200 whose
// key is missing, empty, or blank) is retried by that same call — the fake
// server has not changed, so it fails again, but with the SDK's existing
// "could not be resolved" wording rather than the fixed message, since
// this failure is not cached and a later upload could still succeed.
func TestNewProducer_UnresolvedKeyFailsUploadsClosed(t *testing.T) {
	// A server error body that names an internal location: it must reach
	// neither the warning nor the upload error.
	internalDetail := strings.Join([]string{"", "internal", "customers", "cust-1", "key"}, "/")

	cases := []struct {
		name       string
		status     int
		body       string
		hang       bool
		cut        bool
		definitive bool
	}{
		{name: "404 no key configured", status: http.StatusNotFound, body: `{"error":"not found"}`, definitive: true},
		{name: "403 consumer-only caller", status: http.StatusForbidden, body: `{"error":"forbidden ` + internalDetail + `"}`},
		{name: "401 unauthenticated", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
		{name: "500 server error", status: http.StatusInternalServerError, body: `{"error":"boom ` + internalDetail + `"}`},
		{name: "200 garbage body", status: http.StatusOK, body: `<html>not json</html>`},
		{name: "200 wrong type", status: http.StatusOK, body: `{"encryption_key_id":42}`},
		{name: "200 missing field", status: http.StatusOK, body: `{}`},
		{name: "200 empty value", status: http.StatusOK, body: `{"encryption_key_id":""}`},
		{name: "200 blank value", status: http.StatusOK, body: `{"encryption_key_id":"   "}`},
		{name: "200 valid object then trailing data", status: http.StatusOK, body: `{"encryption_key_id":"key-prefix"}{"garbage":true}`},
		{name: "200 valid object, padding past the read cap, then trailing data", status: http.StatusOK,
			body: `{"encryption_key_id":"key-padded"}` + strings.Repeat(" ", maxProducerConfigBytes) + `{"garbage":true}`},
		{name: "200 valid object padded to one byte over the cap", status: http.StatusOK,
			body: `{"encryption_key_id":"key-oversized"}` + strings.Repeat(" ", maxProducerConfigBytes+1-len(`{"encryption_key_id":"key-oversized"}`))},
		{name: "201 instead of 200", status: http.StatusCreated, body: `{"encryption_key_id":"key-201"}`},
		{name: "302 redirect to a key", status: http.StatusFound},
		{name: "no answer in time", hang: true},
		{name: "connection dropped mid-body", cut: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newProducerConfigAPI(t, tc.status, tc.body)
			api.configHang, api.configCut = tc.hang, tc.cut
			if tc.hang {
				orig := producerConfigTimeout
				producerConfigTimeout = 200 * time.Millisecond
				t.Cleanup(func() { producerConfigTimeout = orig })
			}
			keys, keyService := newKeyServiceRecorder(t)

			p, out := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

			if p == nil {
				t.Fatal("NewProducer returned no Producer; it must still be built")
			}
			if p.KMSKeyID != "" {
				t.Fatalf("KMSKeyID = %q, want empty when the key could not be resolved", p.KMSKeyID)
			}
			if !strings.Contains(out, "Warning: "+errEncryptionKeyUnresolved) {
				t.Errorf("NewProducer output = %q, want the unresolved-key warning", out)
			}
			if strings.Contains(out, internalDetail) {
				t.Errorf("warning leaks the server's error body: %q", out)
			}

			_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("no-key"))
			if err == nil {
				t.Fatal("UploadDataset error = nil, want an error")
			}
			if tc.definitive {
				if !strings.Contains(err.Error(), "no encryption key configured") {
					t.Fatalf("UploadDataset error = %v, want the missing-encryption-key error (definitive, cached)", err)
				}
			} else if err.Error() != errEncryptionKeyUnresolved {
				t.Fatalf("UploadDataset error = %q, want exactly %q (non-definitive, retried)", err.Error(), errEncryptionKeyUnresolved)
			}
			if strings.Contains(err.Error(), internalDetail) {
				t.Errorf("upload error leaks the server's error body: %q", err)
			}

			configCalls, posts, puts := api.counts()
			wantConfigCalls := 1
			if !tc.definitive {
				// Non-definitive: UploadDataset's own ensureEncryptionKeyID
				// call retries the still-failing lookup once more.
				wantConfigCalls = 2
			}
			if configCalls != wantConfigCalls {
				t.Errorf("producer-config called %d time(s), want %d", configCalls, wantConfigCalls)
			}
			if posts != 0 || puts != 0 {
				t.Errorf("posts=%d puts=%d after an unresolved key, want 0/0: nothing may be created or uploaded", posts, puts)
			}
			if got := keys.calls(); len(got) != 0 {
				t.Errorf("key service called %d time(s) with KeyId(s) %q, want none", len(got), got)
			}
		})
	}
}

// TestUploadDataset_RetriesKeyLookupAfterTransientFailure is the reproducing
// test for the parity fix: a construction-time key lookup that gets no
// response (here, a 500) must not block every later upload forever. The
// outage ends before the first upload is attempted; that upload's own
// ensureEncryptionKeyID call retries the lookup, gets served the key, and
// the upload succeeds — exactly two producer-config calls total (one at
// construction, one the upload triggered), never the three or more a
// sloppier retry (e.g. one retry per call site) could cause.
//
// Negative control: on main before this fix, NewProducer resolves the key
// exactly once and UploadDataset never calls resolveEncryptionKeyID again,
// so this test fails main with "UploadDataset after recovery" returning the
// permanent 'no encryption key configured' error.
func TestUploadDataset_RetriesKeyLookupAfterTransientFailure(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusInternalServerError, `{"error":"boom"}`)
	keys, keyService := newKeyServiceRecorder(t)

	p, out := newProducerThroughConstructor(t, api.server.URL, keyService.URL)
	if !strings.Contains(out, "Warning: "+errEncryptionKeyUnresolved) {
		t.Fatalf("construction output = %q, want the unresolved-key warning", out)
	}
	if p.KMSKeyID != "" {
		t.Fatalf("KMSKeyID = %q, want empty after a failed construction-time lookup", p.KMSKeyID)
	}

	// The outage ends before the first upload: the API now serves the key.
	api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-after-recovery"}`)

	if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("after-recovery")); err != nil {
		t.Fatalf("UploadDataset after recovery: %v", err)
	}

	if p.KMSKeyID != "key-after-recovery" {
		t.Fatalf("KMSKeyID = %q, want key-after-recovery", p.KMSKeyID)
	}
	if got := keys.calls(); len(got) != 1 || got[0] != "key-after-recovery" {
		t.Fatalf("Encrypt KeyId(s) = %q, want exactly [key-after-recovery]", got)
	}

	configCalls, posts, puts := api.counts()
	if configCalls != 2 {
		t.Fatalf("producer-config called %d time(s), want exactly 2 (construction + the retry this upload triggered)", configCalls)
	}
	if posts != 1 || puts != 1 {
		t.Fatalf("posts=%d puts=%d, want exactly one dataset created and uploaded", posts, puts)
	}

	// A second upload must not call producer-config again: the resolved key
	// is cached.
	if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("after-recovery-2")); err != nil {
		t.Fatalf("UploadDataset (second, cached key): %v", err)
	}
	if configCalls, _, _ := api.counts(); configCalls != 2 {
		t.Fatalf("producer-config called %d time(s) after a second upload, want still 2: the key is cached", configCalls)
	}
}

// TestUploadDataset_RecoversAfterMissingEmptyOrBlankKeyAnswer is self-attack
// (c): a clean 200 whose encryption_key_id is missing, empty, or blank is
// NOT the API's definitive "no key configured" answer — only a 404 is (see
// TestUploadDataset_404AnswerIsNotRetried) — because the account's key may
// simply not be provisioned yet. It must behave like any other
// non-definitive failure: retried on the next upload. Once the API starts
// answering with a real key, that upload succeeds with exactly one more
// producer-config call, and the resolved key is then cached as usual.
//
// Negative control: before this fix, resolveEncryptionKeyID's empty-key
// branch returned a plain, unmarked error, so runKeyLookup's
// `definitive := lookupErr != nil && !errors.Is(lookupErr, errKeyLookupNoResponse)`
// classified it as definitive and cached it forever. Against that code this
// test fails: "UploadDataset after recovery" returns the permanent 'no
// encryption key configured' error instead of succeeding, because the
// cached definitive answer short-circuits ensureEncryptionKeyID before it
// ever calls producer-config again.
func TestUploadDataset_RecoversAfterMissingEmptyOrBlankKeyAnswer(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "missing field", body: `{}`},
		{name: "empty value", body: `{"encryption_key_id":""}`},
		{name: "blank value", body: `{"encryption_key_id":"   "}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newProducerConfigAPI(t, http.StatusOK, tc.body)
			keys, keyService := newKeyServiceRecorder(t)

			p, out := newProducerThroughConstructor(t, api.server.URL, keyService.URL)
			if !strings.Contains(out, "Warning: "+errEncryptionKeyUnresolved) {
				t.Fatalf("construction output = %q, want the unresolved-key warning", out)
			}
			if p.KMSKeyID != "" {
				t.Fatalf("KMSKeyID = %q, want empty after a construction-time lookup with no key yet", p.KMSKeyID)
			}

			// The account's key is now provisioned.
			api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-after-recovery"}`)

			if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("after-recovery")); err != nil {
				t.Fatalf("UploadDataset after recovery: %v", err)
			}
			if p.KMSKeyID != "key-after-recovery" {
				t.Fatalf("KMSKeyID = %q, want key-after-recovery", p.KMSKeyID)
			}
			if got := keys.calls(); len(got) != 1 || got[0] != "key-after-recovery" {
				t.Fatalf("Encrypt KeyId(s) = %q, want exactly [key-after-recovery]", got)
			}

			configCalls, posts, puts := api.counts()
			if configCalls != 2 {
				t.Fatalf("producer-config called %d time(s), want exactly 2 (construction + the retry this upload triggered)", configCalls)
			}
			if posts != 1 || puts != 1 {
				t.Fatalf("posts=%d puts=%d, want exactly one dataset created and uploaded", posts, puts)
			}

			// A second upload must not call producer-config again: the
			// resolved key is cached.
			if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("after-recovery-2")); err != nil {
				t.Fatalf("UploadDataset (second, cached key): %v", err)
			}
			if configCalls, _, _ := api.counts(); configCalls != 2 {
				t.Fatalf("producer-config called %d time(s) after a second upload, want still 2: the key is cached", configCalls)
			}
		})
	}
}

// TestUploadDataset_404AnswerIsNotRetried is the self-attack for the 404
// fix: a 404 is the API's definitive "no key configured" answer — the only
// one, see TestUploadDataset_RecoversAfterMissingEmptyOrBlankKeyAnswer —
// cached at construction and never looked up again, however many uploads
// follow.
func TestUploadDataset_404AnswerIsNotRetried(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusNotFound, `{"error":"not found"}`)
	_, keyService := newKeyServiceRecorder(t)
	p, _ := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

	for i, name := range []string{"no-key-1", "no-key-2"} {
		if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions(name)); err == nil {
			t.Fatalf("upload %d (%s): want the missing-encryption-key error, got nil", i, name)
		} else if !strings.Contains(err.Error(), "no encryption key configured") {
			t.Fatalf("upload %d (%s) error = %v, want the missing-encryption-key error", i, name, err)
		}
	}

	configCalls, _, _ := api.counts()
	if configCalls != 1 {
		t.Fatalf("producer-config called %d time(s), want exactly 1: a 404 \"no key configured\" answer must not be retried", configCalls)
	}
}

// TestUploadDataset_ConcurrentUploadsShareOneInFlightKeyLookup is self-attack
// (b) and (d): five uploads attempted concurrently while the key lookup is
// down must never send more than one producer-config request at a time (no
// thundering herd) and must make exactly one producer-config request for the
// whole wave, each failing upload's error must carry no raw detail about the
// fake API's host/URL or any key-shaped literal, and once the outage ends, a
// second, equally concurrent wave of uploads shares exactly one more lookup,
// every one of them succeeds, and the key service sees exactly one Encrypt
// call per successful upload.
func TestUploadDataset_ConcurrentUploadsShareOneInFlightKeyLookup(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusInternalServerError, `{"error":"boom"}`)
	api.configDelay = 50 * time.Millisecond // widen the window so true overlap would be caught
	keys, keyService := newKeyServiceRecorder(t)
	p, _ := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

	const n = 5
	runWave := func(opt string) []error {
		files := make([]string, n)
		for i := range files {
			files[i] = writeNDJSON(t, 3)
		}

		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = p.UploadDataset(context.Background(), files[i], testUploadOptions(opt))
			}(i)
		}
		close(start)
		wg.Wait()
		return errs
	}

	// Wave 1: the outage is still live.
	configCallsBeforeWave1, _, _ := api.counts() // 1: construction's own failed attempt.
	for i, err := range runWave("concurrent") {
		if err == nil {
			t.Errorf("upload %d: want an error while the key lookup is down, got nil", i)
			continue
		}
		if err.Error() != errEncryptionKeyUnresolved {
			t.Errorf("upload %d error = %q, want exactly %q", i, err.Error(), errEncryptionKeyUnresolved)
		}
		if strings.Contains(err.Error(), api.server.URL) {
			t.Errorf("upload %d error leaks the API host/URL: %q", i, err)
		}
		if strings.Contains(err.Error(), producerConfigPath) {
			t.Errorf("upload %d error leaks the request path: %q", i, err)
		}
	}

	if max := api.maxConcurrentConfigCalls(); max > 1 {
		t.Fatalf("observed %d concurrent producer-config calls, want at most 1 (single-flight)", max)
	}
	if got := keys.calls(); len(got) != 0 {
		t.Fatalf("key service called %d time(s) while the lookup was down, want none", len(got))
	}
	configCallsAfterWave1, _, _ := api.counts()
	if configCallsAfterWave1-configCallsBeforeWave1 != 1 {
		t.Fatalf("the outage wave of %d concurrent uploads made %d producer-config request(s), want exactly 1 shared lookup", n, configCallsAfterWave1-configCallsBeforeWave1)
	}

	// Wave 2: the outage ends, and this wave is just as concurrent as wave 1.
	api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-after-recovery"}`)

	for i, err := range runWave("post-recovery") {
		if err != nil {
			t.Errorf("post-recovery upload %d: %v", i, err)
		}
	}

	if max := api.maxConcurrentConfigCalls(); max > 1 {
		t.Fatalf("observed %d concurrent producer-config calls during recovery, want at most 1 (single-flight)", max)
	}
	configCallsAfterWave2, _, _ := api.counts()
	if configCallsAfterWave2-configCallsAfterWave1 != 1 {
		t.Fatalf("the recovery wave of %d concurrent uploads made %d producer-config request(s), want exactly 1 shared lookup", n, configCallsAfterWave2-configCallsAfterWave1)
	}
	if got := keys.calls(); len(got) != n {
		t.Fatalf("key service called %d time(s) after recovery, want exactly %d (one per successful upload)", len(got), n)
	}
}

// TestUploadDataset_CancelledWaiterSharesLiveWaitersOneInFlightLookup is the
// reproducing test for the context fix: a single shared lookup, stalled
// (blocked until the test releases it) serves a live waiter and a canceled
// waiter started CONCURRENTLY against it. The canceled one must return
// ctx.Err() promptly without waiting for the stub; releasing the stub must
// still let the live waiter succeed from that same one request — never a
// second producer-config call for this wave, and never poisoned by whichever
// of the two happened to be the one that actually started the lookup.
//
// Negative control: this specific shape (true concurrency, a released stub,
// asserting exactly one request for the wave) is new in this revision —
// round 2's version on head ac0d30d85d5540a95e7066d4f0a02cb749918272 ran the
// canceled caller to completion before even starting the live one, so it
// could not have caught a canceled-caller-poisons-the-lookup regression. Run
// against head's producer.go (context.Background() in place of
// context.WithoutCancel(ctx)) this test still passes — that substitution
// does not change which context the shared lookup runs on from any given
// waiter's point of view, only whether request-scoped values travel with
// it. See TestEnsureEncryptionKeyID_SharedLookupCarriesCallerContextValues
// for the test that is sensitive to exactly that difference, and fails
// against head.
func TestUploadDataset_CancelledWaiterSharesLiveWaitersOneInFlightLookup(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusInternalServerError, `{"error":"boom"}`)
	_, keyService := newKeyServiceRecorder(t)
	p, _ := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

	configCallsBefore, _, _ := api.counts() // 1: construction's own failed attempt.

	// The outage ends, but this lookup hangs until releaseHang is called —
	// a deterministic stand-in for "still running" that a test controls
	// directly instead of racing a fixed delay against it.
	api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-after-stall"}`)
	api.configHang = true

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before either waiter starts

	start := make(chan struct{})
	cancelledErrCh := make(chan error, 1)
	liveErrCh := make(chan error, 1)

	go func() {
		<-start
		_, err := p.UploadDataset(cancelledCtx, writeNDJSON(t, 3), testUploadOptions("cancelled"))
		cancelledErrCh <- err
	}()
	go func() {
		<-start
		_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("live"))
		liveErrCh <- err
	}()
	close(start) // both waiters race for the lock concurrently from here.

	cancelStart := time.Now()
	var cancelErr error
	select {
	case cancelErr = <-cancelledErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the canceled waiter did not return; want it to return promptly without waiting for the stalled lookup")
	}
	if cancelElapsed := time.Since(cancelStart); cancelElapsed >= 100*time.Millisecond {
		t.Fatalf("the canceled waiter took %v, want it to return promptly while the lookup is still stalled", cancelElapsed)
	}
	if !errors.Is(cancelErr, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", cancelErr)
	}

	// The live waiter must still be blocked on the same stalled lookup —
	// the canceled waiter's return must not have poisoned or finished it.
	select {
	case err := <-liveErrCh:
		t.Fatalf("live waiter returned (err=%v) before the stalled lookup was released", err)
	default:
	}

	api.releaseHang() // let the one shared lookup finish.

	select {
	case err := <-liveErrCh:
		if err != nil {
			t.Fatalf("live waiter: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live waiter did not complete after the shared lookup was released")
	}

	if p.KMSKeyID != "key-after-stall" {
		t.Fatalf("KMSKeyID = %q, want key-after-stall", p.KMSKeyID)
	}
	configCallsAfter, _, _ := api.counts()
	if configCallsAfter-configCallsBefore != 1 {
		t.Fatalf("producer-config called %d time(s) for this wave, want exactly 1 shared lookup serving both the canceled and the live waiter", configCallsAfter-configCallsBefore)
	}
}

// waitForConfigCallCount blocks until api has recorded at least want
// producer-config requests, or fails the test after 2s. Used instead of a
// fixed sleep so a test can deterministically wait for a request to actually
// reach the fake API — and, when api.configHang is true and not yet
// released, be stalled there — before it continues.
func waitForConfigCallCount(t *testing.T, api *producerConfigAPI, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if calls, _, _ := api.counts(); calls >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer-config was not called %d time(s) within 2s", want)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestUploadDataset_CancelledWhileAlreadyWaitingOnStalledLookup is round 3's
// requested test: both of round 2's cancellation tests
// (TestUploadDataset_CancelledWaiterSharesLiveWaitersOneInFlightLookup and
// TestUploadDataset_CancelledWaiterLeavesNoGoroutineLeak) cancel their
// context BEFORE calling UploadDataset, so an implementation that reads
// ctx.Err() once up front and then waits unconditionally on the shared
// lookup's done channel — never watching ctx again — would still pass both.
// This test starts upload A with a LIVE context, waits for the fake API to
// confirm the shared lookup has actually arrived and is stalled, starts
// upload B (also live) so it joins that same in-flight call, and only then
// cancels A's context: A's ensureEncryptionKeyID select must notice a
// cancellation that happens WHILE it is already parked waiting, not one in
// effect before the select ever ran. B must stay blocked on the untouched
// shared lookup and succeed once it is released, with exactly one
// producer-config request for the whole wave.
//
// Negative control: changing ensureEncryptionKeyID's select to check
// ctx.Err() once before the wait and then do a plain "<-call.done" (no
// select on ctx.Done() while parked) makes this test fail — upload A never
// returns within the 2s bound, because it is not watching ctx.Done() anymore
// once it starts waiting. Confirmed by hand and reverted; see REPORT.md.
func TestUploadDataset_CancelledWhileAlreadyWaitingOnStalledLookup(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusInternalServerError, `{"error":"boom"}`)
	_, keyService := newKeyServiceRecorder(t)
	p, _ := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

	configCallsBefore, _, _ := api.counts() // 1: construction's own failed attempt.

	// The outage ends, but this lookup hangs until releaseHang is called —
	// a deterministic stand-in for "still running" that the test controls
	// directly instead of racing a fixed delay against it.
	api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-after-stall"}`)
	api.configHang = true

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	aErrCh := make(chan error, 1)
	go func() {
		_, err := p.UploadDataset(ctxA, writeNDJSON(t, 3), testUploadOptions("a-live-then-cancelled"))
		aErrCh <- err
	}()

	// A's own call to ensureEncryptionKeyID creates the shared call and
	// starts its lookup goroutine before A ever reaches its select — that
	// goroutine's HTTP request reaching the fake server (confirmed here) can
	// only happen after A has already moved on to waiting. Only now is A
	// known to be parked on a confirmed-stalled lookup, so starting B and
	// canceling A from this point on exercises exactly the gap round 3
	// found: a cancellation that happens AFTER the wait has begun.
	waitForConfigCallCount(t, api, configCallsBefore+1)

	bErrCh := make(chan error, 1)
	go func() {
		_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), testUploadOptions("b-live"))
		bErrCh <- err
	}()

	cancelStart := time.Now()
	cancelA()

	var aErr error
	select {
	case aErr = <-aErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("upload A did not return after its context was canceled while already waiting on the stalled lookup")
	}
	if elapsed := time.Since(cancelStart); elapsed >= 200*time.Millisecond {
		t.Fatalf("upload A took %v to return after cancellation, want under 200ms", elapsed)
	}
	if !errors.Is(aErr, context.Canceled) {
		t.Fatalf("upload A error = %v, want context.Canceled", aErr)
	}

	// B must still be blocked: A's cancellation must not have poisoned or
	// finished the one shared lookup B joined.
	select {
	case err := <-bErrCh:
		t.Fatalf("upload B returned (err=%v) before the stalled lookup was released", err)
	default:
	}

	api.releaseHang() // let the one shared lookup finish.

	select {
	case err := <-bErrCh:
		if err != nil {
			t.Fatalf("upload B: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upload B did not complete after the shared lookup was released")
	}

	if p.KMSKeyID != "key-after-stall" {
		t.Fatalf("KMSKeyID = %q, want key-after-stall", p.KMSKeyID)
	}
	configCallsAfter, _, _ := api.counts()
	if configCallsAfter-configCallsBefore != 1 {
		t.Fatalf("producer-config called %d time(s) for this wave, want exactly 1 shared lookup serving both A and B", configCallsAfter-configCallsBefore)
	}
}

// TestEnsureEncryptionKeyID_SharedLookupCarriesCallerContextValues is the
// self-attack for the context.WithoutCancel fix: the shared lookup strips
// whichever triggering caller's cancellation, but it must still carry that
// caller's request-scoped context VALUES all the way to the transport — a
// custom RoundTripper reads the value off the actual request context the
// producer-config call traveled with.
//
// Negative control: reverting runKeyLookup to call
// resolveEncryptionKeyID(context.Background()) (head
// ac0d30d85d5540a95e7066d4f0a02cb749918272's behavior) makes this test fail:
// context.Background() carries no caller values, so the RoundTripper
// observes nil instead of the marker value.
func TestEnsureEncryptionKeyID_SharedLookupCarriesCallerContextValues(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusInternalServerError, `{"error":"boom"}`)
	_, keyService := newKeyServiceRecorder(t)
	p, _ := newProducerThroughConstructor(t, api.server.URL, keyService.URL)

	api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-with-ctx-value"}`)

	type ctxKey string
	const traceKey ctxKey = "trace-id"

	var (
		mu   sync.Mutex
		got  any
		seen bool
	)
	base := p.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	p.httpClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == producerConfigPath {
			mu.Lock()
			got, seen = req.Context().Value(traceKey), true
			mu.Unlock()
		}
		return base.RoundTrip(req)
	})

	ctx := context.WithValue(context.Background(), traceKey, "marker-value")
	if _, _, err := p.ensureEncryptionKeyID(ctx); err != nil {
		t.Fatalf("ensureEncryptionKeyID: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !seen {
		t.Fatal("the RoundTripper never observed a producer-config request")
	}
	if got != "marker-value" {
		t.Fatalf("producer-config request context value = %v, want %q: the shared lookup must carry the caller's request-scoped values even though its cancellation is stripped", got, "marker-value")
	}
}

// roundTripperFunc adapts a function to http.RoundTripper, for tests that
// need to inspect an outgoing request without a full fake transport.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestUploadDataset_CancelledWaiterLeavesNoGoroutineLeak proves — through
// deterministic lifecycle signals rather than a process-wide goroutine
// count — that a caller whose context is canceled while the shared lookup is
// still stalled does not leave that lookup's goroutine permanently blocked:
// once the stub is released, the lookup actually finishes (call.done closes)
// and clears keyLookupInFlight, both within a bounded timeout. A
// process-wide runtime.NumGoroutine comparison would pass even if the lookup
// leaked, as long as some unrelated goroutine elsewhere happened to exit in
// the same window and offset the count.
func TestUploadDataset_CancelledWaiterLeavesNoGoroutineLeak(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusInternalServerError, `{"error":"boom"}`)
	p, _ := newProducerThroughConstructor(t, api.server.URL, "http://127.0.0.1:1")

	api.setConfigResponse(http.StatusOK, `{"encryption_key_id":"key-after-stall"}`)
	api.configHang = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.UploadDataset(ctx, writeNDJSON(t, 3), testUploadOptions("leak-check")); !errors.Is(err, context.Canceled) {
		t.Fatalf("UploadDataset with a canceled context = %v, want context.Canceled", err)
	}

	// The canceled waiter returned without waiting for the shared lookup,
	// which must still be running (held open by the hang) until released.
	p.keyLookupMu.Lock()
	inFlight := p.keyLookupInFlight
	p.keyLookupMu.Unlock()
	if inFlight == nil {
		t.Fatal("no lookup in flight after the canceled waiter returned; want the shared lookup still running in the background")
	}

	api.releaseHang()

	// runKeyLookup clears keyLookupInFlight and closes call.done as the last
	// things it does before returning — a direct, deterministic signal that
	// the goroutine actually ran to completion instead of leaking.
	select {
	case <-inFlight.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the shared lookup's goroutine did not finish within 2s of being released: it leaked")
	}

	p.keyLookupMu.Lock()
	stillInFlight := p.keyLookupInFlight
	p.keyLookupMu.Unlock()
	if stillInFlight != nil {
		t.Fatal("keyLookupInFlight was not cleared after the lookup finished")
	}
}

// TestResolveEncryptionKeyID_CauseStaysReachable: the message is the authored
// capability-language one, and the API's own answer is still reachable for
// debugging through errors.As.
func TestResolveEncryptionKeyID_CauseStaysReachable(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusNotFound, `{"error":"no key"}`)
	p := newTestProducer(api.server.URL)

	got, err := p.resolveEncryptionKeyID(context.Background())
	if got != "" || err == nil {
		t.Fatalf("resolveEncryptionKeyID = %q, %v; want an empty key and an error", got, err)
	}
	if err.Error() != errEncryptionKeyUnresolved {
		t.Errorf("Error() = %q, want exactly %q", err.Error(), errEncryptionKeyUnresolved)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("errors.As(*APIError) = %v (%+v), want the 404 reachable as the cause", errors.As(err, &apiErr), apiErr)
	}
}

// TestNewProducer_DefaultsEndpointAndRegion: with no endpoint or region in the
// config, the producer-config call goes to HELIX_API_ENDPOINT and the region
// defaults, exactly as before.
func TestNewProducer_DefaultsEndpointAndRegion(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusOK, `{"encryption_key_id":"key-from-env-endpoint"}`)
	_, keyService := newKeyServiceRecorder(t)
	isolateAWSEnv(t, fakeIdentityServer(t).URL, keyService.URL)
	t.Setenv("HELIX_API_ENDPOINT", "  "+api.server.URL+"  ")

	cfg := testProducerConfig("")
	cfg.Region = ""
	var p *Producer
	captureStdout(t, func() {
		var err error
		if p, err = NewProducer(cfg); err != nil {
			t.Fatalf("NewProducer: %v", err)
		}
	})

	if p.APIEndpoint != api.server.URL || p.Region != "us-east-1" {
		t.Errorf("APIEndpoint=%q Region=%q, want %q and us-east-1", p.APIEndpoint, p.Region, api.server.URL)
	}
	if p.KMSKeyID != "key-from-env-endpoint" {
		t.Errorf("KMSKeyID = %q, want the value from the env-selected endpoint", p.KMSKeyID)
	}
}

// TestNewProducer_InvalidCredentialsStopBeforeProducerConfig: bad credentials
// fail construction outright, and the producer-config route is never asked.
func TestNewProducer_InvalidCredentialsStopBeforeProducerConfig(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusOK, `{"encryption_key_id":"never-used"}`)
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>InvalidClientTokenId</Code><Message>bad token</Message></Error><RequestId>r</RequestId></ErrorResponse>`))
	}))
	t.Cleanup(identity.Close)
	isolateAWSEnv(t, identity.URL, "http://127.0.0.1:1")

	p, err := NewProducer(testProducerConfig(api.server.URL))
	if err == nil || p != nil || err.Error() != "invalid AWS credentials" {
		t.Fatalf("NewProducer = %v, %v; want nil and exactly the invalid-credentials error", p, err)
	}
	if configCalls, _, _ := api.counts(); configCalls != 0 {
		t.Errorf("producer-config requested %d time(s) with invalid credentials, want 0", configCalls)
	}
}

// TestNewProducer_NoCredentialsIsAnError: with no credentials at all the
// provider cannot be selected and nothing is called.
func TestNewProducer_NoCredentialsIsAnError(t *testing.T) {
	api := newProducerConfigAPI(t, http.StatusOK, `{"encryption_key_id":"never-used"}`)
	isolateAWSEnv(t, "http://127.0.0.1:1", "http://127.0.0.1:1")

	cfg := testProducerConfig(api.server.URL)
	cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey = "", ""
	if p, err := NewProducer(cfg); err == nil || p != nil {
		t.Fatalf("NewProducer = %v, %v; want nil and an error", p, err)
	}
	if configCalls, _, _ := api.counts(); configCalls != 0 {
		t.Errorf("producer-config requested %d time(s) without credentials, want 0", configCalls)
	}
}
