package producer

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cipherSetupCause is an upstream cipher failure whose text carries a cloud
// resource identifier with an account number, the kind of detail that must
// never reach a customer message.
var cipherSetupCause = errors.New("cipher: rejected the nonce for " + arnAccountService)

// withCipherSetupFailure makes the AEAD constructor fail with cause until the
// test ends. The real constructor cannot fail for the fixed 16-byte nonce.
func withCipherSetupFailure(t *testing.T, cause error) {
	t.Helper()

	prev := newGCMWithNonceSize
	newGCMWithNonceSize = func(cipher.Block, int) (cipher.AEAD, error) { return nil, cause }
	t.Cleanup(func() { newGCMWithNonceSize = prev })
}

func TestEncryptData_CipherSetupCauseNeverLeaksIntoMessage(t *testing.T) {
	withCipherSetupFailure(t, cipherSetupCause)
	p := newTestProducerWithKMS("https://api.test", "https://endpoint.invalid")

	_, err := p.encryptData(context.Background(), []byte("compressed-bytes"))

	assertClean(t, err, "failed to prepare encryption")
	assertCauseReachable(t, err, arnAccountService)
	if !errors.Is(err, cipherSetupCause) {
		t.Fatal("errors.Is(err, cause) = false, want true — the cause must stay reachable")
	}
}

// TestEncryptData_CipherSetupNegativeControl builds the pre-fix shape (fmt.Errorf
// with %w) from the same cause and confirms the leak, so the assertions above
// are shown to fail for the right reason on the old wording.
func TestEncryptData_CipherSetupNegativeControl(t *testing.T) {
	legacy := fmt.Errorf("failed to prepare encryption: %w", cipherSetupCause)

	if !strings.Contains(legacy.Error(), arnAccountService) {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}
