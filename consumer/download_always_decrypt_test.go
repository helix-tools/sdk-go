package consumer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// objectTransport is a hermetic stand-in for the whole download path: the
// dataset record, the presigned-URL response, the object itself and KMS. It
// counts KMS Decrypt calls so a test can prove a malformed object is refused
// BEFORE anything is sent to KMS.
type objectTransport struct {
	record     string // dataset record JSON
	object     []byte // bytes stored behind the presigned URL
	claimLarge bool   // advertise a >100 MB body so the streaming path runs
	kmsCalls   atomic.Int32
	kmsDenied  bool   // KMS answers Decrypt with AccessDenied
	kmsKey     []byte // data key KMS returns; nil = the valid 32-byte test key
}

func (o *objectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, contentLength := "{}", int64(-1)

	switch {
	case req.Header.Get("X-Amz-Target") == kmsDecryptTarget:
		o.kmsCalls.Add(1)
		switch {
		case o.kmsDenied:
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
				Header:     http.Header{"X-Amzn-Errortype": []string{"AccessDeniedException"}},
				Body:       io.NopCloser(strings.NewReader(`{"__type":"AccessDeniedException","message":"denied"}`)),
				Request:    req,
			}, nil
		case o.kmsKey != nil:
			body = kmsDecryptBodyFor(o.kmsKey)
		default:
			body = kmsDecryptBody()
		}
	case req.URL.Path == "/object":
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        make(http.Header),
			Body:          io.NopCloser(bytes.NewReader(o.object)),
			ContentLength: o.contentLength(),
			Request:       req,
		}, nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/download"):
		body = `{"download_url":"https://objects.test/object"}`
	case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/v1/datasets/"):
		body = o.record
	}

	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: contentLength,
		Request:       req,
	}, nil
}

func (o *objectTransport) contentLength() int64 {
	if o.claimLarge {
		return 100*1024*1024 + 1
	}
	return int64(len(o.object))
}

func (o *objectTransport) consumer() *Consumer {
	client := &http.Client{Transport: o}
	c := newTestConsumer("https://objects.test")
	c.httpClient = client
	return useFakeKMS(c, "", client)
}

const recordFlagsFalse = `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":false,"encryption_enabled":false}}`

var downloadPaths = []struct {
	name       string
	claimLarge bool
}{
	{"small in-memory path", false},
	{"large streaming path", true},
}

// TestDownloadDataset_AlwaysDecryptsAndDecompresses is R2: whatever the
// dataset record claims, the object is decrypted and decompressed. A record
// that says "not encrypted" / "not compressed" (or says nothing) is never a
// licence to hand back raw bytes.
func TestDownloadDataset_AlwaysDecryptsAndDecompresses(t *testing.T) {
	plaintext := []byte(`{"phone":"+15550100"}` + "\n" + `{"phone":"+15550101"}` + "\n")

	records := map[string]string{
		"record says neither flag":   recordFlagsFalse,
		"record says nothing":        `{"_id":"ds-1","name":"n"}`,
		"top-level encryption false": `{"_id":"ds-1","name":"n","encryption":false,"metadata":{}}`,
		"record says both flags":     `{"_id":"ds-1","name":"n","metadata":{"compression_enabled":true,"encryption_enabled":true}}`,
		"promoted encryption flag":   `{"_id":"ds-1","name":"n","encryption":true,"metadata":{"compression_enabled":true}}`,
	}

	for _, path := range downloadPaths {
		for name, record := range records {
			t.Run(path.name+"/"+name, func(t *testing.T) {
				tr := &objectTransport{record: record, object: encryptedObject(plaintext), claimLarge: path.claimLarge}
				out := filepath.Join(t.TempDir(), "out.ndjson")

				if err := tr.consumer().DownloadDataset(context.Background(), "ds-1", out); err != nil {
					t.Fatalf("DownloadDataset: %v", err)
				}
				got, err := os.ReadFile(out)
				if err != nil {
					t.Fatalf("read output: %v", err)
				}
				if !bytes.Equal(got, plaintext) {
					t.Fatalf("output = %q, want the decrypted, decompressed plaintext %q", got, plaintext)
				}
				if tr.kmsCalls.Load() != 1 {
					t.Fatalf("KMS Decrypt calls = %d, want 1 (the object must always be decrypted)", tr.kmsCalls.Load())
				}
			})
		}
	}
}

// TestDownloadDataset_RejectsObjectNotEncryptedAndCompressed is R2's other
// half: an object that is not encrypted, or not compressed, is an error and
// no file is written. The record's flags are irrelevant to the outcome — they
// say "not encrypted, not compressed" here, which used to make the SDK return
// the raw bytes as if they were the dataset.
func TestDownloadDataset_RejectsObjectNotEncryptedAndCompressed(t *testing.T) {
	plaintext := []byte(`{"id":"ds-1","secret":"not for the disk"}` + "\n")

	// A valid-looking envelope header whose body is cut short.
	truncated := encryptedObject(plaintext)[:20]

	tampered := encryptedObject(plaintext)
	tampered[len(tampered)-1] ^= 0xff

	zeroKeyLen := append([]byte{0, 0, 0, 0}, bytes.Repeat([]byte{7}, 64)...)

	cases := []struct {
		name    string
		object  []byte
		want    error  // errors.Is target, when the failure is structural
		wantMsg string // substring, otherwise
		wantKMS int32  // KMS Decrypt calls that are acceptable
	}{
		{"plaintext ndjson", plaintext, errNotEncrypted, "", 0},
		{"plaintext shorter than the header", []byte("hi"), errNotEncrypted, "", 0},
		{"zero-byte object", []byte{}, errNotEncrypted, "", 0},
		{"gzip only, never encrypted", gzipBytes(plaintext), errNotEncrypted, "", 0},
		{"envelope with a zero-length wrapped key", zeroKeyLen, errNotEncrypted, "", 0},
		{"envelope cut short", truncated, errNotEncrypted, "", 0},
		{"encrypted but never compressed", sealEnvelope(plaintext), errNotCompressed, "", 1},
		{"ciphertext tampered with", tampered, nil, "AES-GCM decrypt failed", 1},
	}

	for _, path := range downloadPaths {
		for _, tc := range cases {
			t.Run(path.name+"/"+tc.name, func(t *testing.T) {
				tr := &objectTransport{record: recordFlagsFalse, object: tc.object, claimLarge: path.claimLarge}
				out := filepath.Join(t.TempDir(), "out.ndjson")

				err := tr.consumer().DownloadDataset(context.Background(), "ds-1", out)
				if err == nil {
					got, _ := os.ReadFile(out)
					t.Fatalf("DownloadDataset returned nil; it must refuse this object (wrote %q)", got)
				}
				if tc.want != nil && !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want it to wrap %q", err, tc.want)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantMsg)
				}
				if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
					t.Fatalf("output file exists after a refused download (stat err = %v)", statErr)
				}
				if got := tr.kmsCalls.Load(); got != tc.wantKMS {
					t.Fatalf("KMS Decrypt calls = %d, want %d (a malformed object must be refused before KMS)", got, tc.wantKMS)
				}
			})
		}
	}
}

// TestDecryptData_HugeKeyLengthDoesNotAllocate is the bypass test for the
// plaintext-object check. A plaintext object's first four bytes are read as
// the wrapped-key length: `{"id` is 0x7b226964, about 2 GB. The reader must
// reject it from the bytes it already holds instead of allocating a buffer of
// that size first.
func TestDecryptData_HugeKeyLengthDoesNotAllocate(t *testing.T) {
	c := useFakeKMS(newTestConsumer("https://objects.test"), "", nil)

	objects := map[string][]byte{
		"plaintext json": []byte(`{"id":"ds-1"}` + strings.Repeat(" ", 128)),
		"max uint32":     append([]byte{0xff, 0xff, 0xff, 0xff}, make([]byte, 128)...),
		"just above the KMS ciphertext limit": append(
			binary.BigEndian.AppendUint32(nil, maxWrappedKeyLen+1),
			make([]byte, maxWrappedKeyLen+64)...),
	}

	for name, object := range objects {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := c.decryptData(context.Background(), object)
			runtime.ReadMemStats(&after)

			if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
				t.Fatalf("decryptData allocated %d bytes for a %d-byte object; the header length must be validated before any allocation", grew, len(object))
			}
			if !errors.Is(err, errNotEncrypted) {
				t.Fatalf("decryptData error = %v, want errNotEncrypted", err)
			}
		})
	}
}

// TestDecryptData_WithoutKMSClientIsAnError: a Consumer built by hand has no
// KMS client. Decryption is mandatory, so that is an error, not a nil-pointer
// panic and not a skipped step.
func TestDecryptData_WithoutKMSClientIsAnError(t *testing.T) {
	c := &Consumer{}

	_, err := c.decryptData(context.Background(), encryptedObject([]byte("x")))
	if err == nil || !strings.Contains(err.Error(), "KMS client") {
		t.Fatalf("decryptData error = %v, want a missing-KMS-client error", err)
	}
}

// TestDownloadOutcome_RefusedObject_ReportsCategory: a refused object is
// reported through the outcome callback like any other download failure —
// kms_decrypt for a bad envelope, decompress for a missing gzip layer.
func TestDownloadOutcome_RefusedObject_ReportsCategory(t *testing.T) {
	plaintext := []byte("just some plaintext rows\n")

	cases := []struct {
		name         string
		object       []byte
		wantCategory string
	}{
		{"plaintext object", plaintext, "kms_decrypt"},
		{"encrypted, not compressed", sealEnvelope(plaintext), "decompress"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.s3Body = tc.object
			c := newTestConsumer(f.server.URL)

			out := filepath.Join(t.TempDir(), "out.bin")
			if err := c.DownloadDataset(context.Background(), "ds-1", out); err == nil {
				t.Fatal("DownloadDataset must refuse the object")
			}
			if !waitForCallback(f, 1, 2*time.Second) {
				t.Fatal("outcome callback never fired")
			}

			p := callbackPayload(f)
			if p["status"] != "error" || p["error_category"] != tc.wantCategory {
				t.Fatalf("callback = status %v category %v, want error / %s", p["status"], p["error_category"], tc.wantCategory)
			}
		})
	}
}

// TestDownloadDataset_KeyServiceFailuresAreErrors: when KMS refuses to unwrap
// the data key, or hands back a key that cannot decrypt, the download fails —
// it never falls back to returning the stored bytes.
func TestDownloadDataset_KeyServiceFailuresAreErrors(t *testing.T) {
	cases := []struct {
		name    string
		tr      *objectTransport
		wantMsg string
	}{
		{
			"KMS denies the request",
			&objectTransport{record: recordFlagsFalse, object: encryptedObject([]byte("rows\n")), kmsDenied: true},
			"KMS decrypt failed",
		},
		{
			"KMS returns a key of the wrong size",
			&objectTransport{record: recordFlagsFalse, object: encryptedObject([]byte("rows\n")), kmsKey: []byte("short")},
			"invalid key size",
		},
		{
			"KMS returns a different valid key",
			&objectTransport{record: recordFlagsFalse, object: encryptedObject([]byte("rows\n")), kmsKey: []byte("ffffffffffffffffffffffffffffffff")},
			"AES-GCM decrypt failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.ndjson")

			err := tc.tr.consumer().DownloadDataset(context.Background(), "ds-1", out)
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantMsg)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("output file exists after a failed decryption (stat err = %v)", statErr)
			}
		})
	}
}
