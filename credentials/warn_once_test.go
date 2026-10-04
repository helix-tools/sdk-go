package credentials

import (
	"sync"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// TestWarn_PrintsEachMessageOncePerProcess: a client built over and over with
// the same conflicting configuration must not repeat its warning. The README
// promises a one-time warning, so SelectProvider prints it on the first call
// only.
func TestWarn_PrintsEachMessageOncePerProcess(t *testing.T) {
	buf := captureWarnings(t)

	both := types.Config{
		Region:             testRegion,
		APIKey:             testAPIKey,
		AWSAccessKeyID:     "AKIATESTKEY",
		AWSSecretAccessKey: "testSecret",
	}
	staticWithKey := both
	staticWithKey.CredentialMode = types.CredentialModeStatic

	for i := 0; i < 3; i++ {
		if _, err := SelectProvider("https://api-go.helix.tools", both); err != nil {
			t.Fatalf("SelectProvider(both): %v", err)
		}
		if _, err := SelectProvider("https://api-go.helix.tools", staticWithKey); err != nil {
			t.Fatalf("SelectProvider(static with key): %v", err)
		}
	}

	if n := countOccurrences(buf.String(), warnMsgStaticFieldsIgnoredWhenKeySet); n != 1 {
		t.Errorf("%q printed %d times over 3 constructions, want 1 (output: %q)", warnMsgStaticFieldsIgnoredWhenKeySet, n, buf.String())
	}
	if n := countOccurrences(buf.String(), warnMsgKeyFieldIgnoredInStaticMode); n != 1 {
		t.Errorf("%q printed %d times over 3 constructions, want 1 (output: %q)", warnMsgKeyFieldIgnoredInStaticMode, n, buf.String())
	}
}

// TestWarn_ConcurrentConstructionsPrintOnce: the once-per-process record is
// shared, so simultaneous constructions must still print a message once and
// race-free (the suite runs under -race in CI).
func TestWarn_ConcurrentConstructionsPrintOnce(t *testing.T) {
	buf := captureWarnings(t)

	cfg := types.Config{
		Region:             testRegion,
		APIKey:             testAPIKey,
		AWSAccessKeyID:     "AKIATESTKEY",
		AWSSecretAccessKey: "testSecret",
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = SelectProvider("https://api-go.helix.tools", cfg)
		}()
	}
	wg.Wait()

	if n := countOccurrences(buf.String(), warnMsgStaticFieldsIgnoredWhenKeySet); n != 1 {
		t.Errorf("warning printed %d times across 16 concurrent constructions, want 1", n)
	}
}
