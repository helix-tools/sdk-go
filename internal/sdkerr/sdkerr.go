// Package sdkerr gives every SDK package (consumer, producer, agent,
// credentials) one place to turn an upstream failure (a cloud SDK call, an
// HTTP client request, a compression/decompression library call) into a
// customer-facing error: an authored, capability-language message that never
// interpolates the upstream error's own text (which can carry account IDs,
// key ARNs, queue URLs, or other internal service detail), while still
// attaching the original error as the cause so errors.Is / errors.As and a
// developer's own error-message logging can reach it.
package sdkerr

import (
	"errors"
	"strconv"
)

// ErrCredentialServiceUnreachable marks a credential-session request that
// failed before any response arrived (DNS, connection, TLS, or timeout). The
// credentials package attaches it with WrapMarked; NewConsumer/NewProducer
// check for it so a caller sees that the Helix credential service could not
// be reached, instead of a message about AWS access keys they may never have
// configured.
var ErrCredentialServiceUnreachable = errors.New("could not reach the Helix credential service: credential mint request failed before a response")

// CredentialServiceMessage is the customer-facing text for an error the
// Helix credential service answered with but has no mapped message for (a
// 5xx, or an unmapped 4xx): it names the service and carries the service's
// own code and message, so an API-key caller is not told their AWS keys are
// at fault. code and message must already be scrubbed of secrets; an empty
// message falls back to the HTTP status.
func CredentialServiceMessage(statusCode int, code, message string) string {
	msg := "Helix credential service error"
	if code != "" {
		msg += " (" + code + ")"
	}
	if message == "" {
		return msg + ": HTTP " + strconv.Itoa(statusCode)
	}
	return msg + ": " + message
}

// KeyCallerServiceFailure is the customer-facing text when an API-key caller
// could not get working credentials for any other reason (e.g. the service
// answered with something unusable, or the credentials it issued were
// rejected): it names the credential service, never AWS keys the caller
// never configured. Callers attach the real failure with Wrap.
const KeyCallerServiceFailure = "Helix credential service error: could not get working credentials for this API key"

// Wrap returns an error whose Error() is exactly msg — cause's text is never
// interpolated into it — and whose Unwrap() returns cause, so errors.Is and
// errors.As still traverse to the original upstream error.
func Wrap(msg string, cause error) error {
	return &wrapped{msg: msg, cause: cause}
}

// WrapMarked is Wrap plus a category marker: Error() is exactly msg,
// Unwrap() is cause, and errors.Is(err, marker) also reports true, so a
// caller further up can recognise the failure category without matching on
// message text.
func WrapMarked(msg string, marker, cause error) error {
	return &wrapped{msg: msg, cause: cause, marker: marker}
}

type wrapped struct {
	msg    string
	cause  error
	marker error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.cause }

// Is reports whether target is this error's marker (see WrapMarked).
func (w *wrapped) Is(target error) bool { return w.marker != nil && target == w.marker }

// WrapSentinel returns an error whose Error() is exactly sentinel's message
// and whose errors.Is/errors.As reach BOTH sentinel (so an existing exported
// error category keeps matching, e.g. a package-level "not compressed"
// marker) AND cause (the raw upstream error, for debugging) — via the
// multi-error Unwrap() []error form errors.Is/As have followed since Go 1.20.
func WrapSentinel(sentinel, cause error) error {
	return &sentinelWrapped{sentinel: sentinel, cause: cause}
}

type sentinelWrapped struct {
	sentinel error
	cause    error
}

func (w *sentinelWrapped) Error() string   { return w.sentinel.Error() }
func (w *sentinelWrapped) Unwrap() []error { return []error{w.sentinel, w.cause} }
