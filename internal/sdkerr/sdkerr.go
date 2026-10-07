// Package sdkerr gives every SDK package (consumer, producer, agent,
// credentials) one place to turn an upstream failure (a cloud SDK call, an
// HTTP client request, a compression/decompression library call) into a
// customer-facing error: an authored, capability-language message that never
// interpolates the upstream error's own text (which can carry internal
// identifiers or other internal service detail), while still
// attaching the original error as the cause so errors.Is / errors.As and a
// developer's own error-message logging can reach it.
package sdkerr

import (
	"errors"
	"net/http"
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

// IsRetryableIdentityStatus reports whether statusCode — 0 or negative
// meaning no HTTP response arrived at all (DNS, connection, TLS, timeout, or
// a canceled context) — means a static-credentials identity check (a direct
// call to the cloud provider, no Helix credential service involved) should
// be treated as a temporary failure rather than a credential rejection: 408,
// 429, or a status in the 500-599 range, or no response at all. A status
// outside that range (including one at or above 600) is a definitive
// answer, never temporary.
func IsRetryableIdentityStatus(statusCode int) bool {
	return statusCode <= 0 ||
		statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusTooManyRequests ||
		(statusCode >= http.StatusInternalServerError && statusCode <= 599)
}

// IdentityCheckTemporaryFailureMessage is the customer-facing text for a
// static-credentials identity check that IsRetryableIdentityStatus marked
// temporary: the provider itself could not complete the check, so the
// caller is never told their own credentials are invalid. statusCode 0 (or
// negative) means no response arrived at all; any other value is reported
// plainly, with no raw provider exception name, request id, or message
// attached. The caller must attach no cause to this message either — unlike
// every other error this package wraps, nothing about this one is reachable
// through errors.Unwrap, since the point of dropping the provider's name
// here is defeated if a caller's own %+v or further unwrapping still finds
// it.
func IdentityCheckTemporaryFailureMessage(statusCode int) string {
	if statusCode <= 0 {
		return "could not verify credentials: the identity check got no response, try again"
	}
	return "could not verify credentials: the identity check answered with a temporary error (HTTP " + strconv.Itoa(statusCode) + "), try again"
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

// WrapMarkers returns an error whose Error() is exactly msg and whose
// errors.Is/errors.As reach each of markers — a sentinel value for
// errors.Is, or a value of a specific type for errors.As — via the
// multi-error Unwrap() []error form, with no other cause attached at all.
// Unlike Wrap/WrapSentinel, msg, Error(), and a caller's fmt "%+v" (which
// for any error value calls only Error(), never reflects into fields) never
// include any text from the markers themselves: this is for a failure whose
// customer-facing message must stay clean of upstream detail while specific,
// pre-existing errors.Is/errors.As relationships (e.g. context.Canceled, or
// an SDK's own response-error type carrying nothing but what the caller is
// allowed to see) still have to resolve for callers that check them.
func WrapMarkers(msg string, markers ...error) error {
	return &markedChain{msg: msg, markers: markers}
}

type markedChain struct {
	msg     string
	markers []error
}

func (w *markedChain) Error() string   { return w.msg }
func (w *markedChain) Unwrap() []error { return w.markers }
