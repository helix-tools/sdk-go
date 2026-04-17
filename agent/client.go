package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// userAgent is sent on every request so server-side logs can
// attribute traffic to a specific SDK build. The version is
// intentionally coarse — the server doesn't make decisions on it.
const userAgent = "helix-connect-sdk-go/agent"

// defaultTimeout is the per-request HTTP timeout applied when the
// caller doesn't supply one via WithHTTPClient. Chosen to be longer
// than the server-side default handler deadline so slow-but-healthy
// operations finish, but short enough that a stuck network is
// surfaced quickly.
const defaultTimeout = 30 * time.Second

// Client is the thin HTTP client for the agent-callable surface.
//
// A Client is safe for concurrent use by multiple goroutines — all
// fields are read-only after construction and the embedded
// *http.Client is itself goroutine-safe.
type Client struct {
	apiBaseURL string
	token      string
	httpClient *http.Client
	userAgent  string
}

// Option configures a Client at construction time. Options are
// applied left-to-right; later options override earlier ones.
type Option func(*Client)

// WithHTTPClient replaces the default *http.Client. Useful for
// injecting custom timeouts, retry middlewares, or instrumented
// round-trippers.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithUserAgent overrides the User-Agent header sent with each
// request. Server logs attribute traffic via this header — callers
// embedding the SDK in a named agent binary should set this to
// something like "nova/1.4.2 (helix-connect-sdk-go)".
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if strings.TrimSpace(ua) != "" {
			c.userAgent = ua
		}
	}
}

// NewClient constructs a Client pointed at apiBaseURL using the
// provided bearer token for every call.
//
// apiBaseURL must NOT end with a trailing slash — callers typically
// pass "https://api-go.helix.tools" in production. The base URL is
// used as-is for all endpoints; redirections are followed by the
// standard HTTP client.
//
// token is a JWT v2 (HS256 for internal agents, RS256 for external
// agents). The client never inspects the token — verification and
// claim extraction happen server-side.
//
// The returned Client always uses Content-Type: application/json on
// request bodies and sets User-Agent to the canonical SDK string
// unless overridden via WithUserAgent.
func NewClient(apiBaseURL, token string, opts ...Option) *Client {
	c := &Client{
		apiBaseURL: strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"),
		token:      strings.TrimSpace(token),
		httpClient: &http.Client{Timeout: defaultTimeout},
		userAgent:  userAgent,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// APIBaseURL returns the configured API base URL. Read-only.
func (c *Client) APIBaseURL() string { return c.apiBaseURL }

// do executes an HTTP request with the agent bearer token, decodes
// the JSON response into result (if non-nil), and maps error
// statuses to the typed errors declared in errors.go.
//
// For endpoints that need a special decoder (e.g. MCPContext's 409
// → DuplicateCorrelationError), callers pass a non-nil
// duplicateCorrelationID so the mapper can populate the
// correlation_id without re-parsing the request body.
func (c *Client) do(ctx context.Context, method, path string, body any, result any, duplicateCorrelationID string) error {
	if c.apiBaseURL == "" {
		return errors.New("agent: apiBaseURL is required")
	}
	if c.token == "" {
		return errors.New("agent: token is required")
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("agent: marshal request body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	url := c.apiBaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("agent: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Surface context cancellation / deadline separately so
		// callers can type-check against ctx.Err() without having
		// to unwrap a URL error first.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("agent: request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("agent: read response: %w", readErr)
	}

	requestID := firstNonEmpty(
		resp.Header.Get("X-Helix-Request-ID"),
		resp.Header.Get("X-Request-ID"),
	)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result == nil || len(rawBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(rawBody, result); err != nil {
			return fmt.Errorf("agent: decode response: %w", err)
		}
		return nil
	}

	// Map known status codes to typed errors.
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return &UnauthorizedError{RequestID: requestID}
	case http.StatusServiceUnavailable:
		return &KillSwitchOffError{RequestID: requestID}
	case http.StatusTooManyRequests:
		return &RateLimitedError{
			RetryAfterSeconds: parseRetryAfter(resp.Header.Get("Retry-After")),
			RequestID:         requestID,
		}
	case http.StatusConflict:
		if duplicateCorrelationID != "" {
			return &DuplicateCorrelationError{CorrelationID: duplicateCorrelationID}
		}
	}

	// Fallback: try to pull an error message out of the JSON body
	// for nicer diagnostics.
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Body:       string(rawBody),
		RequestID:  requestID,
	}
	var errResp struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(rawBody, &errResp) == nil {
		switch {
		case errResp.Message != "":
			apiErr.Message = errResp.Message
		case errResp.Error != "":
			apiErr.Message = errResp.Error
		}
	}
	return apiErr
}

// parseRetryAfter parses the Retry-After header. The header may be
// an integer number of seconds or an HTTP date (RFC 7231 §7.1.3).
// We accept integer seconds and HTTP-date; anything else returns 0
// (the caller will apply its own backoff).
func parseRetryAfter(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	// Integer seconds fast path.
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
		return n
	}
	// HTTP-date form.
	if t, err := http.ParseTime(raw); err == nil {
		delta := time.Until(t)
		if delta <= 0 {
			return 0
		}
		// Round up so "retry after 1.6s" becomes 2.
		seconds := int(delta.Seconds())
		if time.Duration(seconds)*time.Second < delta {
			seconds++
		}
		return seconds
	}
	return 0
}

// firstNonEmpty returns the first non-empty string in the args, or
// "" if all are empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ============================================================
// Public API
// ============================================================

// Me returns the caller's registry record, pruned of fields the
// server considers policy-internal (rate-limit configuration,
// forbidden-operations list). Those fields arrive as zero values.
//
// GET /v1/agents/me
func (c *Client) Me(ctx context.Context) (*AgentRecord, error) {
	var out AgentRecord
	if err := c.do(ctx, http.MethodGet, "/v1/agents/me", nil, &out, ""); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPeers returns the tier-scoped list of visible peers.
//
//   - T0 callers see every active agent.
//   - T1 callers see T0 + T1 only.
//   - T2 callers see an empty list (their tier can't discover peers).
//
// The empty list is returned as 200 OK from the server; callers
// should not treat it as an error.
//
// GET /v1/agents/registry
func (c *Client) ListPeers(ctx context.Context) (*RegistryList, error) {
	var out RegistryList
	if err := c.do(ctx, http.MethodGet, "/v1/agents/registry", nil, &out, ""); err != nil {
		return nil, err
	}
	// The server always populates Peers even on an empty tier, but
	// defensive: normalize nil to an empty slice so callers can
	// range over it unconditionally.
	if out.Peers == nil {
		out.Peers = []RegistryPeer{}
	}
	return &out, nil
}

// SendMCPContext sends a signed MCP envelope to the server for
// verification and acknowledgement.
//
// The envelope's Signature field (detached from the canonical JSON
// via json:"-") is sent as the top-level "signature" field of the
// request body, matching the server-side MCPContextRequest wire
// shape.
//
// Server behavior:
//
//   - Signature and freshness verification via
//     MCPEnvelopeBuilder.Verify.
//   - Replay protection via a Redis-backed idempotency store keyed on
//     envelope.correlation_id with a 1-hour TTL. A duplicate
//     correlation_id within the window returns 409, which this
//     client surfaces as DuplicateCorrelationError.
//
// POST /v1/agents/mcp/context
func (c *Client) SendMCPContext(ctx context.Context, envelope MCPEnvelope) (*MCPContextResponse, error) {
	req := mcpContextRequest{
		Envelope:  envelope,
		Signature: envelope.Signature,
	}
	var out MCPContextResponse
	if err := c.do(ctx, http.MethodPost, "/v1/agents/mcp/context", req, &out, envelope.CorrelationID); err != nil {
		return nil, err
	}
	return &out, nil
}

// A2AOption configures a single SendA2A call.
type A2AOption func(*a2aSendRequest)

// WithA2AOperation sets the operation the recipient will evaluate
// against its own policy. The server rejects SendA2A with a 400 if
// this is empty, so most callers should provide it.
func WithA2AOperation(operation string) A2AOption {
	return func(r *a2aSendRequest) {
		r.Operation = strings.TrimSpace(operation)
	}
}

// WithA2ACustomerID attaches the customer scope carried on the A2A
// envelope. The sender MUST have this customer in its token's
// customer_scope or the server rejects the send.
func WithA2ACustomerID(customerID string) A2AOption {
	return func(r *a2aSendRequest) {
		r.CustomerID = strings.TrimSpace(customerID)
	}
}

// SendA2A enqueues an A2A message for asynchronous delivery to the
// named recipient agent. Returns the outbox-tracked message id and
// the echoed correlation id.
//
// The server returns HTTP 202 Accepted immediately; the actual
// webhook delivery happens in the background with retries +
// eventual DLQ. The status in the response is typically "queued".
//
// Server requires:
//
//   - recipient_agent_id != caller.agent_id
//   - operation non-empty (pass via WithA2AOperation)
//   - customer_id non-empty, in caller's token scope
//     (pass via WithA2ACustomerID)
//   - recipient must exist in the active registry
//
// The simpler signature — recipient, correlation, payload — is the
// primary happy path; callers that need Operation or CustomerID
// should attach them via the options.
//
// POST /v1/agents/a2a/send
func (c *Client) SendA2A(ctx context.Context, recipientAgentID, correlationID string, payload any, opts ...A2AOption) (*A2ASendResponse, error) {
	req := a2aSendRequest{
		RecipientAgentID: strings.TrimSpace(recipientAgentID),
		CorrelationID:    strings.TrimSpace(correlationID),
		Payload:          payload,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&req)
		}
	}

	var out A2ASendResponse
	if err := c.do(ctx, http.MethodPost, "/v1/agents/a2a/send", req, &out, ""); err != nil {
		return nil, err
	}
	return &out, nil
}

// MyAuditTrail returns the caller's recent audit events (at most
// 100 entries from the last hour, server-enforced). The filter
// pattern is applied server-side by the CloudWatch FilterByAgent
// reader.
//
// A nil slice is returned when the server returns an empty array.
// Callers can len() the result without a nil check.
//
// GET /v1/agents/audit
func (c *Client) MyAuditTrail(ctx context.Context) ([]AuditEntry, error) {
	var out []AuditEntry
	if err := c.do(ctx, http.MethodGet, "/v1/agents/audit", nil, &out, ""); err != nil {
		return nil, err
	}
	if out == nil {
		out = []AuditEntry{}
	}
	return out, nil
}
