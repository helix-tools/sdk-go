package consumer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// If the credential service echoes the request's Authorization header (the
// raw Helix API key, or a replayable SigV4 signature) back in an error body,
// NewConsumer's error, every error it wraps and anything printed while it
// runs must not carry it.
func TestNewConsumer_EchoedCredentialSecretsNeverLeak(t *testing.T) {
	cases := map[string]types.Config{
		"sigv4": {
			AWSAccessKeyID: "AKIAIOSFODNN7EXAMPLE", AWSSecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			CredentialMode: types.CredentialModeSTS, Region: "us-east-1",
		},
		"api key": {APIKey: "hlx_shortTESTkeyEXAMPLE"},
	}
	for name, cfg := range cases {
		for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
			t.Run(name+"/"+http.StatusText(status), func(t *testing.T) {
				identity, _ := countingIdentityServer(t, http.StatusOK)
				isolateConsumerAWSEnv(t, identity.URL)
				service, sent := echoCredentialService(t, status)
				cfg.APIEndpoint = service.URL
				cfg.CustomerID = "cons-1"

				var err error
				output := captureStdio(t, func() { _, err = NewConsumer(cfg) })

				assertNoEchoedSecrets(t, err, output, sent(), cfg.APIKey)
			})
		}
	}
}

// echoCredentialService answers every request with status and an error
// envelope whose every field echoes the request's Authorization and
// X-Amz-Security-Token values; sent returns the last request's headers.
func echoCredentialService(t *testing.T, status int) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var last http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		echo := "rejected " + r.Header.Get("Authorization") + " token " + r.Header.Get("X-Amz-Security-Token")
		body, _ := json.Marshal(map[string]any{
			"message": echo,
			"error":   map[string]string{"code": echo, "message": echo, "request_id": echo},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func captureStdio(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()
	defer func() { os.Stdout, os.Stderr = oldStdout, oldStderr }()
	fn()
	_ = w.Close()
	return string(<-done)
}

func assertNoEchoedSecrets(t *testing.T, err error, output string, sent http.Header, apiKey string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a credential error")
	}
	if sent == nil {
		t.Fatal("the credential service never received a request")
	}
	auth := sent.Get("Authorization")
	secrets := []string{auth}
	if i := strings.LastIndex(auth, "Signature="); i >= 0 {
		secrets = append(secrets, auth[i+len("Signature="):])
	}
	for _, s := range []string{sent.Get("X-Amz-Security-Token"), apiKey} {
		if s != "" {
			secrets = append(secrets, s)
		}
	}
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		for _, secret := range secrets {
			if strings.Contains(e.Error(), secret) {
				t.Errorf("error chain (%T) leaks %q: %q", e, secret, e.Error())
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(output, secret) {
			t.Errorf("printed output leaks %q: %q", secret, output)
		}
	}
	if !strings.Contains(errors.Unwrap(err).Error(), "<redacted>") {
		t.Errorf("cause = %q, want the echoed secret replaced by <redacted>", errors.Unwrap(err).Error())
	}
}
