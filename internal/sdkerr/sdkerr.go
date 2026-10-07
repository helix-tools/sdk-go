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
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
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
// interpolated into it — and whose Unwrap() returns cause UNCHANGED, so
// errors.Is and errors.As still traverse to the original upstream error.
// Wrap never sanitizes cause itself: a call site that needs SanitizeCause's
// no-host/no-URL guarantee (see its doc comment for the two cases that do)
// applies it explicitly before calling Wrap — sdkerr.Wrap(msg,
// sdkerr.SanitizeCause(cause)) — so that every other call site stays
// byte-for-byte behavior-compatible with cause passed straight through.
func Wrap(msg string, cause error) error {
	return &wrapped{msg: msg, cause: cause}
}

// WrapMarked is Wrap plus a category marker: Error() is exactly msg,
// Unwrap() is cause UNCHANGED (see Wrap's doc comment — apply
// SanitizeCause explicitly first if the call site needs it), and
// errors.Is(err, marker) also reports true, so a caller further up can
// recognise the failure category without matching on message text.
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
// marker) AND cause UNCHANGED (the raw upstream error, for debugging — see
// Wrap's doc comment: apply SanitizeCause explicitly first if the call site
// needs it) — via the multi-error Unwrap() []error form errors.Is/As have
// followed since Go 1.20.
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

// SanitizeCause returns cause unchanged unless its chain carries one of the
// stdlib network-transport error types whose own Error() method embeds a
// dialed host, a network address, or a request URL: *url.Error,
// *net.OpError, *net.DNSError, *net.AddrError. Those appear not just at the
// top of cause but also buried inside a generic wrapper that quotes its own
// cause's text in its Error() method (an AWS SDK *smithy.OperationError or
// *retry.MaxAttemptsError, for instance) — for a storage upload/download the
// URL is a presigned link carrying its SigV4 credential, security token,
// signature and bucket, and that text stays reachable through a plain
// errors.Unwrap walk even though Wrap's own Error() never shows it.
//
// When a branch of cause carries one of those types anywhere in its chain,
// the WHOLE branch is discarded — never just the host-bearing node itself —
// and replaced with a fresh stand-in holding no pointer back into the
// original tree: its Error() is a fixed, generic phrase with no host, URL,
// query string or credential; its Timeout()/Temporary() answer exactly as
// the innermost recognized node would have; and errors.Is against it
// reports true for context.Canceled/context.DeadlineExceeded exactly when
// the original branch did. Nothing else about the original branch survives
// — a prior design kept whatever a host-bearing node wrapped (a context
// sentinel, a redirect-refusal marker, a bare syscall errno) reachable
// through the stand-in's own Unwrap(), but that "first unrecognized
// descendant" can just as easily be an adversarial or merely-unfamiliar
// error type whose own Error() text embeds the same sensitive data (a
// custom RoundTripper returning fmt.Errorf("dial %s: %w", signedURL, …), for
// instance) — see TestWrap_RoundTripperErrorWithOwnSensitiveText. Only two
// relationships are preserved by design, and both without retaining any
// error value: cancellation/deadline via errors.Is, and the net.Error
// Timeout()/Temporary() answer.
//
// A branch with no such type anywhere in its chain (a response-bearing API
// error, a local parse/crypto error, an authored sentinel) is never
// network-transport-shaped and is returned unchanged.
//
// cause itself may be a joined error (errors.Join, Unwrap() []error), at any
// depth — not just at the top: each direct branch of a join is sanitized
// independently, so a safe sibling — a context sentinel, an SDK marker, or
// an unrelated failure's message joined alongside a genuine transport
// failure — stays fully reachable (both via errors.Is and in a dump of the
// error) even though the transport-shaped branch next to it is replaced.
// This holds even when the join sits BENEATH an ordinary single-cause
// wrapper (ordinaryWrapper.Unwrap() returns errors.Join(transportErr,
// safeSibling), rather than cause being the join itself): the wrapper's own
// Error() can quote its cause's text exactly like the generic-wrapper case
// above, so it cannot be trusted to survive untouched either, but the join
// underneath it is still unwrapped down to and sanitized exactly as if it
// had been cause directly — the wrapper is discarded, the join (with its
// safe sibling intact) is what SanitizeCause returns instead of it.
//
// SanitizeCause is deliberately NOT applied automatically by Wrap/
// WrapMarked/WrapSentinel — call it explicitly, and only at two kinds of
// call sites: (a) wrapping an http.Client.Do error already confirmed to
// have a nil *http.Response (a genuine no-response transport failure: DNS,
// connection, TLS, or timeout — never a refused-redirect, which net/http
// pairs with a non-nil Response instead); or (b) wrapping a request-
// construction error (http.NewRequest/url.Parse) for a URL that carries a
// query string or was itself returned by the API, e.g. a presigned storage
// upload/download URL. Every other wrap site passes its cause to Wrap/
// WrapMarked/WrapSentinel unchanged.
func SanitizeCause(cause error) error {
	return sanitizeTree(cause)
}

// sanitizeTree is SanitizeCause's recursive implementation.
//
// For a joined error (Unwrap() []error) it rebuilds the join — via the
// stdlib errors.Join, which computes Error() dynamically from its current
// members rather than baking text in at construction — from each branch's
// own sanitized result, so a safe sibling of a sanitized branch is never
// discarded; if none of the branches changed, e itself is returned,
// unchanged.
//
// For anything else, it walks e's own single-cause chain (via
// firstHostBearingOrJoin) looking for whichever comes first: a host-bearing
// node, or a nested join.
//   - A host-bearing node first: the whole of e is discarded and replaced
//     with a fresh stand-in (buildTransportFailure) — e's own Error() method
//     cannot be trusted not to quote the host-bearing node's text.
//   - A join first (before any host-bearing node): e is discarded too (same
//     reason), but recursing sanitizeTree on the join itself re-enters the
//     branch above, so the join's branches — including any safe sibling —
//     are preserved rather than collapsed into one opaque stand-in. If that
//     recursion finds nothing to sanitize either, e is still returned
//     unchanged: nothing in its chain needed replacing.
//   - Neither: e is returned unchanged.
//
// sanitizeTree itself never compares error values with ==/!= to detect a
// change — e's chain may hold a caller-supplied error type whose dynamic
// type is not comparable (e.g. a struct carrying a slice or map field
// stored by value), and comparing two interface values of an identical
// non-comparable dynamic type panics at runtime even when the two values
// are the exact same one (an unchanged branch is returned as itself, so
// this is not a corner case: it is the common case). The actual work is
// done by sanitizeTreeChanged, which reports whether anything changed as an
// explicit bool instead.
func sanitizeTree(e error) error {
	sanitized, _ := sanitizeTreeChanged(e)
	return sanitized
}

// sanitizeTreeChanged is sanitizeTree's implementation: same walk, same
// result, plus an explicit bool reporting whether the returned error is the
// same value as e (false) or a replacement (true) — see sanitizeTree's doc
// comment for why that can never be answered with e/sc comparisons.
func sanitizeTreeChanged(e error) (error, bool) {
	if e == nil {
		return nil, false
	}
	if multi, ok := e.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		sanitized := make([]error, len(children))
		changed := false
		for i, child := range children {
			sc, childChanged := sanitizeTreeChanged(child)
			if childChanged {
				changed = true
			}
			sanitized[i] = sc
		}
		if !changed {
			return e, false
		}
		return errors.Join(sanitized...), true
	}

	hostNode, joinNode := firstHostBearingOrJoin(e)
	switch {
	case hostNode != nil:
		return buildTransportFailure(e, hostNode), true
	case joinNode != nil:
		if sanitizedJoin, joinChanged := sanitizeTreeChanged(joinNode); joinChanged {
			return sanitizedJoin, true
		}
		return e, false
	default:
		return e, false
	}
}

// buildTransportFailure returns the sanitized stand-in for branch, whose
// chain contains a host-bearing node starting at start (as found by
// firstHostBearingOrJoin(branch)). timeout/temporary are copied by
// flattening through every CONSECUTIVE host-bearing node from start — none
// of the recognized host-bearing types use the multi-error Unwrap() []error
// form, so a single-error walk is enough — landing on whatever they
// wrapped, which is itself discarded, never retained. canceled/
// deadlineExceeded are computed against the FULL original branch (not just
// from start down), since a context sentinel commonly sits underneath a
// *net.OpError that itself sits underneath the *url.Error
// firstHostBearingOrJoin found.
func buildTransportFailure(branch, start error) error {
	var timeout, temporary bool
	for node := start; node != nil; {
		ne, inner, ok := hostBearing(node)
		if !ok {
			break
		}
		timeout, temporary = ne.Timeout(), ne.Temporary()
		node = inner
	}

	return &transportFailure{
		timeout:          timeout,
		temporary:        temporary,
		canceled:         errors.Is(branch, context.Canceled),
		deadlineExceeded: errors.Is(branch, context.DeadlineExceeded),
	}
}

// firstHostBearingOrJoin walks e's chain through ordinary single-cause
// wrappers (Unwrap() error) looking for whichever comes first: a
// host-bearing node (see hostBearing), returned as hostNode, or a node
// exposing Unwrap() []error (a nested join), returned as joinNode. At most
// one of the two return values is non-nil. Unlike an earlier version of
// this walk, it never descends INTO a join looking for a host-bearing node
// inside one of its branches — doing that let the caller discard the whole
// branch up to and including the join, losing any safe sibling joined
// alongside the transport failure (see sanitizeTree's doc comment on the
// join case for why the join itself, not a node inside it, is what gets
// handed back for the caller to recurse into instead).
// Returns (nil, nil) if neither exists anywhere in e's single-cause chain.
func firstHostBearingOrJoin(e error) (hostNode, joinNode error) {
	for e != nil {
		if _, _, ok := hostBearing(e); ok {
			return e, nil
		}
		if _, ok := e.(interface{ Unwrap() []error }); ok {
			return nil, e
		}
		e = errors.Unwrap(e)
	}
	return nil, nil
}

// hostBearing reports whether e is itself one of the stdlib types whose own
// Error() text embeds a host, network address, or URL, returning its
// net.Error view and the one error it directly wraps (nil for the two leaf
// types, which wrap nothing).
func hostBearing(e error) (net.Error, error, bool) {
	switch v := e.(type) {
	case *url.Error:
		return v, v.Err, true
	case *net.OpError:
		return v, v.Err, true
	case *net.DNSError:
		return v, nil, true
	case *net.AddrError:
		return v, nil, true
	}
	return nil, nil, false
}

// transportFailure is the sanitized stand-in SanitizeCause returns for a raw
// network-transport error. It carries no pointer back into the original
// error tree — only booleans captured before that tree was discarded — so
// no unrecognized descendant of the original cause, sensitive or not, stays
// reachable through it.
type transportFailure struct {
	timeout, temporary         bool
	canceled, deadlineExceeded bool
}

func (t *transportFailure) Error() string {
	return "network transport failure: no usable response was received"
}

func (t *transportFailure) Timeout() bool   { return t.timeout }
func (t *transportFailure) Temporary() bool { return t.temporary } //nolint:staticcheck // preserved for callers that still check it

// Is reports true for context.Canceled or context.DeadlineExceeded exactly
// when the original (now-discarded) branch matched them — the one
// relationship, besides net.Error, that survives sanitizing without
// retaining any error value from that branch. There is deliberately no
// Unwrap: nothing else is reachable through the stand-in.
func (t *transportFailure) Is(target error) bool {
	switch target { //nolint:errorlint // intentional identity check against the two context sentinels
	case context.Canceled:
		return t.canceled
	case context.DeadlineExceeded:
		return t.deadlineExceeded
	default:
		return false
	}
}
