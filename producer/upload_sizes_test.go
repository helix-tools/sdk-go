package producer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

var isoDateRegex = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// newTestProducerWithKMS extends newTestProducer with a kmsClient pointed at
// a local mock so the real UploadDataset() flow (which requires
// Encrypt+Compress) can run end-to-end without a live AWS account.
func newTestProducerWithKMS(apiURL, kmsURL string) *Producer {
	p := newTestProducer(apiURL)
	p.KMSKeyID = "test-kms-key"
	p.kmsClient = kms.NewFromConfig(p.awsConfig, func(o *kms.Options) {
		o.BaseEndpoint = aws.String(kmsURL)
		o.Credentials = credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", "")
	})
	return p
}

// fakeKMSCiphertextBlobB64 / fakeKMSCiphertextBlob are the fixed
// "CiphertextBlob" the mock KMS server below always returns for an Encrypt
// call, and its decoded form, so tests can assert the exact bytes encryptData
// embedded for the encrypted data key.
const fakeKMSCiphertextBlobB64 = "ZmFrZS1jaXBoZXJ0ZXh0LWJsb2I="

// capturedDataKey lets a test recover the REAL AES-256 data key
// encryptData generated and sent to KMS Encrypt as plaintext (KMS's real
// job is only to protect that key at rest; a mock never needs to, so
// capturing it here is enough to fully decrypt what was uploaded and prove
// a real round trip, not just envelope shape).
type capturedDataKey struct {
	mu  sync.Mutex
	key []byte
}

func (c *capturedDataKey) get() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key
}

// newFakeKMSServer returns an httptest server that answers KMS Encrypt calls
// (the only KMS action encryptData issues) with a fixed, decodable
// CiphertextBlob so processFile can complete without a live KMS key. When
// capture is non-nil, it also records the real plaintext data key from each
// request so a test can independently decrypt the uploaded envelope.
func newFakeKMSServer(t *testing.T, capture *capturedDataKey) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			var req struct {
				Plaintext string `json:"Plaintext"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &req)
			if dataKey, err := base64.StdEncoding.DecodeString(req.Plaintext); err == nil {
				capture.mu.Lock()
				capture.key = dataKey
				capture.mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"CiphertextBlob":"` + fakeKMSCiphertextBlobB64 + `","KeyId":"test-kms-key"}`))
	}))
}

func writeNDJSON(t *testing.T, lines int) string {
	t.Helper()
	tmpDir := t.TempDir()
	dataFile := filepath.Join(tmpDir, "data.ndjson")
	if err := os.WriteFile(dataFile, []byte(strings.Repeat(`{"id":1,"name":"x"}`+"\n", lines)), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return dataFile
}

// TestCreateDatasetRecord_MatchesV1FieldTable is acceptance question 1's
// field table: every field v1.3.11's buildDatasetPayload sent (producer.go,
// tag v1.3.11) must be present in the v2.16 POST body, sourced from the same
// place v1 sourced it (the completed processFile pass for sizes, the
// analysis pass for record_count, computed UTC date for version).
func TestCreateDatasetRecord_MatchesV1FieldTable(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	dataFile := writeNDJSON(t, 20)

	opts := NewUploadOptions("sizes-table-test")
	processed := &ProcessedFileData{
		OriginalSize: 424420949,
		Sizes: map[string]any{
			"original_size_bytes":   int64(424420949),
			"compressed_size_bytes": int64(12248326),
			"encrypted_size_bytes":  int64(12248390),
			"encryption_enabled":    true,
			"compression_enabled":   true,
		},
	}

	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, processed); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	md, ok := gotBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata is not an object: %T", gotBody["metadata"])
	}

	cases := []struct {
		field string
		want  float64 // json numbers decode to float64
	}{
		{"original_size_bytes", 424420949},
		{"compressed_size_bytes", 12248326},
		{"encrypted_size_bytes", 12248390},
	}
	for _, c := range cases {
		got, ok := md[c.field]
		if !ok {
			t.Errorf("metadata.%s missing; keys=%v", c.field, keysOf(md))
			continue
		}
		if got != c.want {
			t.Errorf("metadata.%s = %v, want %v", c.field, got, c.want)
		}
	}

	if md["file_format"] != "json" {
		t.Errorf("metadata.file_format = %v, want %q (v1.3.11 default)", md["file_format"], "json")
	}
	if md["encoding"] != "utf-8" {
		t.Errorf("metadata.encoding = %v, want %q (v1.3.11 default)", md["encoding"], "utf-8")
	}

	version, ok := gotBody["version"].(string)
	if !ok || !isoDateRegex.MatchString(version) {
		t.Errorf("top-level version = %v, want an unquoted YYYY-MM-DD date (v1.3.11 computes now.Format(\"2006-01-02\"))", gotBody["version"])
	}

	// 20 NDJSON lines -> analyzeData must count 20 records; v1 sent this
	// BOTH top-level and inside metadata (buildDatasetPayload).
	if rc, ok := gotBody["record_count"].(float64); !ok || rc != 20 {
		t.Errorf("top-level record_count = %v, want 20", gotBody["record_count"])
	}
	if rc, ok := md["record_count"].(float64); !ok || rc != 20 {
		t.Errorf("metadata.record_count = %v, want 20", md["record_count"])
	}
}

// TestCreateDatasetRecord_RecordCountDefaultsZeroWhenAnalysisFails matches
// v1.3.11's buildDatasetPayload, which always set record_count (top-level
// and in metadata) to 0 rather than omitting the key when analysis failed —
// v2.15.0 omitted it entirely in that case.
func TestCreateDatasetRecord_RecordCountDefaultsZeroWhenAnalysisFails(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	// createDatasetRecord's OWN analyzeData call reads filePath directly; a
	// directory (not a regular file) makes os.Open's subsequent Read fail
	// deterministically, so analysis fails and the default-0 path runs.
	tmpDir := t.TempDir()

	opts := NewUploadOptions("record-count-default-test")
	if _, err := p.createDatasetRecord(context.Background(), tmpDir, opts, fakeProcessedFileData()); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	if rc, ok := gotBody["record_count"].(float64); !ok || rc != 0 {
		t.Errorf("top-level record_count = %v, want 0 (analysis unavailable)", gotBody["record_count"])
	}
	md := gotBody["metadata"].(map[string]any)
	if rc, ok := md["record_count"].(float64); !ok || rc != 0 {
		t.Errorf("metadata.record_count = %v, want 0 (analysis unavailable)", md["record_count"])
	}
}

// TestCreateDatasetRecord_OverridesWinOverComputedValues is acceptance
// question 3: a caller's explicit DatasetOverrides values still win over the
// SDK-computed version/record_count, matching v1.3.11's
// deepMergeMaps(payload, overrideCopy) (overrides applied last).
func TestCreateDatasetRecord_OverridesWinOverComputedValues(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	dataFile := writeNDJSON(t, 5)

	opts := NewUploadOptions("override-wins-test")
	opts.DatasetOverrides = map[string]any{
		"version":      "2020-01-01",
		"record_count": 999,
	}
	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, fakeProcessedFileData()); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	if gotBody["version"] != "2020-01-01" {
		t.Errorf("version = %v, want caller override %q to win over the computed UTC date", gotBody["version"], "2020-01-01")
	}
	if rc, ok := gotBody["record_count"].(float64); !ok || rc != 999 {
		t.Errorf("record_count = %v, want caller override 999 to win over the computed count", gotBody["record_count"])
	}
}

// TestCreateDatasetRecord_ExplicitEmptyVersionOverridesComputedDate is the
// self-attack question: does an explicit version="" in DatasetOverrides win
// (blanking the version) the same way v1.3.11 would have, versus a caller
// who never touches DatasetOverrides.version at all (computed date wins)?
// v1.3.11's deepMergeMaps does `base[key] = value` unconditionally for any
// key present in the override map, including an explicit "" — so yes, an
// explicit empty override DOES blank the version. This test pins that this
// SDK reproduces that exact (if surprising) v1 behavior, and that omitting
// the key entirely is the only way to get the computed default.
func TestCreateDatasetRecord_ExplicitEmptyVersionOverridesComputedDate(t *testing.T) {
	t.Run("explicit empty string override wins, blanking version", func(t *testing.T) {
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
		}))
		defer server.Close()

		p := newTestProducer(server.URL)
		dataFile := writeNDJSON(t, 5)

		opts := NewUploadOptions("empty-version-override-test")
		opts.DatasetOverrides = map[string]any{"version": ""}
		if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, fakeProcessedFileData()); err != nil {
			t.Fatalf("createDatasetRecord returned error: %v", err)
		}

		if v, ok := gotBody["version"]; !ok || v != "" {
			t.Errorf("version = %v (present=%v), want explicit empty-string override to win and blank it, matching v1.3.11", v, ok)
		}
	})

	t.Run("version key omitted from overrides leaves the computed UTC date", func(t *testing.T) {
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
		}))
		defer server.Close()

		p := newTestProducer(server.URL)
		dataFile := writeNDJSON(t, 5)

		opts := NewUploadOptions("no-version-override-test")
		opts.DatasetOverrides = map[string]any{"category": "custom"} // touches overrides, but never "version"
		if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, fakeProcessedFileData()); err != nil {
			t.Fatalf("createDatasetRecord returned error: %v", err)
		}

		version, ok := gotBody["version"].(string)
		if !ok || !isoDateRegex.MatchString(version) || version == "" {
			t.Errorf("version = %v, want the computed UTC date since DatasetOverrides never set \"version\"", gotBody["version"])
		}
	})
}

// TestUploadDataset_NothingUploadedWhenPOSTRefused is acceptance question 2:
// it drives the REAL public UploadDataset end-to-end (real processFile
// compress+KMS-encrypt pass, real createDatasetRecord POST) against a mock
// API that refuses the POST. Unlike an earlier version of this test, it does
// NOT check a separate, never-wired-in "presigned PUT" server for zero hits
// — that only proves the producer didn't call THAT specific URL, not that it
// made no further call at all. Instead every producer-bound HTTP request
// (POST, and any hypothetical follow-up PUT/GET a regression might add) goes
// through ONE shared handler that counts every hit, so "exactly 1 request
// total" is the actual invariant under test.
func TestUploadDataset_NothingUploadedWhenPOSTRefused(t *testing.T) {
	for _, status := range []int{426, 403, 500} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			var totalRequests int64
			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&totalRequests, 1)
				if r.URL.Path == "/v1/datasets" && r.Method == http.MethodPost {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":"refused"}`))
					return
				}
				t.Errorf("unexpected request %s %s; a refused POST must not lead to any further call", r.Method, r.URL.Path)
			}))
			defer apiServer.Close()

			kmsServer := newFakeKMSServer(t, nil)
			defer kmsServer.Close()

			p := newTestProducerWithKMS(apiServer.URL, kmsServer.URL)
			dataFile := writeNDJSON(t, 5)

			opts := NewUploadOptions("post-refused-test")
			_, err := p.UploadDataset(context.Background(), dataFile, opts)
			if err == nil {
				t.Fatal("expected UploadDataset to return an error when the POST is refused")
			}
			if !strings.Contains(err.Error(), "failed to create dataset record") {
				t.Errorf("expected error to mention dataset-record creation, got: %v", err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expected error to unwrap to *APIError, got %T: %v", err, err)
			}
			if apiErr.StatusCode != status {
				t.Errorf("APIError.StatusCode = %d, want %d", apiErr.StatusCode, status)
			}

			// Exactly 1: the refused POST itself, and NOTHING else — no PUT to
			// any URL, no GET. TestUploadDataset_ProcessesBeforePOST_SoRealSizesReachTheBody
			// is this counter's positive control: it proves the identical
			// counting mechanism correctly reaches >1 on a real POST+PUT+GET
			// happy path, so a silent "counts nothing, always passes" bug in
			// the counter itself would be caught there, not hidden here.
			if got := atomic.LoadInt64(&totalRequests); got != 1 {
				t.Errorf("apiServer received %d total request(s), want exactly 1 (the refused POST) — a refused POST must upload nothing and issue no further call", got)
			}
		})
	}
}

// TestUploadDataset_ProcessesBeforePOST_SoRealSizesReachTheBody is the
// end-to-end happy path proving the reordering actually took effect: the
// POST body the mock API receives carries the REAL sizes of the processed
// (compressed+encrypted) bytes, not zeros — only possible if processFile ran
// before createDatasetRecord built the payload. It also exercises the
// presigned PUT and the trailing GET, and — using the fake KMS server's
// captured (real) data key — fully AES-256-GCM DECRYPTS and gunzips the
// uploaded envelope back to the exact original plaintext, proving a real
// round trip rather than just the envelope's byte-length shape. A tampered
// copy of the same envelope must fail to decrypt (negative control), which
// is what makes the successful decrypt above meaningful rather than
// accidental.
func TestUploadDataset_ProcessesBeforePOST_SoRealSizesReachTheBody(t *testing.T) {
	plaintext := []byte(strings.Repeat(`{"id":1,"name":"ringboost"}`+"\n", 500))
	dataFile := filepath.Join(t.TempDir(), "data.ndjson")
	if err := os.WriteFile(dataFile, plaintext, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	// Independently computed expected compressed size, via the same
	// (unmodified) compressData this PR does not touch — an exact target,
	// not just "smaller than original".
	wantCompressed, err := (&Producer{}).compressData(plaintext, 6)
	if err != nil {
		t.Fatalf("compressData (expected value) returned error: %v", err)
	}

	var postBody map[string]any
	var uploadedBytes []byte
	var putHits int64

	putServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&putHits, 1)
		body, _ := io.ReadAll(r.Body)
		uploadedBytes = body
		w.WriteHeader(http.StatusOK)
	}))
	defer putServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/datasets" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &postBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "ds-real-sizes",
				"upload_url": putServer.URL,
				"s3_key":     "datasets/real-sizes-test/data.ndjson.gz",
			})
		case r.URL.Path == "/v1/datasets/ds-real-sizes" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ds-real-sizes","name":"real-sizes-test"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	var capture capturedDataKey
	kmsServer := newFakeKMSServer(t, &capture)
	defer kmsServer.Close()

	p := newTestProducerWithKMS(apiServer.URL, kmsServer.URL)
	opts := NewUploadOptions("real-sizes-test")

	ds, err := p.UploadDataset(context.Background(), dataFile, opts)
	if err != nil {
		t.Fatalf("UploadDataset returned error: %v", err)
	}
	if ds.ID != "ds-real-sizes" {
		t.Errorf("expected returned dataset id ds-real-sizes, got %q", ds.ID)
	}
	if atomic.LoadInt64(&putHits) != 1 {
		t.Fatalf("expected exactly 1 PUT to the presigned URL, got %d", putHits)
	}

	md, ok := postBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("POST body metadata missing or wrong type: %T", postBody["metadata"])
	}

	originalSize, ok := md["original_size_bytes"].(float64)
	if !ok || int(originalSize) != len(plaintext) {
		t.Errorf("metadata.original_size_bytes = %v, want %d (the real plaintext size — proves processFile ran BEFORE the POST)", md["original_size_bytes"], len(plaintext))
	}
	compressedSize, ok := md["compressed_size_bytes"].(float64)
	if !ok || int(compressedSize) != len(wantCompressed) {
		t.Errorf("metadata.compressed_size_bytes = %v, want exactly %d (independently computed via compressData)", md["compressed_size_bytes"], len(wantCompressed))
	}
	encryptedSize, ok := md["encrypted_size_bytes"].(float64)
	if !ok || int(encryptedSize) != len(uploadedBytes) {
		t.Errorf("metadata.encrypted_size_bytes = %v, want exactly %d (len of the bytes actually uploaded)", md["encrypted_size_bytes"], len(uploadedBytes))
	}

	// Top-level size_bytes (restores v1.3.11's dataset_payload.go:168
	// finalSize, which the API maps to the catalog's total_size_bytes) must
	// be present, positive, and equal the exact byte length of the object
	// actually PUT to S3 — not a derived/duplicated value that could drift
	// from what was really uploaded.
	sizeBytes, ok := postBody["size_bytes"].(float64)
	if !ok {
		t.Fatalf("top-level size_bytes missing or wrong type: %T", postBody["size_bytes"])
	}
	if sizeBytes <= 0 {
		t.Errorf("top-level size_bytes = %v, want > 0", sizeBytes)
	}
	if int(sizeBytes) != len(uploadedBytes) {
		t.Errorf("top-level size_bytes = %v, want exactly %d (len of the bytes actually PUT to S3)", sizeBytes, len(uploadedBytes))
	}

	if len(uploadedBytes) < 4+16+16 {
		t.Fatalf("uploaded data too short to contain the envelope header: %d bytes", len(uploadedBytes))
	}
	keyLen := binary.BigEndian.Uint32(uploadedBytes[:4])
	offset := 4 + int(keyLen)
	if offset+32 > len(uploadedBytes) {
		t.Fatalf("uploaded data too short for iv+tag after a %d-byte encrypted key", keyLen)
	}
	encryptedKeySection := uploadedBytes[4:offset]
	iv := uploadedBytes[offset : offset+16]
	tag := uploadedBytes[offset+16 : offset+32]
	ciphertext := uploadedBytes[offset+32:]

	wantKeyBlob, _ := base64.StdEncoding.DecodeString(fakeKMSCiphertextBlobB64)
	if !bytes.Equal(encryptedKeySection, wantKeyBlob) {
		t.Errorf("encrypted-key section = %x, want the fake KMS CiphertextBlob %x embedded unchanged", encryptedKeySection, wantKeyBlob)
	}
	if len(iv) != 16 || len(tag) != 16 {
		t.Fatalf("iv/tag section lengths = %d/%d, want 16/16 (encryptData's fixed envelope)", len(iv), len(tag))
	}
	if len(ciphertext) == 0 {
		t.Fatalf("ciphertext section is empty")
	}

	// Real decrypt: the fake KMS server captured the REAL AES-256 data key
	// encryptData generated (it sends that key to KMS Encrypt as plaintext;
	// a real KMS would protect it at rest, but our mock only needs to see
	// it). Mirror encryptData's exact scheme in reverse.
	dataKey := capture.get()
	if len(dataKey) != 32 {
		t.Fatalf("captured data key length = %d, want 32 (AES-256)", len(dataKey))
	}
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	aesGCM, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		t.Fatalf("cipher.NewGCMWithNonceSize: %v", err)
	}
	sealed := append(append([]byte{}, ciphertext...), tag...)

	decompressed, err := aesGCM.Open(nil, iv, sealed, nil)
	if err != nil {
		t.Fatalf("AES-GCM decrypt of the uploaded envelope failed: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(decompressed))
	if err != nil {
		t.Fatalf("gzip.NewReader on decrypted data: %v", err)
	}
	defer func() { _ = gz.Close() }()
	gotPlaintext, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip of decrypted data: %v", err)
	}
	if !bytes.Equal(gotPlaintext, plaintext) {
		t.Fatalf("full round-trip mismatch: decrypted+decompressed %d bytes, want the original %d-byte plaintext", len(gotPlaintext), len(plaintext))
	}

	// Negative control: a tampered tag must fail to decrypt — proving the
	// successful Open() above is really authenticating this exact envelope,
	// not a no-op that would accept anything.
	tamperedSealed := append([]byte{}, sealed...)
	tamperedSealed[len(tamperedSealed)-1] ^= 0xFF
	if _, err := aesGCM.Open(nil, iv, tamperedSealed, nil); err == nil {
		t.Fatal("expected AES-GCM decrypt to fail on a tampered tag, got success")
	}
}

// TestCreateDatasetRecord_SizeBytesTopLevel_CompressOnly is chunk A(2) of the
// size_bytes fix: UploadDataset itself hard-requires Encrypt=true
// (processFile refuses Encrypt=false), so a real compress-only upload can
// never reach createDatasetRecord through the public entry point. This test
// exercises createDatasetRecord directly with a processed result shaped like
// a compress-only pass (encryption_enabled=false, Data holding only the
// compressed bytes) to prove size_bytes tracks len(processed.Data) — the
// exact bytes that would be PUT to S3 — rather than being hardwired to the
// encrypted-size case, so the field stays correct if compress-only is ever
// allowed.
func TestCreateDatasetRecord_SizeBytesTopLevel_CompressOnly(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ds-1","upload_url":"https://example.invalid/put"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	dataFile := writeNDJSON(t, 5)

	compressedOnlyBytes := []byte("compressed-only-payload-bytes")
	processed := &ProcessedFileData{
		OriginalSize: 100,
		Data:         compressedOnlyBytes,
		Sizes: map[string]any{
			"original_size_bytes":   int64(100),
			"compressed_size_bytes": int64(len(compressedOnlyBytes)),
			"encrypted_size_bytes":  int64(100),
			"encryption_enabled":    false,
			"compression_enabled":   true,
		},
	}

	opts := NewUploadOptions("compress-only-size-bytes-test")
	if _, err := p.createDatasetRecord(context.Background(), dataFile, opts, processed); err != nil {
		t.Fatalf("createDatasetRecord returned error: %v", err)
	}

	sizeBytes, ok := gotBody["size_bytes"].(float64)
	if !ok {
		t.Fatalf("top-level size_bytes missing or wrong type: %T", gotBody["size_bytes"])
	}
	if sizeBytes <= 0 {
		t.Errorf("top-level size_bytes = %v, want > 0", sizeBytes)
	}
	if int(sizeBytes) != len(compressedOnlyBytes) {
		t.Errorf("top-level size_bytes = %v, want exactly %d (len of processed.Data, the bytes that would be PUT to S3 in a compress-only upload)", sizeBytes, len(compressedOnlyBytes))
	}
	md := gotBody["metadata"].(map[string]any)
	if md["compressed_size_bytes"] != sizeBytes {
		t.Errorf("top-level size_bytes = %v, want it to equal metadata.compressed_size_bytes = %v (Compress-only contract)", sizeBytes, md["compressed_size_bytes"])
	}
}

// TestCompressData_GzipRoundTrip is the closest available golden test to
// acceptance question 4 ("bytes on wire byte-identical to v2.15.0 for the
// same input") given this change does not touch compressData or
// encryptData at all (only call order and payload construction changed,
// verified by `git diff` leaving both functions untouched): it proves
// compressData — the exact function processFile calls, unmodified by this
// PR — still gunzips back to the original plaintext byte-for-byte, the same
// compressData v2.15.0 shipped. (processFile itself can't isolate
// compression alone: it hard-requires Encrypt=true, covered by the
// KMS-mocked end-to-end tests above.)
func TestCompressData_GzipRoundTrip(t *testing.T) {
	plaintext := []byte(strings.Repeat(`{"id":1,"name":"golden"}`+"\n", 300))

	p := &Producer{CustomerID: "golden-test"}
	compressed, err := p.compressData(plaintext, 6)
	if err != nil {
		t.Fatalf("compressData returned error: %v", err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }()

	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-tripped plaintext mismatch: got %d bytes, want %d bytes", len(got), len(plaintext))
	}
}

// TestUploadDataset_ExplicitVersionOverride_EndToEnd is the full
// UploadDataset()-level counterpart to
// TestCreateDatasetRecord_ExplicitEmptyVersionOverridesComputedDate: proves
// the override behavior holds through the real, reordered public entry
// point, not just the internal helper.
func TestUploadDataset_ExplicitVersionOverride_EndToEnd(t *testing.T) {
	var postBody map[string]any
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/datasets" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &postBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "ds-override-e2e", "upload_url": "", "s3_key": "datasets/x/data.ndjson.gz",
			})
		case strings.HasPrefix(r.URL.Path, "/v1/datasets/") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"ds-override-e2e"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	kmsServer := newFakeKMSServer(t, nil)
	defer kmsServer.Close()

	p := newTestProducerWithKMS(apiServer.URL, kmsServer.URL)
	dataFile := writeNDJSON(t, 3)

	opts := NewUploadOptions("override-e2e-test")
	opts.DatasetOverrides = map[string]any{"version": "1999-12-31"}

	// The create-response's upload_url is deliberately "" (see below); this
	// test only asserts the POST body reflects the override, so a PUT
	// failure afterward is expected and not itself a test failure.
	if _, err := p.UploadDataset(context.Background(), dataFile, opts); err != nil {
		// The presigned PUT points at "" (invalid) deliberately for this
		// override-focused test — only the POST body matters here, so a PUT
		// failure is expected and not itself a test failure.
		if !strings.Contains(err.Error(), "dataset record created but upload failed") {
			t.Fatalf("UploadDataset returned an unexpected error: %v", err)
		}
	}

	if postBody["version"] != "1999-12-31" {
		t.Errorf("version = %v, want the caller's override to win end-to-end", postBody["version"])
	}
}
