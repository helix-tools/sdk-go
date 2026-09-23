package producer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// newFakeKMSServer returns an httptest server that answers KMS Encrypt calls
// (the only KMS action encryptData issues) with a fixed, decodable
// CiphertextBlob so processFile can complete without a live KMS key.
func newFakeKMSServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
// API that refuses the POST, and a mock presigned-PUT target that must never
// receive a request.
func TestUploadDataset_NothingUploadedWhenPOSTRefused(t *testing.T) {
	for _, status := range []int{426, 403, 500} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			var putCount int64
			putServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&putCount, 1)
				w.WriteHeader(http.StatusOK)
			}))
			defer putServer.Close()

			apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/datasets" && r.Method == http.MethodPost {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":"refused"}`))
					return
				}
				t.Errorf("unexpected request %s %s; a refused POST must not lead to any further call", r.Method, r.URL.Path)
			}))
			defer apiServer.Close()

			kmsServer := newFakeKMSServer(t)
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

			if got := atomic.LoadInt64(&putCount); got != 0 {
				t.Errorf("presigned-PUT server received %d request(s), want 0 — a refused POST must upload nothing", got)
			}
		})
	}
}

// TestUploadDataset_ProcessesBeforePOST_SoRealSizesReachTheBody is the
// end-to-end happy path proving the reordering actually took effect: the
// POST body the mock API receives carries the REAL sizes of the processed
// (compressed+encrypted) bytes, not zeros — only possible if processFile ran
// before createDatasetRecord built the payload. It also exercises the
// presigned PUT and the trailing GET, and inspects the uploaded envelope to
// confirm its SHAPE is exactly what encryptData has always produced ([4B key
// length][encrypted key][16B iv][16B tag][ciphertext]) — i.e. only the call
// ORDER changed, not compressData/encryptData themselves.
func TestUploadDataset_ProcessesBeforePOST_SoRealSizesReachTheBody(t *testing.T) {
	plaintext := []byte(strings.Repeat(`{"id":1,"name":"ringboost"}`+"\n", 500))
	dataFile := filepath.Join(t.TempDir(), "data.ndjson")
	if err := os.WriteFile(dataFile, plaintext, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
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
			_, _ = w.Write([]byte(`{"_id":"ds-real-sizes","id":"ds-real-sizes","name":"real-sizes-test"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	kmsServer := newFakeKMSServer(t)
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
	if !ok || compressedSize <= 0 || compressedSize >= originalSize {
		t.Errorf("metadata.compressed_size_bytes = %v, want a real positive value smaller than original_size_bytes (%v)", md["compressed_size_bytes"], originalSize)
	}
	encryptedSize, ok := md["encrypted_size_bytes"].(float64)
	if !ok || encryptedSize <= compressedSize {
		t.Errorf("metadata.encrypted_size_bytes = %v, want a real value larger than compressed_size_bytes (%v, envelope overhead)", md["encrypted_size_bytes"], compressedSize)
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
			_, _ = w.Write([]byte(`{"_id":"ds-override-e2e","id":"ds-override-e2e"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	kmsServer := newFakeKMSServer(t)
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
