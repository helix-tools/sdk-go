package agent

import (
	"errors"
	"fmt"
)

// UnauthorizedError is returned when the server rejects the caller's
// token with HTTP 401. Typically the token has expired, been revoked
// (JTI on the revocation set), or is malformed. The agent should
// obtain a fresh token and retry.
//
// RequestID is the server-side correlation identifier (from the
// "X-Request-ID" or "X-Helix-Request-ID" response header) when the
// server provides one, empty otherwise.
type UnauthorizedError struct {
	RequestID string
}

// Error implements the error interface.
func (e *UnauthorizedError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("agent: unauthorized (request_id=%s)", e.RequestID)
	}
	return "agent: unauthorized"
}

// KillSwitchOffError is returned when the server responds with HTTP 503
// on an agent endpoint. The server returns 503 when the global kill-
// switch is flipped off (HELIX_AGENTS_ENABLED=false) or when a
// per-component dependency — registry, audit reader, MCP builder,
// A2A service — has not been wired.
//
// Callers should not retry this error in a tight loop; the kill-
// switch flip is an operator action and typically resolves on the
// order of minutes.
type KillSwitchOffError struct {
	RequestID string
}

// Error implements the error interface.
func (e *KillSwitchOffError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("agent: kill-switch is off / agents disabled (request_id=%s)", e.RequestID)
	}
	return "agent: kill-switch is off / agents disabled"
}

// RateLimitedError is returned when the server responds with HTTP 429.
// The per-agent rate limiter has exceeded the sustained-rate + burst
// budget configured for the caller.
//
// RetryAfterSeconds is parsed from the Retry-After response header.
// Zero means the server did not send one and the caller should apply
// its own backoff (typically 1s with jitter).
type RateLimitedError struct {
	RetryAfterSeconds int
	RequestID         string
}

// Error implements the error interface.
func (e *RateLimitedError) Error() string {
	if e.RetryAfterSeconds > 0 {
		if e.RequestID != "" {
			return fmt.Sprintf("agent: rate limited, retry after %ds (request_id=%s)", e.RetryAfterSeconds, e.RequestID)
		}
		return fmt.Sprintf("agent: rate limited, retry after %ds", e.RetryAfterSeconds)
	}
	if e.RequestID != "" {
		return fmt.Sprintf("agent: rate limited (request_id=%s)", e.RequestID)
	}
	return "agent: rate limited"
}

// DuplicateCorrelationError is returned by SendMCPContext when the
// server rejects the envelope with HTTP 409 because its
// correlation_id has already been seen within the idempotency
// window (1 hour, see internal/resources/agents-api/controller.go).
//
// This is NOT a generic 409 — the agent surface only emits 409 from
// /v1/agents/mcp/context today. If other 409 semantics appear in a
// future server version this type may need splitting.
type DuplicateCorrelationError struct {
	CorrelationID string
}

// Error implements the error interface.
func (e *DuplicateCorrelationError) Error() string {
	if e.CorrelationID != "" {
		return fmt.Sprintf("agent: duplicate correlation_id %q within idempotency window", e.CorrelationID)
	}
	return "agent: duplicate correlation_id within idempotency window"
}

// APIError is the fallback error type for HTTP responses that don't
// map to one of the typed errors above (anything in 4xx/5xx that
// isn't 401/409/429/503). Callers can type-assert to inspect the raw
// status code and body, or use errors.Is / errors.As with the typed
// errors above for the common failure modes.
type APIError struct {
	StatusCode int
	Body       string
	Message    string
	RequestID  string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	switch {
	case e.Message != "" && e.RequestID != "":
		return fmt.Sprintf("agent: API error %d: %s (request_id=%s)", e.StatusCode, e.Message, e.RequestID)
	case e.Message != "":
		return fmt.Sprintf("agent: API error %d: %s", e.StatusCode, e.Message)
	case e.Body != "":
		return fmt.Sprintf("agent: API error %d: %s", e.StatusCode, e.Body)
	default:
		return fmt.Sprintf("agent: API error %d", e.StatusCode)
	}
}

// IsUnauthorized reports whether the error is (or wraps) an
// UnauthorizedError. Convenience for callers that don't want to
// type-assert directly.
func IsUnauthorized(err error) bool {
	var target *UnauthorizedError
	return errors.As(err, &target)
}

// IsKillSwitchOff reports whether the error is (or wraps) a
// KillSwitchOffError.
func IsKillSwitchOff(err error) bool {
	var target *KillSwitchOffError
	return errors.As(err, &target)
}

// IsRateLimited reports whether the error is (or wraps) a
// RateLimitedError.
func IsRateLimited(err error) bool {
	var target *RateLimitedError
	return errors.As(err, &target)
}

// IsDuplicateCorrelation reports whether the error is (or wraps) a
// DuplicateCorrelationError.
func IsDuplicateCorrelation(err error) bool {
	var target *DuplicateCorrelationError
	return errors.As(err, &target)
}
