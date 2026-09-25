package producer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// ----------------------------------------------------------------------------
// A-21: SSM lookup tries only real prefixes and fails closed on real errors.
// ----------------------------------------------------------------------------

func TestSSMParamCandidates_OnlyTheRealPrefix(t *testing.T) {
	t.Setenv("HELIX_SSM_CUSTOMER_PREFIX", "")
	t.Setenv("HELIX_ENVIRONMENT", "")
	t.Setenv("ENVIRONMENT", "")

	got := ssmParamCandidates("cust-1", "s3_bucket")
	want := []string{"/helix-tools/production/customers/cust-1/s3_bucket"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %v, want exactly %v (the dead /helix/... prefixes are gone)", got, want)
	}
}

func TestSSMParamCandidates_OverrideAndEnvironment(t *testing.T) {
	t.Setenv("HELIX_SSM_CUSTOMER_PREFIX", "/custom/customers/")
	t.Setenv("HELIX_ENVIRONMENT", "staging")
	t.Setenv("ENVIRONMENT", "ignored")

	got := ssmParamCandidates("cust-1", "kms_key_id")
	want := []string{
		"/custom/customers/cust-1/kms_key_id",
		"/helix-tools/staging/customers/cust-1/kms_key_id",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %v, want %v (override first, trailing slash trimmed, HELIX_ENVIRONMENT wins)", got, want)
	}

	// An override equal to the default prefix is not tried twice.
	t.Setenv("HELIX_SSM_CUSTOMER_PREFIX", "/helix-tools/staging/customers")
	if got := ssmParamCandidates("cust-1", "kms_key_id"); len(got) != 1 {
		t.Errorf("candidates = %v, want the duplicate collapsed to one", got)
	}

	t.Setenv("HELIX_ENVIRONMENT", "")
	t.Setenv("HELIX_SSM_CUSTOMER_PREFIX", "")
	if got := ssmParamCandidates("cust-1", "x"); !strings.Contains(got[0], "/helix-tools/ignored/customers/") {
		t.Errorf("ENVIRONMENT fallback not honoured: %v", got)
	}

	if ssmParamCandidates("", "x") != nil || ssmParamCandidates("c", "") != nil {
		t.Error("empty customer id or param name must yield no candidates")
	}
}

// fakeSSM answers GetParameter per parameter name from a script and records the
// names requested, in order.
type fakeSSM struct {
	srv *httptest.Server

	mu        sync.Mutex
	requested []string
}

// script maps a requested Name to "found:<value>", "novalue", "notfound" or "denied".
func newFakeSSM(t *testing.T, script map[string]string) *fakeSSM {
	t.Helper()
	f := &fakeSSM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct{ Name string }
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		f.requested = append(f.requested, req.Name)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		action := script[req.Name]
		switch {
		case strings.HasPrefix(action, "found:"):
			_, _ = w.Write([]byte(`{"Parameter":{"Name":"` + req.Name + `","Value":"` + strings.TrimPrefix(action, "found:") + `"}}`))
		case action == "novalue":
			_, _ = w.Write([]byte(`{"Parameter":{"Name":"` + req.Name + `"}}`))
		case action == "denied":
			w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"not authorized to perform ssm:GetParameter"}`))
		default:
			w.Header().Set("X-Amzn-ErrorType", "ParameterNotFound")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"ParameterNotFound","message":""}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSSM) client() *ssm.Client {
	return ssm.New(ssm.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(f.srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
	})
}

func (f *fakeSSM) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

func TestGetSSMParameterValue_NotFoundMovesToNextCandidate(t *testing.T) {
	f := newFakeSSM(t, map[string]string{"/a/p": "notfound", "/b/p": "found:bucket-1"})

	got, err := getSSMParameterValue(context.Background(), f.client(), []string{"/a/p", "/b/p"})
	if err != nil || got != "bucket-1" {
		t.Fatalf("got %q, %v; want bucket-1, nil", got, err)
	}
	if !reflect.DeepEqual(f.names(), []string{"/a/p", "/b/p"}) {
		t.Errorf("requested %v, want both candidates in order", f.names())
	}
}

// The heart of A-21: an AccessDenied on the first candidate is the REAL error
// and must surface at once. Before the fix the loop swallowed it, tried the next
// candidate and reported whichever error came last.
func TestGetSSMParameterValue_FailsClosedOnRealError(t *testing.T) {
	f := newFakeSSM(t, map[string]string{"/a/p": "denied", "/b/p": "notfound"})

	_, err := getSSMParameterValue(context.Background(), f.client(), []string{"/a/p", "/b/p"})
	if err == nil {
		t.Fatal("expected the AccessDenied to surface")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v, want the real AccessDenied (not the later not-found)", err)
	}
	if got := f.names(); !reflect.DeepEqual(got, []string{"/a/p"}) {
		t.Errorf("requested %v; a real error must stop the search at the first candidate", got)
	}
}

// Bypass: an error that comes AFTER a clean miss must not be masked either,
// and a later success must not paper over an earlier real error.
func TestGetSSMParameterValue_RealErrorAfterMissStillWins(t *testing.T) {
	f := newFakeSSM(t, map[string]string{"/a/p": "notfound", "/b/p": "denied", "/c/p": "found:late"})

	got, err := getSSMParameterValue(context.Background(), f.client(), []string{"/a/p", "/b/p", "/c/p"})
	if err == nil || got != "" {
		t.Fatalf("got %q, %v; want the AccessDenied on the second candidate", got, err)
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v, want AccessDenied", err)
	}
}

func TestGetSSMParameterValue_AllMissingDoesNotLeakThePath(t *testing.T) {
	f := newFakeSSM(t, map[string]string{})

	_, err := getSSMParameterValue(context.Background(), f.client(), []string{"/helix-tools/production/customers/c/s3_bucket"})
	if err == nil {
		t.Fatal("expected an error when every candidate is missing")
	}
	if strings.Contains(err.Error(), "/helix-tools") || strings.Contains(err.Error(), "customers/") {
		t.Errorf("error %q prints an internal parameter path", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not found") {
		t.Errorf("error %q should say the parameter was not found", err)
	}
}

func TestGetSSMParameterValue_NoCandidates(t *testing.T) {
	f := newFakeSSM(t, nil)
	if _, err := getSSMParameterValue(context.Background(), f.client(), nil); err == nil {
		t.Fatal("expected an error with no candidates")
	}
	if len(f.names()) != 0 {
		t.Errorf("requested %v, want no calls", f.names())
	}
}

// End-to-end through the real candidate builder: with no override, exactly one
// GetParameter goes out, on the real prefix.
func TestSSMLookup_OnlyTheRealPathIsRequested(t *testing.T) {
	t.Setenv("HELIX_SSM_CUSTOMER_PREFIX", "")
	t.Setenv("HELIX_ENVIRONMENT", "")
	t.Setenv("ENVIRONMENT", "")

	f := newFakeSSM(t, map[string]string{})
	_, _ = getSSMParameterValue(context.Background(), f.client(), ssmParamCandidates("cust-1", "s3_bucket"))

	if want := []string{"/helix-tools/production/customers/cust-1/s3_bucket"}; !reflect.DeepEqual(f.names(), want) {
		t.Errorf("requested %v, want %v", f.names(), want)
	}
}

// A parameter that exists but carries no value is an error, not a silent "".
func TestGetSSMParameterValue_ParameterWithoutValueIsAnError(t *testing.T) {
	f := newFakeSSM(t, map[string]string{"/a/p": "novalue", "/b/p": "found:never-reached"})

	got, err := getSSMParameterValue(context.Background(), f.client(), []string{"/a/p", "/b/p"})
	if err == nil || got != "" {
		t.Fatalf("got %q, %v; want an error for a value-less parameter", got, err)
	}
	if len(f.names()) != 1 {
		t.Errorf("requested %v; a value-less parameter must stop the search", f.names())
	}
}
