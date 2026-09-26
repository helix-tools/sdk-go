package consumer

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// testDataKey is the 32-byte AES-256 data key the fake KMS "unwraps" for every
// Decrypt call. The wrapped key inside a test object is an opaque blob: the
// fake KMS ignores it and always answers with this key.
var testDataKey = []byte("0123456789abcdef0123456789abcdef")

const kmsDecryptTarget = "TrentService.Decrypt"

// gzipBytes compresses b the way the producer does (compress FIRST).
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// sealEnvelope wraps data in the producer's encryption envelope:
// [4 bytes big-endian key length][wrapped key][16 IV][16 tag][ciphertext].
func sealEnvelope(data []byte) []byte {
	block, err := aes.NewCipher(testDataKey)
	if err != nil {
		panic(err)
	}
	aesGCM, err := cipher.NewGCMWithNonceSize(block, 16)
	if err != nil {
		panic(err)
	}
	iv := []byte("iv-iv-iv-iv-iv-1") // 16 bytes
	sealed := aesGCM.Seal(nil, iv, data, nil)
	tagAt := len(sealed) - aesGCM.Overhead()
	wrappedKey := []byte("wrapped-data-key-blob")

	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, uint32(len(wrappedKey)))
	out.Write(wrappedKey)
	out.Write(iv)
	out.Write(sealed[tagAt:])
	out.Write(sealed[:tagAt])
	return out.Bytes()
}

// encryptedObject is the exact object a conforming upload leaves in storage:
// gzip first, then the envelope.
func encryptedObject(plaintext []byte) []byte {
	return sealEnvelope(gzipBytes(plaintext))
}

// fakeKMSDecrypt answers a KMS Decrypt request with testDataKey. It reports
// false when the request is not a KMS Decrypt call.
func fakeKMSDecrypt(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || r.Header.Get("X-Amz-Target") != kmsDecryptTarget {
		return false
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"Plaintext": base64.StdEncoding.EncodeToString(testDataKey),
		"KeyId":     "test-kms-key",
	})
	return true
}

// kmsDecryptBody is the JSON a KMS Decrypt reply carries, for round trippers.
func kmsDecryptBody() string { return kmsDecryptBodyFor(testDataKey) }

// kmsDecryptBodyFor is a KMS Decrypt reply that unwraps to key.
func kmsDecryptBodyFor(key []byte) string {
	b, _ := json.Marshal(map[string]string{
		"Plaintext": base64.StdEncoding.EncodeToString(key),
		"KeyId":     "test-kms-key",
	})
	return string(b)
}

// useFakeKMS points c's KMS client at a fake that unwraps every data key to
// testDataKey. baseURL is a server that serves fakeKMSDecrypt, or "" when
// client already routes KMS traffic (a custom round tripper).
func useFakeKMS(c *Consumer, baseURL string, client aws.HTTPClient) *Consumer {
	c.kmsClient = kms.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
	}, func(o *kms.Options) {
		if baseURL != "" {
			o.BaseEndpoint = aws.String(baseURL)
		}
		if client != nil {
			o.HTTPClient = client
		}
	})
	return c
}
