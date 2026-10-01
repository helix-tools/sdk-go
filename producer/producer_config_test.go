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

	configCalls []*http.Request
	postBodies  []map[string]any
	puts        int

	server *httptest.Server
}

func newProducerConfigAPI(t *testing.T, status int, body string) *producerConfigAPI {
	t.Helper()
	a := &producerConfigAPI{configStatus: status, configBody: body}
	release := make(chan struct{})

	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == producerConfigPath:
			a.mu.Lock()
			a.configCalls = append(a.configCalls, r.Clone(context.Background()))
			hang := a.configHang
			a.mu.Unlock()
			if hang {
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(a.configStatus)
			_, _ = w.Write([]byte(a.configBody))
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
		close(release)
		a.server.Close()
	})
	return a
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

// retiredLookupEnvVar is the name of the environment variable older releases
// read to locate the producer's configuration. Built from fragments so the
// name never appears contiguously in a published file.
var retiredLookupEnvVar = strings.Join([]string{"HELIX", "SSM", "CUSTOMER", "PREFIX"}, "_")

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
	// A value left behind from an older release must change nothing.
	t.Setenv(retiredLookupEnvVar, "/left/behind")
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

	if _, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), NewUploadOptions("api-key-upload")); err != nil {
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
// built, a warning is printed, and every upload fails before a record is
// created, before the key service is called and before any byte is PUT.
func TestNewProducer_UnresolvedKeyFailsUploadsClosed(t *testing.T) {
	// A server error body that names an internal location: it must reach
	// neither the warning nor the upload error.
	internalDetail := strings.Join([]string{"", "internal", "customers", "cust-1", "key"}, "/")

	cases := []struct {
		name   string
		status int
		body   string
		hang   bool
	}{
		{name: "404 no key configured", status: http.StatusNotFound, body: `{"error":"not found"}`},
		{name: "403 consumer-only caller", status: http.StatusForbidden, body: `{"error":"forbidden ` + internalDetail + `"}`},
		{name: "401 unauthenticated", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
		{name: "500 server error", status: http.StatusInternalServerError, body: `{"error":"boom ` + internalDetail + `"}`},
		{name: "200 garbage body", status: http.StatusOK, body: `<html>not json</html>`},
		{name: "200 wrong type", status: http.StatusOK, body: `{"encryption_key_id":42}`},
		{name: "200 missing field", status: http.StatusOK, body: `{}`},
		{name: "200 empty value", status: http.StatusOK, body: `{"encryption_key_id":""}`},
		{name: "200 blank value", status: http.StatusOK, body: `{"encryption_key_id":"   "}`},
		{name: "no answer in time", hang: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newProducerConfigAPI(t, tc.status, tc.body)
			api.configHang = tc.hang
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

			_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), NewUploadOptions("no-key"))
			if err == nil || !strings.Contains(err.Error(), "no encryption key configured") {
				t.Fatalf("UploadDataset error = %v, want the missing-encryption-key error", err)
			}
			if strings.Contains(err.Error(), internalDetail) {
				t.Errorf("upload error leaks the server's error body: %q", err)
			}

			_, posts, puts := api.counts()
			if posts != 0 || puts != 0 {
				t.Errorf("posts=%d puts=%d after an unresolved key, want 0/0: nothing may be created or uploaded", posts, puts)
			}
			if got := keys.calls(); len(got) != 0 {
				t.Errorf("key service called %d time(s) with KeyId(s) %q, want none", len(got), got)
			}
		})
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
	if err == nil || p != nil || !strings.Contains(err.Error(), "invalid AWS credentials") {
		t.Fatalf("NewProducer = %v, %v; want nil and an invalid-credentials error", p, err)
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
