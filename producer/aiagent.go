package producer

import (
	"context"
	"net/http"

	"github.com/helix-tools/sdk-go/v2/types"
)

// AI-agent (Wave-2 Model-2) self-serve provisioning.
//
// A producer manages THEIR OWN per-producer, Helix-hosted AI agent via three
// endpoints under /v1/self/ai-agent (producer JWT v1 / SigV4 — the same auth as
// the other self-serve producer methods). The status projection NEVER exposes
// infra internals; see types.AIAgentStatus.
//
// NOTE: server-side this surface is dark behind the HELIX_MODEL2_ENABLED feature
// flag. While it is off, all three endpoints respond 404 and these methods return
// that API error (a *APIError with StatusCode 404).

// ProvisionAIAgent triggers provisioning of the caller's per-producer AI agent.
// Idempotent: a call while the agent is already active or provisioning returns
// the current status without launching a second time.
//
// POST /v1/self/ai-agent. Producer identity comes from the signed request, never
// a request body (none is sent). Returns the resulting lifecycle status (e.g.
// "provisioning", or "active" when already provisioned) and a message.
func (p *Producer) ProvisionAIAgent(ctx context.Context) (*types.AIAgentProvisionResult, error) {
	var resp types.AIAgentProvisionResult
	if err := p.makeAPIRequest(ctx, http.MethodPost, "/v1/self/ai-agent", nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetAIAgentStatus returns the masked status of the caller's per-producer AI
// agent. The response NEVER carries infra internals — only the lifecycle status,
// an optional USD cost (read it via types.AIAgentStatus.CostUSD), the creation
// time, and (on error) a curated message. A producer with no agent gets status
// "not_provisioned".
//
// GET /v1/self/ai-agent.
func (p *Producer) GetAIAgentStatus(ctx context.Context) (*types.AIAgentStatus, error) {
	var resp types.AIAgentStatus
	if err := p.makeAPIRequest(ctx, http.MethodGet, "/v1/self/ai-agent", nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// DeprovisionAIAgent triggers teardown of the caller's per-producer AI agent.
// Idempotent: deprovisioning a producer with no agent is a safe no-op that
// returns status "not_provisioned".
//
// DELETE /v1/self/ai-agent. Returns the resulting lifecycle status (e.g.
// "deprovisioning") and a message.
func (p *Producer) DeprovisionAIAgent(ctx context.Context) (*types.AIAgentProvisionResult, error) {
	var resp types.AIAgentProvisionResult
	if err := p.makeAPIRequest(ctx, http.MethodDelete, "/v1/self/ai-agent", nil, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}
