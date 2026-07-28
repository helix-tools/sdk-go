package credentials

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestRetryJitterDoesNotUseWeakRandomness(t *testing.T) {
	source, err := os.ReadFile("broker.go")
	if err != nil {
		t.Fatalf("read broker.go: %v", err)
	}
	code := string(source)

	if strings.Contains(code, `"math/rand"`) {
		t.Fatal("retry jitter still imports math/rand")
	}
	if !strings.Contains(code, `"crypto/rand"`) {
		t.Fatal("retry jitter does not import crypto/rand")
	}
	if strings.Contains(code, "nolint:gosec") {
		t.Fatal("stale gosec suppression remains")
	}
}

func TestBackoffDelayUsesExpectedJitterRange(t *testing.T) {
	for attempt := 1; attempt < mintMaxAttempts; attempt++ {
		base := mintRetryBaseDelay * time.Duration(int64(1)<<uint(attempt-1))
		minimum := base - base/4
		maximum := base + base/4

		for sample := 0; sample < 100; sample++ {
			delay, err := backoffDelay(attempt)
			if err != nil {
				t.Fatalf("backoffDelay(%d): %v", attempt, err)
			}
			if delay < minimum || delay >= maximum {
				t.Fatalf("backoffDelay(%d) = %v, want [%v, %v)", attempt, delay, minimum, maximum)
			}
		}
	}
}

func TestBackoffDelayFromDeterministicEntropy(t *testing.T) {
	minimum, err := backoffDelayFrom(bytes.NewReader(make([]byte, 8)), 1)
	if err != nil {
		t.Fatalf("minimum jitter: %v", err)
	}
	if want := mintRetryBaseDelay * 3 / 4; minimum != want {
		t.Fatalf("minimum jitter delay = %v, want %v", minimum, want)
	}

	maximumBytes := bytes.Repeat([]byte{0xff}, 8)
	maximum, err := backoffDelayFrom(bytes.NewReader(maximumBytes), 1)
	if err != nil {
		t.Fatalf("maximum jitter: %v", err)
	}
	if lower, upper := mintRetryBaseDelay, mintRetryBaseDelay*5/4; maximum < lower || maximum >= upper {
		t.Fatalf("maximum jitter delay = %v, want [%v, %v)", maximum, lower, upper)
	}

	entropyErr := errors.New("entropy unavailable")
	if _, err := backoffDelayFrom(iotest.ErrReader(entropyErr), 1); !errors.Is(err, entropyErr) {
		t.Fatalf("entropy error = %v, want %v", err, entropyErr)
	}
}
