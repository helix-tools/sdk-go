package types

// AI-agent (Wave-2 Model-2) self-serve provisioning types.
//
// Mirrors the helix-api aiagent surface (GET/POST/DELETE /v1/self/ai-agent). The
// customer-facing projection carries ZERO infra internals (task ARNs, KMS, IAM,
// internal URLs) — only the lifecycle status, an optional USD cost, the creation
// time, and (on error) a curated, infra-free message.

// AIAgentState is the lifecycle state of a producer's per-producer AI agent.
// Modelled as a string alias (mirrors BillingStatus) so callers may compare
// against the canonical constants or a raw string interchangeably.
type AIAgentState = string

// Canonical AIAgentState values (the status field of the /v1/self/ai-agent
// responses). NotProvisioned is the virtual state for a producer with no agent
// record; it is never persisted server-side.
const (
	AIAgentStateNotProvisioned AIAgentState = "not_provisioned"
	AIAgentStateProvisioning   AIAgentState = "provisioning"
	AIAgentStateActive         AIAgentState = "active"
	AIAgentStateError          AIAgentState = "error"
	AIAgentStateDeprovisioning AIAgentState = "deprovisioning"
)

// AIAgentStatus is the masked status of a producer's per-producer AI agent
// (GET /v1/self/ai-agent). It NEVER carries infra internals — only the lifecycle
// status, an optional USD cost, the creation time, and (on error) a curated
// message. A producer with no agent gets Status AIAgentStateNotProvisioned.
type AIAgentStatus struct {
	// Status is one of the AIAgentState* constants.
	Status AIAgentState `json:"status"`
	// Cost is the current billing period's accrued AI-agent infrastructure cost
	// in US dollars (2dp). nil when unknown/zero (the server omits it). Read it
	// via CostUSD for a nil-safe figure.
	Cost *float64 `json:"cost,omitempty"`
	// CreatedAt is the RFC 3339 creation time of the agent record, or nil when
	// absent (e.g. not_provisioned). Kept as a string to match the SDK's other
	// timestamp fields (ConnectOnboardedAt, DelistedAt, ...).
	CreatedAt *string `json:"created_at,omitempty"`
	// Message is a curated, customer-safe explanation, set only when Status is
	// AIAgentStateError. It never embeds infra detail.
	Message string `json:"message,omitempty"`
}

// CostUSD returns the accrued AI-agent cost for the current billing period in US
// dollars, or 0 when the server omitted it (unknown/zero). A convenience over the
// optional Cost pointer so callers can read a figure without a nil check.
func (s AIAgentStatus) CostUSD() float64 {
	if s.Cost == nil {
		return 0
	}

	return *s.Cost
}

// AIAgentProvisionResult is the result of a provision (POST) or deprovision
// (DELETE) trigger on /v1/self/ai-agent. It mirrors the status projection with no
// infra: the resulting lifecycle status and an optional human-readable message.
type AIAgentProvisionResult struct {
	// Status is the resulting lifecycle state (one of the AIAgentState* constants).
	Status AIAgentState `json:"status"`
	// Message is an optional human-readable description of the outcome
	// (e.g. "provisioning started", "agent already active").
	Message string `json:"message,omitempty"`
}
