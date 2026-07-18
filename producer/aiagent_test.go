package producer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AI-agent (Wave-2 Model-2) self-serve producer surface:
//   - ProvisionAIAgent   -> POST   /v1/self/ai-agent -> {status,message}
//   - GetAIAgentStatus   -> GET    /v1/self/ai-agent -> {status,cost?,created_at?,message?}
//   - DeprovisionAIAgent -> DELETE /v1/self/ai-agent -> {status,message}
//
// Drives the REAL Producer methods end-to-end against an httptest server (real
// SigV4 signing + real error mapping — no re-implementation of client logic).
// Contract mirrored from helix-api internal/resources/aiagent (types.go /
// controller.go). Server-side this surface is dark behind HELIX_MODEL2_ENABLED;
// the Dark404 test pins that path.

func TestProvisionAIAgent_Provisioning(t *testing.T) {
	var gotMethod, gotPath string
	var gotBodyLen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBodyLen = len(raw)
		w.WriteHeader(http.StatusAccepted) // 202: an orchestrator launch is in flight
		_, _ = w.Write([]byte(`{"status":"provisioning","message":"provisioning started"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	res, err := p.ProvisionAIAgent(context.Background())
	if err != nil {
		t.Fatalf("ProvisionAIAgent: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/self/ai-agent" {
		t.Errorf("path = %q, want /v1/self/ai-agent", gotPath)
	}
	if gotBodyLen != 0 {
		t.Errorf("request body length = %d, want 0 (identity from signed request)", gotBodyLen)
	}
	if res.Status != "provisioning" {
		t.Errorf("status = %q, want provisioning", res.Status)
	}
	if res.Message != "provisioning started" {
		t.Errorf("message = %q, want 'provisioning started'", res.Message)
	}
}

func TestProvisionAIAgent_AlreadyActive(t *testing.T) {
	// Idempotent provision: the agent is already active, so POST returns 200 OK
	// with status "active" and does NOT launch again — the provision -> active
	// path a caller sees when it re-provisions a live agent.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"active","message":"agent already active"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	res, err := p.ProvisionAIAgent(context.Background())
	if err != nil {
		t.Fatalf("ProvisionAIAgent: %v", err)
	}
	if res.Status != "active" {
		t.Errorf("status = %q, want active", res.Status)
	}
	if res.Message != "agent already active" {
		t.Errorf("message = %q, want 'agent already active'", res.Message)
	}
}

func TestGetAIAgentStatus_NotProvisioned(t *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"not_provisioned"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	st, err := p.GetAIAgentStatus(context.Background())
	if err != nil {
		t.Fatalf("GetAIAgentStatus: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/v1/self/ai-agent" {
		t.Errorf("path = %q, want /v1/self/ai-agent", gotPath)
	}
	if st.Status != "not_provisioned" {
		t.Errorf("status = %q, want not_provisioned", st.Status)
	}
	if st.Cost != nil {
		t.Errorf("cost = %v, want nil (absent)", *st.Cost)
	}
	if st.CostUSD() != 0 {
		t.Errorf("CostUSD() = %v, want 0 (nil cost)", st.CostUSD())
	}
	if st.CreatedAt != nil {
		t.Errorf("created_at = %v, want nil (absent)", *st.CreatedAt)
	}
}

func TestGetAIAgentStatus_ActiveWithCost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"active","cost":12.34,"created_at":"2026-07-18T00:00:00Z"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	st, err := p.GetAIAgentStatus(context.Background())
	if err != nil {
		t.Fatalf("GetAIAgentStatus: %v", err)
	}
	if st.Status != "active" {
		t.Errorf("status = %q, want active", st.Status)
	}
	if st.Cost == nil {
		t.Fatal("cost = nil, want 12.34")
	}
	if *st.Cost != 12.34 {
		t.Errorf("cost = %v, want 12.34", *st.Cost)
	}
	if st.CostUSD() != 12.34 {
		t.Errorf("CostUSD() = %v, want 12.34", st.CostUSD())
	}
	if st.CreatedAt == nil || *st.CreatedAt != "2026-07-18T00:00:00Z" {
		t.Errorf("created_at = %v, want 2026-07-18T00:00:00Z", st.CreatedAt)
	}
}

func TestGetAIAgentStatus_Error(t *testing.T) {
	// Status "error" carries a curated, infra-free message (the ARN-leak guard).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"error","message":"provisioning failed; the team has been notified and will investigate"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	st, err := p.GetAIAgentStatus(context.Background())
	if err != nil {
		t.Fatalf("GetAIAgentStatus: %v", err)
	}
	if st.Status != "error" {
		t.Errorf("status = %q, want error", st.Status)
	}
	if !strings.Contains(st.Message, "team has been notified") {
		t.Errorf("message = %q, want curated error text", st.Message)
	}
}

func TestDeprovisionAIAgent(t *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted) // 202: a destroy launch is in flight
		_, _ = w.Write([]byte(`{"status":"deprovisioning","message":"deprovisioning started"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	res, err := p.DeprovisionAIAgent(context.Background())
	if err != nil {
		t.Fatalf("DeprovisionAIAgent: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/v1/self/ai-agent" {
		t.Errorf("path = %q, want /v1/self/ai-agent", gotPath)
	}
	if res.Status != "deprovisioning" {
		t.Errorf("status = %q, want deprovisioning", res.Status)
	}
	if res.Message != "deprovisioning started" {
		t.Errorf("message = %q, want 'deprovisioning started'", res.Message)
	}
}

func TestDeprovisionAIAgent_NoAgentNoop(t *testing.T) {
	// Deprovisioning a producer with no agent is an idempotent no-op: 200 OK with
	// status "not_provisioned".
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"not_provisioned","message":"no agent to deprovision"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	res, err := p.DeprovisionAIAgent(context.Background())
	if err != nil {
		t.Fatalf("DeprovisionAIAgent: %v", err)
	}
	if res.Status != "not_provisioned" {
		t.Errorf("status = %q, want not_provisioned", res.Status)
	}
}

func TestAIAgent_Dark404(t *testing.T) {
	// While HELIX_MODEL2_ENABLED is off server-side every endpoint responds 404.
	// All three methods must surface that as an error carrying the status code.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.ProvisionAIAgent(context.Background()); err == nil {
		t.Error("ProvisionAIAgent: expected error on 404")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("ProvisionAIAgent error = %q, want it to contain 404", err.Error())
	}
	if _, err := p.GetAIAgentStatus(context.Background()); err == nil {
		t.Error("GetAIAgentStatus: expected error on 404")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("GetAIAgentStatus error = %q, want it to contain 404", err.Error())
	}
	if _, err := p.DeprovisionAIAgent(context.Background()); err == nil {
		t.Error("DeprovisionAIAgent: expected error on 404")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("DeprovisionAIAgent error = %q, want it to contain 404", err.Error())
	}
}
