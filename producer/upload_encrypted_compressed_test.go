package producer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestUploadDataset_CannotDisableEncryptionOrCompression is R1: no option may
// turn encryption or compression off, and every attempt is refused BEFORE any
// network call — API, presigned URL or KMS. Every server the producer could
// reach is the same counting server, so "0 requests" is the invariant.
//
// The rows after the plain flags are the evasions: the record's flags can also
// be reached through UploadOptions.Metadata and UploadOptions.DatasetOverrides
// (top-level or nested under "metadata"), so a caller could upload a file that
// is encrypted while the catalog says it is not — or the reverse.
func TestUploadDataset_CannotDisableEncryptionOrCompression(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(o *UploadOptions)
		noKMS   bool
		wantErr string
	}{
		{"Encrypt=false", func(o *UploadOptions) { o.Encrypt = false }, false, "encryption is required"},
		{"Compress=false", func(o *UploadOptions) { o.Compress = false }, false, "compression is required"},
		{"both false", func(o *UploadOptions) { o.Encrypt, o.Compress = false, false }, false, "encryption is required"},
		{"zero-value options", func(o *UploadOptions) { *o = UploadOptions{DatasetName: "x"} }, false, "encryption is required"},
		{"missing KMS key", func(o *UploadOptions) {}, true, "KMS key not found"},
		{"invalid gzip level", func(o *UploadOptions) { o.CompressionLevel = 10 }, false, "gzip"},
		{
			"metadata says encryption off",
			func(o *UploadOptions) { o.Metadata = map[string]any{"encryption_enabled": false} },
			false, "cannot be disabled",
		},
		{
			"metadata says compression is the string false",
			func(o *UploadOptions) { o.Metadata = map[string]any{"compression_enabled": "false"} },
			false, "cannot be disabled",
		},
		{
			"override encryption_enabled=false",
			func(o *UploadOptions) { o.DatasetOverrides = map[string]any{"encryption_enabled": false} },
			false, "cannot be disabled",
		},
		{
			"override top-level encryption=false",
			func(o *UploadOptions) { o.DatasetOverrides = map[string]any{"encryption": false} },
			false, "cannot be disabled",
		},
		{
			"override compression_enabled=null",
			func(o *UploadOptions) { o.DatasetOverrides = map[string]any{"compression_enabled": nil} },
			false, "cannot be disabled",
		},
		{
			"override metadata replaces the flags with false",
			func(o *UploadOptions) {
				o.DatasetOverrides = map[string]any{"metadata": map[string]any{"encryption_enabled": false}}
			},
			false, "cannot be disabled",
		},
		{
			"override metadata as a typed map carrying false",
			func(o *UploadOptions) {
				o.DatasetOverrides = map[string]any{"metadata": map[string]bool{"compression_enabled": false}}
			},
			false, "cannot be disabled",
		},
		{
			"override metadata that is not an object",
			func(o *UploadOptions) { o.DatasetOverrides = map[string]any{"metadata": "encryption_enabled=false"} },
			false, "must be an object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer sink.Close()

			p := newTestProducerWithKMS(sink.URL, sink.URL)
			if tc.noKMS {
				p.KMSKeyID = ""
			}
			opts := NewUploadOptions("cannot-disable")
			tc.mutate(&opts)

			_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), opts)
			if err == nil {
				t.Fatal("UploadDataset accepted options that disable encryption or compression")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("%d network request(s) reached a server before the refusal; want 0", got)
			}
		})
	}
}

// TestUploadDataset_DefaultOptionsStayEncryptedAndCompressed: the defaults are
// the only mode there is.
func TestUploadDataset_DefaultOptionsStayEncryptedAndCompressed(t *testing.T) {
	opts := NewUploadOptions("d")
	if !opts.Encrypt || !opts.Compress || opts.CompressionLevel != 6 {
		t.Fatalf("NewUploadOptions = Encrypt %v Compress %v level %d, want true true 6", opts.Encrypt, opts.Compress, opts.CompressionLevel)
	}
}

// uploadFixture is a hermetic producer: API, presigned-URL target and KMS.
type uploadFixture struct {
	postBody      map[string]any
	uploaded      []byte
	order         []string
	dataKey       capturedDataKey
	p             *Producer
	datasetIDBack string
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	f := &uploadFixture{datasetIDBack: "dataset-from-the-server"}

	put := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.order = append(f.order, "PUT")
		f.uploaded, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(put.Close)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/datasets":
			f.order = append(f.order, "POST")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &f.postBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         f.datasetIDBack,
				"upload_url": put.URL,
				"s3_key":     "datasets/x/data.ndjson.gz",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/datasets/"):
			f.order = append(f.order, "GET")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + f.datasetIDBack + `","name":"n"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	kmsServer := newFakeKMSServer(t, &f.dataKey)
	t.Cleanup(kmsServer.Close)

	f.p = newTestProducerWithKMS(api.URL, kmsServer.URL)
	return f
}

// openUploaded reverses what UploadDataset stored: envelope, then gzip.
func (f *uploadFixture) openUploaded(t *testing.T) []byte {
	t.Helper()
	obj := f.uploaded
	keyLen := int(binary.BigEndian.Uint32(obj[:4]))
	off := 4 + keyLen
	iv, tag, ct := obj[off:off+16], obj[off+16:off+32], obj[off+32:]

	block, err := aes.NewCipher(f.dataKey.get())
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	gzipped, err := gcm.Open(nil, iv, append(append([]byte{}, ct...), tag...), nil)
	if err != nil {
		t.Fatalf("decrypt uploaded object: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		t.Fatalf("the uploaded object is not gzip once decrypted: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return plain
}

// TestUploadDataset_RingboostCallPathIsUnchanged drives UploadDataset with
// exactly the options Ringboost's vendor exporter passes (Encrypt, Compress,
// CompressionLevel 6, its own Metadata keys and the version/original_size/
// record_count DatasetOverrides) and pins that the call still succeeds and
// sends what it always sent — plus the R1 record flags and no client-made id.
func TestUploadDataset_RingboostCallPathIsUnchanged(t *testing.T) {
	f := newUploadFixture(t)

	plaintext := []byte(strings.Repeat(`{"phone":"+15550100"}`+"\n", 40))
	dataFile := filepath.Join(t.TempDir(), "merged.ndjson")
	if err := os.WriteFile(dataFile, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	opts := UploadOptions{
		DatasetName:   "acme-export-2026-09-25",
		Description:   "Export (full) for Acme - ringboost + acme",
		Category:      "phone-numbers",
		DataFreshness: "daily",
		Metadata: map[string]any{
			"export_type":       "full",
			"consumer":          "acme",
			"consumer_name":     "Acme",
			"ringboost_count":   30,
			"additional_count":  10,
			"total_count":       40,
			"export_date":       time.Now().Format(time.RFC3339),
			"source_service":    "ringboost-vendor",
			"source_query_hash": "abc123",
			"file_format":       "json",
			"encoding":          "utf-8",
		},
		DatasetOverrides: map[string]any{
			"version":       "2026-09-25",
			"original_size": int64(len(plaintext)),
			"record_count":  40,
		},
		Encrypt:          true,
		Compress:         true,
		CompressionLevel: 6,
	}

	ds, err := f.p.UploadDataset(context.Background(), dataFile, opts)
	if err != nil {
		t.Fatalf("UploadDataset with Ringboost's options: %v", err)
	}
	if ds == nil || ds.ID != f.datasetIDBack {
		t.Fatalf("returned dataset = %+v, want the server-assigned id %q", ds, f.datasetIDBack)
	}

	if got := strings.Join(f.order, ","); got != "POST,PUT,GET" {
		t.Fatalf("call order = %s, want POST,PUT,GET (record, then object, then read-back)", got)
	}

	// What Ringboost's call always sent.
	for key, want := range map[string]any{
		"name":           "acme-export-2026-09-25",
		"category":       "phone-numbers",
		"data_freshness": "daily",
		"producer_id":    "test-producer",
		"s3_key":         "datasets/acme-export-2026-09-25/data.ndjson.gz",
		"version":        "2026-09-25",
		"record_count":   float64(40),
		"original_size":  float64(len(plaintext)),
		"visibility":     "private",
	} {
		if f.postBody[key] != want {
			t.Errorf("POST body %s = %v, want %v", key, f.postBody[key], want)
		}
	}
	md, _ := f.postBody["metadata"].(map[string]any)
	if md["export_type"] != "full" || md["source_service"] != "ringboost-vendor" || md["consumer"] != "acme" {
		t.Errorf("caller metadata keys were not forwarded: %v", md)
	}

	// R1: the record always says encrypted and compressed.
	if md["encryption_enabled"] != true || md["compression_enabled"] != true {
		t.Errorf("metadata.encryption_enabled/compression_enabled = %v/%v, want true/true", md["encryption_enabled"], md["compression_enabled"])
	}

	// R4: ids are the server's job; the SDK sends none of its own.
	for _, key := range []string{"id", "_id", "dataset_id"} {
		if _, present := f.postBody[key]; present {
			t.Errorf("POST body carries a client-generated %q: %v", key, f.postBody[key])
		}
	}

	// The stored object is gzip inside the encryption envelope, and its length
	// is what the record reports.
	if got := f.openUploaded(t); !bytes.Equal(got, plaintext) {
		t.Fatalf("stored object decrypts+gunzips to %d bytes, want the original %d", len(got), len(plaintext))
	}
	if f.postBody["size_bytes"] != float64(len(f.uploaded)) {
		t.Errorf("size_bytes = %v, want %d", f.postBody["size_bytes"], len(f.uploaded))
	}
}

// TestUploadDataset_RecordFlagsSurviveAnOverrideThatReplacesMetadata: a
// DatasetOverrides "metadata" object REPLACES the computed metadata wholesale,
// which used to drop encryption_enabled/compression_enabled from the record
// altogether — a record that says nothing about a file that is in fact
// encrypted and compressed. The flags are pinned after the merge.
func TestUploadDataset_RecordFlagsSurviveAnOverrideThatReplacesMetadata(t *testing.T) {
	f := newUploadFixture(t)

	opts := NewUploadOptions("replaces-metadata")
	opts.DatasetOverrides = map[string]any{"metadata": map[string]any{"owner_note": "kept"}}

	if _, err := f.p.UploadDataset(context.Background(), writeNDJSON(t, 3), opts); err != nil {
		t.Fatalf("UploadDataset: %v", err)
	}

	md, _ := f.postBody["metadata"].(map[string]any)
	if md["owner_note"] != "kept" {
		t.Errorf("the override's own metadata keys must still be sent: %v", md)
	}
	if md["encryption_enabled"] != true || md["compression_enabled"] != true {
		t.Errorf("metadata flags = %v/%v after a metadata override, want true/true", md["encryption_enabled"], md["compression_enabled"])
	}
}

// TestCreateDatasetRecord_FlagsAreAlwaysTrue: even a processed result that
// (wrongly) reports a flag as false cannot make the record say it — the record
// mirrors the invariant, not the input.
func TestCreateDatasetRecord_FlagsAreAlwaysTrue(t *testing.T) {
	f := newUploadFixture(t)

	processed := &ProcessedFileData{
		Data:         []byte("bytes-to-put"),
		OriginalSize: 10,
		Sizes: map[string]any{
			"original_size_bytes":   int64(10),
			"compressed_size_bytes": int64(12),
			"encrypted_size_bytes":  int64(12),
			"encryption_enabled":    false,
			"compression_enabled":   false,
		},
	}
	if _, err := f.p.createDatasetRecord(context.Background(), writeNDJSON(t, 2), NewUploadOptions("flags-true"), processed); err != nil {
		t.Fatalf("createDatasetRecord: %v", err)
	}

	md, _ := f.postBody["metadata"].(map[string]any)
	if md["encryption_enabled"] != true || md["compression_enabled"] != true {
		t.Errorf("metadata flags = %v/%v, want true/true whatever the processed sizes said", md["encryption_enabled"], md["compression_enabled"])
	}
	if f.postBody["s3_key"] != "datasets/flags-true/data.ndjson.gz" {
		t.Errorf("s3_key = %v, want the .gz name (uploads are always compressed)", f.postBody["s3_key"])
	}
}

// TestProcessFile_ResultAlwaysSaysEncryptedAndCompressed: the sizes a real
// processFile pass hands to the record carry both flags as true, and the bytes
// it returns are the gzip-then-envelope object, not the input.
func TestProcessFile_ResultAlwaysSaysEncryptedAndCompressed(t *testing.T) {
	f := newUploadFixture(t)
	plaintext := []byte(strings.Repeat(`{"a":1}`+"\n", 50))
	dataFile := filepath.Join(t.TempDir(), "in.ndjson")
	if err := os.WriteFile(dataFile, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := f.p.processFile(context.Background(), dataFile, NewUploadOptions("p"))
	if err != nil {
		t.Fatalf("processFile: %v", err)
	}
	if got.Sizes["encryption_enabled"] != true || got.Sizes["compression_enabled"] != true {
		t.Errorf("sizes flags = %v/%v, want true/true", got.Sizes["encryption_enabled"], got.Sizes["compression_enabled"])
	}
	if bytes.Contains(got.Data, plaintext[:20]) {
		t.Error("the processed bytes contain the plaintext")
	}
	if got.Sizes["original_size_bytes"] != int64(len(plaintext)) || got.Sizes["encrypted_size_bytes"] != int64(len(got.Data)) {
		t.Errorf("sizes = %v, want original %d and encrypted %d", got.Sizes, len(plaintext), len(got.Data))
	}
}

// TestEncryptData_WithoutKMSClientIsAnError: a Producer with a key id but no
// KMS client cannot encrypt — that is an error, never a plaintext upload and
// never a nil-pointer panic.
func TestEncryptData_WithoutKMSClientIsAnError(t *testing.T) {
	p := &Producer{KMSKeyID: "some-key"}

	if _, err := p.encryptData(context.Background(), []byte("x")); err == nil || !strings.Contains(err.Error(), "KMS client") {
		t.Fatalf("encryptData error = %v, want a missing-KMS-client error", err)
	}
}

// TestResolveKMSKeyID: a producer whose KMS key cannot be resolved is built
// without one (it can still do non-upload work) but says plainly that uploads
// will fail — the old message claimed encryption "will be disabled", which is
// exactly what can no longer happen.
func TestResolveKMSKeyID(t *testing.T) {
	t.Setenv("HELIX_SSM_CUSTOMER_PREFIX", "")
	t.Setenv("HELIX_ENVIRONMENT", "")
	t.Setenv("ENVIRONMENT", "")

	t.Run("found", func(t *testing.T) {
		names := ssmParamCandidates("cust-1", "kms_key_id")
		f := newFakeSSM(t, map[string]string{names[0]: "found:key-123"})

		got, err := resolveKMSKeyID(context.Background(), f.client(), "cust-1")
		if err != nil || got != "key-123" {
			t.Fatalf("resolveKMSKeyID = %q, %v; want key-123, nil", got, err)
		}
	})

	for name, script := range map[string]string{"not found": "notfound", "access denied": "denied"} {
		t.Run(name, func(t *testing.T) {
			names := ssmParamCandidates("cust-1", "kms_key_id")
			f := newFakeSSM(t, map[string]string{names[0]: script})

			got, err := resolveKMSKeyID(context.Background(), f.client(), "cust-1")
			if got != "" || err == nil {
				t.Fatalf("resolveKMSKeyID = %q, %v; want an empty key and an error", got, err)
			}
			if !strings.Contains(err.Error(), "uploads will fail") {
				t.Errorf("error %q must say uploads will fail", err)
			}
			if strings.Contains(err.Error(), "disabled") {
				t.Errorf("error %q must not claim encryption is disabled", err)
			}
			if strings.Contains(err.Error(), "/helix-tools/") {
				t.Errorf("error %q prints an internal parameter path", err)
			}
		})
	}
}

// Compile-time guard: the option fields callers already use keep their names
// and types (source compatibility) even though false is no longer accepted.
var _ = UploadOptions{Encrypt: true, Compress: true, CompressionLevel: 6, DataFreshness: types.DataFreshnessDaily}

// TestUploadDataset_KeyServiceOutageUploadsNothing: if KMS cannot encrypt the
// data key, the upload fails before the catalog record is created and before
// any byte is PUT — a key-service outage never turns into a plaintext upload.
func TestUploadDataset_KeyServiceOutageUploadsNothing(t *testing.T) {
	var apiRequests atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiRequests.Add(1)
	}))
	defer api.Close()

	kmsDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"denied"}`))
	}))
	defer kmsDown.Close()

	p := newTestProducerWithKMS(api.URL, kmsDown.URL)

	_, err := p.UploadDataset(context.Background(), writeNDJSON(t, 3), NewUploadOptions("kms-down"))
	if err == nil || !strings.Contains(err.Error(), "encryption failed") {
		t.Fatalf("error = %v, want an encryption failure", err)
	}
	if got := apiRequests.Load(); got != 0 {
		t.Fatalf("%d API request(s) after the encryption failure; want 0 (no record, no upload)", got)
	}
}

// TestMetadataObject covers the shapes a metadata value can take: nil is an
// empty object, a caller's map is copied (never aliased), and anything that
// cannot be an object is an error.
func TestMetadataObject(t *testing.T) {
	t.Run("nil is an empty object", func(t *testing.T) {
		got, err := metadataObject(nil)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("metadataObject(nil) = %v, %v; want an empty, non-nil map", got, err)
		}
	})

	t.Run("a map is copied, not aliased", func(t *testing.T) {
		in := map[string]any{"k": "v"}
		got, err := metadataObject(in)
		if err != nil {
			t.Fatal(err)
		}
		got["added"] = true
		if _, leaked := in["added"]; leaked {
			t.Fatal("metadataObject returned the caller's own map; writes would leak into it")
		}
	})

	t.Run("a value that cannot be encoded is an error", func(t *testing.T) {
		if _, err := metadataObject(make(chan int)); err == nil || !strings.Contains(err.Error(), "must be an object") {
			t.Fatalf("error = %v, want a must-be-an-object error", err)
		}
	})
}

// TestCreateDatasetRecord_RefusesNonObjectMetadataOverride: createDatasetRecord
// is reached only through UploadDataset, which validates first — but the pin
// after the merge must not silently swallow a metadata override that is not an
// object either.
func TestCreateDatasetRecord_RefusesNonObjectMetadataOverride(t *testing.T) {
	f := newUploadFixture(t)
	opts := NewUploadOptions("bad-metadata")
	opts.DatasetOverrides = map[string]any{"metadata": []string{"encryption_enabled"}}

	processed := &ProcessedFileData{Data: []byte("x"), Sizes: map[string]any{}}
	if _, err := f.p.createDatasetRecord(context.Background(), writeNDJSON(t, 2), opts, processed); err == nil || !strings.Contains(err.Error(), "must be an object") {
		t.Fatalf("error = %v, want a must-be-an-object error", err)
	}
	if len(f.order) != 0 {
		t.Fatalf("calls = %v, want none: the record must not be created", f.order)
	}
}

// TestUploadDataset_NilMetadataOverrideStillCarriesFlags: an explicit nil
// metadata override (JSON null) is not a way to send a record without flags.
func TestUploadDataset_NilMetadataOverrideStillCarriesFlags(t *testing.T) {
	f := newUploadFixture(t)
	opts := NewUploadOptions("nil-metadata")
	opts.DatasetOverrides = map[string]any{"metadata": nil}

	if _, err := f.p.UploadDataset(context.Background(), writeNDJSON(t, 3), opts); err != nil {
		t.Fatalf("UploadDataset: %v", err)
	}
	md, _ := f.postBody["metadata"].(map[string]any)
	if md["encryption_enabled"] != true || md["compression_enabled"] != true {
		t.Errorf("metadata = %v, want both flags true", f.postBody["metadata"])
	}
}
