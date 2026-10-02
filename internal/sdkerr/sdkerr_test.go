package sdkerr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// errARNLaden is what a real cloud SDK error looks like: an operation
// name, a service name, and an ARN carrying an account ID — exactly the
// kind of text D19 says must never reach a customer-facing message.
var errARNLaden = errors.New("operation error KMS: Decrypt, https response error StatusCode: 400, " +
	"api error AccessDeniedException: User: arn:aws:iam::123456789012:user/test is not authorized " +
	"to perform: kms:Decrypt on resource: arn:aws:kms:us-east-1:123456789012:key/abcd-1234")

func TestWrap_MessageIsCleanCauseIsReachable(t *testing.T) {
	err := Wrap("decryption failed", errARNLaden)

	if err.Error() != "decryption failed" {
		t.Fatalf("Error() = %q, want exactly the clean authored message", err.Error())
	}
	if strings.Contains(err.Error(), "arn:aws:") || strings.Contains(err.Error(), "123456789012") {
		t.Fatalf("Error() = %q, leaks the upstream cause's ARN/account id", err.Error())
	}

	if !errors.Is(err, errARNLaden) {
		t.Fatal("errors.Is(err, cause) = false, want true — the cause must stay reachable")
	}
	if got := errors.Unwrap(err); got != errARNLaden {
		t.Fatalf("errors.Unwrap(err) = %v, want the original cause", got)
	}
}

// TestWrap_NegativeControl is the negative control: build the SAME error the
// way the pre-fix code did (fmt.Errorf with %w) and confirm THAT leaks, so
// the assertions above are proven to fail for the right reason on the old
// shape rather than passing vacuously.
func TestWrap_NegativeControl(t *testing.T) {
	legacy := fmt.Errorf("decryption failed: %w", errARNLaden)

	if !strings.Contains(legacy.Error(), "arn:aws:") {
		t.Fatalf("negative control did not reproduce the leak: %q", legacy.Error())
	}
}

func TestWrap_AsReachesTypedCause(t *testing.T) {
	type typedCause struct{ error }
	cause := typedCause{errors.New("operation error S3: GetObject, arn:aws:s3:::acct-bucket/key")}

	err := Wrap("failed to download", cause)

	var target typedCause
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to reach the typed cause through Wrap")
	}
	if err.Error() != "failed to download" {
		t.Fatalf("Error() = %q, want the clean message unchanged", err.Error())
	}
}

func TestWrapSentinel_KeepsSentinelAndCauseReachable(t *testing.T) {
	sentinel := errors.New("object is not compressed; refusing to return it uncompressed")
	cause := errors.New("gzip: invalid header")

	err := WrapSentinel(sentinel, cause)

	if err.Error() != sentinel.Error() {
		t.Fatalf("Error() = %q, want the sentinel's own clean message", err.Error())
	}
	if strings.Contains(err.Error(), "gzip:") {
		t.Fatalf("Error() = %q, leaks the raw gzip cause text", err.Error())
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is(err, sentinel) = false, want true — existing sentinel behaviour must keep working")
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is(err, cause) = false, want true — the raw cause must stay reachable for debugging")
	}
}

// TestWrapSentinel_NestedTwoLevelsDeep is the bypass test: an upstream error
// wrapped a second time (as happens when a caller re-wraps an already-clean
// error with its own authored prefix via fmt.Errorf("...: %w", err)) must
// still not leak the original cause's text into the visible message, and the
// cause and sentinel must both still be reachable through the deeper chain.
func TestWrapSentinel_NestedTwoLevelsDeep(t *testing.T) {
	sentinel := errors.New("object is not compressed; refusing to return it uncompressed")
	cause := errors.New("gzip: invalid header: arn:aws:s3:::acct-123456789012-bucket/key")

	inner := WrapSentinel(sentinel, cause)
	outer := fmt.Errorf("decompression failed: %w", inner)

	if strings.Contains(outer.Error(), "arn:aws:") || strings.Contains(outer.Error(), "123456789012") {
		t.Fatalf("outer.Error() = %q, leaks the cause nested two levels deep", outer.Error())
	}
	if !errors.Is(outer, sentinel) {
		t.Fatal("errors.Is(outer, sentinel) = false through a second wrap layer")
	}
	if !errors.Is(outer, cause) {
		t.Fatal("errors.Is(outer, cause) = false through a second wrap layer")
	}
}

func TestWrapMarked_MarkerAndCauseReachableMessageUnchanged(t *testing.T) {
	marker := errors.New("category marker")
	err := WrapMarked("mint request failed", marker, errARNLaden)

	if err.Error() != "mint request failed" {
		t.Fatalf("Error() = %q, want exactly the authored message", err.Error())
	}
	if !errors.Is(err, marker) {
		t.Fatal("errors.Is(err, marker) = false, want true")
	}
	if !errors.Is(err, errARNLaden) {
		t.Fatal("errors.Is(err, cause) = false, want true")
	}
	if got := errors.Unwrap(err); got != errARNLaden {
		t.Fatalf("errors.Unwrap(err) = %v, want the original cause (single-error chain unchanged)", got)
	}
	// Still reachable through further %w wrapping, as in mintWithRetry.
	if outer := fmt.Errorf("mint failed after 3 attempts: %w", err); !errors.Is(outer, marker) {
		t.Fatal("errors.Is(outer, marker) = false through a %w wrap, want true")
	}
}

// TestWrap_CarriesNoMarker is the negative control for WrapMarked: a plain
// Wrap must never report a marker, or every wrapped failure would be
// mistaken for an unreachable credential service.
func TestWrap_CarriesNoMarker(t *testing.T) {
	if errors.Is(Wrap("mint request failed", errARNLaden), ErrCredentialServiceUnreachable) {
		t.Fatal("errors.Is(Wrap(...), ErrCredentialServiceUnreachable) = true, want false")
	}
	if errors.Is(WrapMarked("x", nil, errARNLaden), ErrCredentialServiceUnreachable) {
		t.Fatal("a nil marker must match nothing")
	}
}

func TestCredentialServiceMessage(t *testing.T) {
	cases := []struct {
		status        int
		code, message string
		want          string
	}{
		{500, "internal_error", "boom", "Helix credential service error (internal_error): boom"},
		{503, "", "upstream unavailable", "Helix credential service error: upstream unavailable"},
		{502, "", "", "Helix credential service error: HTTP 502"},
		{500, "internal_error", "", "Helix credential service error (internal_error): HTTP 500"},
	}
	for _, tc := range cases {
		if got := CredentialServiceMessage(tc.status, tc.code, tc.message); got != tc.want {
			t.Errorf("CredentialServiceMessage(%d, %q, %q) = %q, want %q", tc.status, tc.code, tc.message, got, tc.want)
		}
	}
}
