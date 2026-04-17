// Package agent provides a client for the Helix Connect agent-callable
// HTTP surface (the /v1/agents/* routes described in ADR-0037 and
// ADR-0038).
//
// It is intended for use by:
//
//   - Helix internal agents (Nova, helix-api-agent, etc.) that have
//     been issued a JWT v2 token.
//   - Customer-operated agents that have obtained a JWT (internal HS256
//     or external RS256 — the client doesn't care which as long as the
//     token verifies on the server side).
//
// The client is admin-surface-free on purpose: admin-only endpoints
// (kill-switch, token revocation, DLQ operations, etc.) remain
// Python-SDK-exclusive per the cross-SDK parity decision recorded in
// the project memory.
//
// The types in this file mirror the wire format of the server-side
// types in:
//
//   - internal/resources/agents-api/types.go  (Phase 3 agent-callable)
//   - internal/resources/agent-admin/types.go (for AgentRecord shape)
//   - internal/pkg/agents/mcp.go              (MCP envelope wire shape)
//
// All json tags are snake_case and match the server wire format
// exactly. If the server types change, these must be updated in
// lockstep — there is no runtime schema negotiation.
package agent

import "time"

// AgentRecord describes a single agent registered with Helix.
//
// The on-the-wire shape mirrors agent-admin.AgentRecord (the admin
// surface) rather than agents-api.MeResponse (the pruned self-view).
// An agent calling Me() sees its own record minus a few fields the
// server considers policy-internal (rate limits, forbidden
// operations); those fields arrive as their zero values in this
// struct. List callers of the admin surface — if and when the Go SDK
// grows one — reuse the same type.
//
// ActorClass is one of: "human_admin", "internal_agent", "external_agent".
// TrustTier is one of:  "T0", "T1", "T2".
// Status is one of:     "active", "disabled", "review".
type AgentRecord struct {
	AgentID                string    `json:"agent_id"`
	DisplayName            string    `json:"display_name"`
	ActorClass             string    `json:"actor_class"`
	OwnerOrgID             string    `json:"owner_org_id,omitempty"`
	TrustTier              string    `json:"trust_tier"`
	Status                 string    `json:"status"`
	AllowedCustomers       []string  `json:"allowed_customers,omitempty"`
	AllowedOperations      []string  `json:"allowed_operations,omitempty"`
	ForbiddenOperations    []string  `json:"forbidden_operations,omitempty"`
	DefaultTokenTTLSeconds int       `json:"default_token_ttl_seconds"`
	WebhookURL             string    `json:"webhook_url,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// RegistryPeer is the minimal peer-discovery shape returned from
// GET /v1/agents/registry — just enough to know a peer exists, with
// no sensitive config fields leaked.
//
// Mirrors agents-api.RegistryPeer.
type RegistryPeer struct {
	AgentID     string `json:"agent_id"`
	DisplayName string `json:"display_name"`
	TrustTier   string `json:"trust_tier"`
	ActorClass  string `json:"actor_class"`
}

// RegistryList wraps the tier-filtered peer list.
//
// Count is a convenience field for clients that want an O(1) size
// without re-traversing Peers. The server always populates it.
//
// Mirrors agents-api.RegistryListResponse.
type RegistryList struct {
	Peers []RegistryPeer `json:"peers"`
	Count int            `json:"count"`
}

// MCPActor mirrors the actor block of an MCP envelope.
type MCPActor struct {
	ActorType       string `json:"actor_type"`
	AgentID         string `json:"agent_id"`
	OwnerOrgID      string `json:"owner_org_id"`
	ActingForUserID string `json:"acting_for_user_id,omitempty"`
}

// MCPAuth mirrors the auth block of an MCP envelope.
type MCPAuth struct {
	JWTVersion string   `json:"jwt_version"`
	JTI        string   `json:"jti"`
	Scopes     []string `json:"scopes"`
}

// MCPResource mirrors the resource block of an MCP envelope.
type MCPResource struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id,omitempty"`
	CustomerID   string `json:"customer_id"`
	Operation    string `json:"operation"`
}

// MCPTrace mirrors the trace block of an MCP envelope.
type MCPTrace struct {
	PolicyDecisionID  string `json:"policy_decision_id,omitempty"`
	ApprovalContextID string `json:"approval_context_id,omitempty"`
	AuditEventID      string `json:"audit_event_id,omitempty"`
}

// MCPEnvelope is the signed wire shape for tool results and inter-
// agent handoffs. Mirrors agents.MCPEnvelope exactly.
//
// The Signature field is transported alongside the envelope in the
// POST /v1/agents/mcp/context body (as the top-level "signature"
// field) rather than as a struct member of the envelope itself — the
// server verifies the signature over a canonical JSON of the
// envelope, and a Signature field inside the envelope would break
// that canonicalization. The client therefore uses `json:"-"` here
// and attaches the signature in SendMCPContext below.
type MCPEnvelope struct {
	EnvelopeVersion string      `json:"envelope_version"`
	RequestID       string      `json:"request_id"`
	CorrelationID   string      `json:"correlation_id"`
	ExecutionID     string      `json:"execution_id"`
	SentAt          time.Time   `json:"sent_at"`
	ExpiresAt       time.Time   `json:"expires_at"`
	TTLSeconds      int         `json:"ttl_seconds"`
	Actor           MCPActor    `json:"actor"`
	Auth            MCPAuth     `json:"auth"`
	Resource        MCPResource `json:"resource"`
	PayloadRef      string      `json:"payload_ref,omitempty"`
	Trace           MCPTrace    `json:"trace,omitempty"`

	// Signature is the hex-encoded HMAC-SHA256 computed by the
	// envelope builder. It does NOT travel inside the canonical
	// envelope JSON — see SendMCPContext for how it's transmitted.
	Signature string `json:"-"`
}

// mcpContextRequest is the internal wire shape posted to
// /v1/agents/mcp/context. Mirrors agents-api.MCPContextRequest.
type mcpContextRequest struct {
	Envelope  MCPEnvelope `json:"envelope"`
	Signature string      `json:"signature,omitempty"`
}

// MCPContextResponse is the ack returned by /v1/agents/mcp/context.
//
// Mirrors agents-api.MCPContextResponse.
type MCPContextResponse struct {
	Ack           bool      `json:"ack"`
	ReceivedAt    time.Time `json:"received_at"`
	CorrelationID string    `json:"correlation_id"`
}

// a2aSendRequest is the internal wire shape posted to /v1/agents/a2a/send.
//
// The server-side type (agents-api.A2ASendRequest) requires Operation
// and CustomerID. The Go SDK surface exposes those via the SendA2A
// options helper — see the Option functions below.
type a2aSendRequest struct {
	RecipientAgentID string `json:"recipient_agent_id"`
	Payload          any    `json:"payload"`
	CorrelationID    string `json:"correlation_id,omitempty"`
	Operation        string `json:"operation,omitempty"`
	CustomerID       string `json:"customer_id,omitempty"`
}

// A2ASendResponse is the queued-message receipt returned by
// /v1/agents/a2a/send.
//
// Mirrors agents-api.A2ASendResponse. Status is typically "queued"
// on a successful enqueue.
type A2ASendResponse struct {
	MessageID     string `json:"message_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Status        string `json:"status"`
}

// AuditEntry is one row of the audit feed returned by
// /v1/agents/audit.
//
// Mirrors agents-api.AuditEntry. Resource is a flattened string
// rather than a nested struct to avoid exposing internal
// event-resource shape to agent clients.
type AuditEntry struct {
	Timestamp time.Time `json:"timestamp"`
	EventType string    `json:"event_type"`
	Status    string    `json:"status"`
	Resource  string    `json:"resource"`
}
