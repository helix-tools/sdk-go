package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

// testServer wires up an httptest.Server whose handler records the last
// request and returns whatever the test's handler func decides.
type testServer struct {
	srv      *httptest.Server
	lastPath string
	lastAuth string
	lastUA   string
	lastCT   string
	lastBody []byte
}

func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts.lastPath = r.URL.Path
		ts.lastAuth = r.Header.Get("Authorization")
		ts.lastUA = r.Header.Get("User-Agent")
		ts.lastCT = r.Header.Get("Content-Type")
		ts.lastBody = body
		// Put the body back so handlers can re-read if they want to.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handler(w, r)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (t *testServer) URL() string { return t.srv.URL }

// writeJSON writes v as JSON with the given status.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// ----------------------------------------------------------------------------
// Happy-path tests — one per method.
// ----------------------------------------------------------------------------

func TestMe_Success(t *testing.T) {
	want := AgentRecord{
		AgentID:                "agent-nova",
		DisplayName:            "Nova Orchestrator",
		ActorClass:             "internal_agent",
		TrustTier:              "T0",
		Status:                 "active",
		DefaultTokenTTLSeconds: 900,
		AllowedCustomers:       []string{"*"},
		AllowedOperations:      []string{"dataset.read", "dataset.subscribe"},
		WebhookURL:             "https://nova.internal/webhook",
		CreatedAt:              time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC),
		UpdatedAt:              time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC),
	}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s, want GET", r.Method)
		}
		writeJSON(t, w, http.StatusOK, want)
	})

	c := NewClient(ts.URL(), "jwt-token")
	got, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got.AgentID != want.AgentID {
		t.Errorf("AgentID=%q, want %q", got.AgentID, want.AgentID)
	}
	if got.TrustTier != want.TrustTier {
		t.Errorf("TrustTier=%q, want %q", got.TrustTier, want.TrustTier)
	}
	if got.DefaultTokenTTLSeconds != want.DefaultTokenTTLSeconds {
		t.Errorf("DefaultTokenTTLSeconds=%d, want %d", got.DefaultTokenTTLSeconds, want.DefaultTokenTTLSeconds)
	}
	if ts.lastPath != "/v1/agents/me" {
		t.Errorf("path=%q, want /v1/agents/me", ts.lastPath)
	}
	if ts.lastAuth != "Bearer jwt-token" {
		t.Errorf("auth=%q, want 'Bearer jwt-token'", ts.lastAuth)
	}
	if !strings.HasPrefix(ts.lastUA, "helix-connect-sdk-go") {
		t.Errorf("user-agent=%q, want prefix 'helix-connect-sdk-go'", ts.lastUA)
	}
}

func TestListPeers_Success(t *testing.T) {
	want := RegistryList{
		Peers: []RegistryPeer{
			{AgentID: "agent-api", DisplayName: "API Worker", TrustTier: "T0", ActorClass: "internal_agent"},
			{AgentID: "agent-schema", DisplayName: "Schema Worker", TrustTier: "T1", ActorClass: "internal_agent"},
		},
		Count: 2,
	}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s, want GET", r.Method)
		}
		writeJSON(t, w, http.StatusOK, want)
	})

	c := NewClient(ts.URL(), "jwt-token")
	got, err := c.ListPeers(context.Background())
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if got.Count != 2 {
		t.Errorf("Count=%d, want 2", got.Count)
	}
	if len(got.Peers) != 2 {
		t.Errorf("len(Peers)=%d, want 2", len(got.Peers))
	}
	if ts.lastPath != "/v1/agents/registry" {
		t.Errorf("path=%q, want /v1/agents/registry", ts.lastPath)
	}
}

func TestListPeers_EmptyNormalizedToSlice(t *testing.T) {
	// Server returns an object with nil Peers — client must
	// normalize to an empty slice so callers can range without a
	// nil check.
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"count":0}`))
	})

	c := NewClient(ts.URL(), "jwt")
	got, err := c.ListPeers(context.Background())
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if got.Peers == nil {
		t.Error("Peers=nil, want empty slice")
	}
	if len(got.Peers) != 0 {
		t.Errorf("len(Peers)=%d, want 0", len(got.Peers))
	}
}

func TestSendMCPContext_Success(t *testing.T) {
	corrID := "correlation-42"
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/agents/mcp/context" {
			t.Errorf("path=%q", r.URL.Path)
		}
		// Verify the wire format: body has top-level "envelope" and
		// "signature", with signature outside the envelope.
		var wire struct {
			Envelope  map[string]any `json:"envelope"`
			Signature string         `json:"signature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Fatalf("decode wire body: %v", err)
		}
		if wire.Signature != "deadbeef" {
			t.Errorf("signature=%q, want deadbeef", wire.Signature)
		}
		if _, hasSig := wire.Envelope["Signature"]; hasSig {
			t.Error("envelope contains Signature field; should be stripped")
		}
		if got := wire.Envelope["correlation_id"]; got != corrID {
			t.Errorf("envelope.correlation_id=%v, want %q", got, corrID)
		}
		writeJSON(t, w, http.StatusOK, MCPContextResponse{
			Ack:           true,
			ReceivedAt:    time.Now().UTC(),
			CorrelationID: corrID,
		})
	})

	c := NewClient(ts.URL(), "jwt")
	env := MCPEnvelope{
		EnvelopeVersion: "v1",
		RequestID:       "req-1",
		CorrelationID:   corrID,
		ExecutionID:     "exec-1",
		SentAt:          time.Now().UTC(),
		ExpiresAt:       time.Now().Add(time.Minute).UTC(),
		TTLSeconds:      60,
		Actor:           MCPActor{ActorType: "internal_agent", AgentID: "agent-nova", OwnerOrgID: "helix"},
		Auth:            MCPAuth{JWTVersion: "2", JTI: "jti-1", Scopes: []string{"dataset.read"}},
		Resource:        MCPResource{ResourceType: "dataset", CustomerID: "cust-1", Operation: "dataset.read"},
		Signature:       "deadbeef",
	}
	got, err := c.SendMCPContext(context.Background(), env)
	if err != nil {
		t.Fatalf("SendMCPContext: %v", err)
	}
	if !got.Ack {
		t.Error("Ack=false, want true")
	}
	if got.CorrelationID != corrID {
		t.Errorf("CorrelationID=%q, want %q", got.CorrelationID, corrID)
	}
}

func TestSendA2A_Success(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if r.URL.Path != "/v1/agents/a2a/send" {
			t.Errorf("path=%q", r.URL.Path)
		}
		var req a2aSendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.RecipientAgentID != "agent-peer" {
			t.Errorf("recipient=%q", req.RecipientAgentID)
		}
		if req.CorrelationID != "corr-9" {
			t.Errorf("correlation=%q", req.CorrelationID)
		}
		if req.Operation != "dataset.subscribe" {
			t.Errorf("operation=%q", req.Operation)
		}
		if req.CustomerID != "cust-123" {
			t.Errorf("customer_id=%q", req.CustomerID)
		}
		writeJSON(t, w, http.StatusAccepted, A2ASendResponse{
			MessageID:     "msg-42",
			CorrelationID: "corr-9",
			Status:        "queued",
		})
	})

	c := NewClient(ts.URL(), "jwt")
	got, err := c.SendA2A(context.Background(), "agent-peer", "corr-9",
		map[string]string{"hello": "world"},
		WithA2AOperation("dataset.subscribe"),
		WithA2ACustomerID("cust-123"),
	)
	if err != nil {
		t.Fatalf("SendA2A: %v", err)
	}
	if got.MessageID != "msg-42" {
		t.Errorf("MessageID=%q", got.MessageID)
	}
	if got.Status != "queued" {
		t.Errorf("Status=%q", got.Status)
	}
}

func TestMyAuditTrail_Success(t *testing.T) {
	want := []AuditEntry{
		{Timestamp: time.Now().UTC(), EventType: "agent.policy.evaluate", Status: "executed", Resource: "dataset/cust-1/ds-42"},
		{Timestamp: time.Now().UTC(), EventType: "agent.a2a.send", Status: "executed", Resource: "a2a/agent-peer"},
	}
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		writeJSON(t, w, http.StatusOK, want)
	})

	c := NewClient(ts.URL(), "jwt")
	got, err := c.MyAuditTrail(context.Background())
	if err != nil {
		t.Fatalf("MyAuditTrail: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d, want 2", len(got))
	}
	if got[0].EventType != "agent.policy.evaluate" {
		t.Errorf("got[0].EventType=%q", got[0].EventType)
	}
}

func TestMyAuditTrail_EmptyNormalized(t *testing.T) {
	// Server returns null body (JSON null) — client should return
	// an empty slice.
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("null"))
	})
	c := NewClient(ts.URL(), "jwt")
	got, err := c.MyAuditTrail(context.Background())
	if err != nil {
		t.Fatalf("MyAuditTrail: %v", err)
	}
	if got == nil {
		t.Error("got=nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len=%d, want 0", len(got))
	}
}

// ----------------------------------------------------------------------------
// Error path tests — one per typed error.
// ----------------------------------------------------------------------------

func TestMe_UnauthorizedError(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Helix-Request-ID", "req-xyz")
		writeJSON(t, w, http.StatusUnauthorized, map[string]string{"error": "token expired"})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("err=nil, want UnauthorizedError")
	}
	var ue *UnauthorizedError
	if !errors.As(err, &ue) {
		t.Fatalf("err=%T %v, want *UnauthorizedError", err, err)
	}
	if ue.RequestID != "req-xyz" {
		t.Errorf("RequestID=%q, want req-xyz", ue.RequestID)
	}
	if !IsUnauthorized(err) {
		t.Error("IsUnauthorized=false, want true")
	}
}

func TestListPeers_KillSwitchOffError(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "req-7")
		writeJSON(t, w, http.StatusServiceUnavailable, map[string]string{"error": "agents_disabled"})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.ListPeers(context.Background())
	var kse *KillSwitchOffError
	if !errors.As(err, &kse) {
		t.Fatalf("err=%T %v, want *KillSwitchOffError", err, err)
	}
	if kse.RequestID != "req-7" {
		t.Errorf("RequestID=%q, want req-7", kse.RequestID)
	}
	if !IsKillSwitchOff(err) {
		t.Error("IsKillSwitchOff=false")
	}
}

func TestSendA2A_RateLimitedError_IntegerSeconds(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-Helix-Request-ID", "req-limit")
		writeJSON(t, w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.SendA2A(context.Background(), "agent-peer", "corr", nil,
		WithA2AOperation("dataset.read"),
		WithA2ACustomerID("cust-1"),
	)
	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("err=%T %v, want *RateLimitedError", err, err)
	}
	if rle.RetryAfterSeconds != 42 {
		t.Errorf("RetryAfterSeconds=%d, want 42", rle.RetryAfterSeconds)
	}
	if rle.RequestID != "req-limit" {
		t.Errorf("RequestID=%q, want req-limit", rle.RequestID)
	}
	if !IsRateLimited(err) {
		t.Error("IsRateLimited=false")
	}
}

func TestSendA2A_RateLimitedError_HTTPDate(t *testing.T) {
	future := time.Now().UTC().Add(30 * time.Second)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", future.Format(http.TimeFormat))
		writeJSON(t, w, http.StatusTooManyRequests, nil)
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.SendA2A(context.Background(), "agent-peer", "corr", nil,
		WithA2AOperation("dataset.read"),
		WithA2ACustomerID("cust-1"),
	)
	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("err=%T, want *RateLimitedError", err)
	}
	// Be forgiving: the delta is ~30s but timing may skew ±2s.
	if rle.RetryAfterSeconds < 25 || rle.RetryAfterSeconds > 35 {
		t.Errorf("RetryAfterSeconds=%d, want ~30", rle.RetryAfterSeconds)
	}
}

func TestSendA2A_RateLimitedError_MalformedHeader(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "not-a-number")
		writeJSON(t, w, http.StatusTooManyRequests, nil)
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.SendA2A(context.Background(), "agent-peer", "corr", nil,
		WithA2AOperation("dataset.read"),
		WithA2ACustomerID("cust-1"),
	)
	var rle *RateLimitedError
	if !errors.As(err, &rle) {
		t.Fatalf("err=%T, want *RateLimitedError", err)
	}
	if rle.RetryAfterSeconds != 0 {
		t.Errorf("RetryAfterSeconds=%d, want 0 for malformed header", rle.RetryAfterSeconds)
	}
}

func TestSendMCPContext_DuplicateCorrelation(t *testing.T) {
	corrID := "corr-duplicate"
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, map[string]string{
			"error":   "duplicate_correlation_id",
			"message": "correlation_id has already been processed within the idempotency window",
		})
	})

	c := NewClient(ts.URL(), "jwt")
	env := MCPEnvelope{
		EnvelopeVersion: "v1",
		RequestID:       "req-1",
		CorrelationID:   corrID,
		Signature:       "sig",
	}
	_, err := c.SendMCPContext(context.Background(), env)
	var de *DuplicateCorrelationError
	if !errors.As(err, &de) {
		t.Fatalf("err=%T %v, want *DuplicateCorrelationError", err, err)
	}
	if de.CorrelationID != corrID {
		t.Errorf("CorrelationID=%q, want %q", de.CorrelationID, corrID)
	}
	if !IsDuplicateCorrelation(err) {
		t.Error("IsDuplicateCorrelation=false")
	}
}

func TestSendA2A_NonMCP409FallsThroughToAPIError(t *testing.T) {
	// 409 on any non-MCP endpoint should NOT produce
	// DuplicateCorrelationError — that semantic is MCP-only.
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, map[string]string{"error": "whatever"})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.SendA2A(context.Background(), "agent-peer", "corr", nil,
		WithA2AOperation("dataset.read"),
		WithA2ACustomerID("cust-1"),
	)
	var de *DuplicateCorrelationError
	if errors.As(err, &de) {
		t.Errorf("err=%T, want APIError not DuplicateCorrelationError", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err=%T, want *APIError", err)
	}
	if ae.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode=%d", ae.StatusCode)
	}
}

func TestMe_NotFoundFallsThroughToAPIError(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]string{"error": "agent not found"})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.Me(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err=%T, want *APIError", err)
	}
	if ae.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode=%d", ae.StatusCode)
	}
	if ae.Message != "agent not found" {
		t.Errorf("Message=%q", ae.Message)
	}
}

func TestMe_500FallsThroughToAPIError(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError, map[string]string{"message": "boom"})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.Me(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err=%T, want *APIError", err)
	}
	if ae.StatusCode != 500 {
		t.Errorf("StatusCode=%d", ae.StatusCode)
	}
	if ae.Message != "boom" {
		t.Errorf("Message=%q, want boom", ae.Message)
	}
}

func TestMe_500NonJSONBody(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("HTML error page"))
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.Me(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err=%T, want *APIError", err)
	}
	if ae.StatusCode != 500 {
		t.Errorf("StatusCode=%d", ae.StatusCode)
	}
	if ae.Body != "HTML error page" {
		t.Errorf("Body=%q", ae.Body)
	}
}

// ----------------------------------------------------------------------------
// Context + HTTP plumbing tests.
// ----------------------------------------------------------------------------

func TestMe_ContextCancellationRespected(t *testing.T) {
	// Server blocks until released; client should abort immediately
	// when the context is cancelled.
	release := make(chan struct{})
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(t, w, http.StatusOK, AgentRecord{})
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	c := NewClient(ts.URL(), "jwt")
	go func() {
		_, err := c.Me(ctx)
		done <- err
	}()
	// Give the request a moment to reach the server.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("err=nil, want context cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Me did not return after context cancel")
	}
}

func TestMe_ContextDeadlineRespected(t *testing.T) {
	// Server blocks forever; deadline must surface through.
	release := make(chan struct{})
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(t, w, http.StatusOK, AgentRecord{})
	})
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := NewClient(ts.URL(), "jwt")
	_, err := c.Me(ctx)
	if err == nil {
		t.Fatal("err=nil, want deadline exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err=%v, want context.DeadlineExceeded", err)
	}
}

func TestMe_HTTPClientTimeout(t *testing.T) {
	// Client-level timeout (WithHTTPClient), NOT context deadline.
	release := make(chan struct{})
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeJSON(t, w, http.StatusOK, AgentRecord{})
	})
	defer close(release)

	c := NewClient(ts.URL(), "jwt", WithHTTPClient(&http.Client{Timeout: 100 * time.Millisecond}))
	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("err=nil, want timeout")
	}
	// The http.Client timeout manifests as a url.Error whose
	// message contains "Client.Timeout"; we don't pin to that text
	// directly. Just ensure we did get *some* error.
}

func TestNewClient_AppliesOptions(t *testing.T) {
	c := NewClient("https://example.com/",
		"tok",
		WithUserAgent("custom-agent/1.0"),
		WithHTTPClient(&http.Client{Timeout: 5 * time.Second}),
	)
	if c.userAgent != "custom-agent/1.0" {
		t.Errorf("userAgent=%q", c.userAgent)
	}
	if c.httpClient.Timeout != 5*time.Second {
		t.Errorf("Timeout=%v", c.httpClient.Timeout)
	}
	// Trailing slash must be trimmed so path concatenation works.
	if c.apiBaseURL != "https://example.com" {
		t.Errorf("apiBaseURL=%q", c.apiBaseURL)
	}
	if c.APIBaseURL() != "https://example.com" {
		t.Errorf("APIBaseURL()=%q", c.APIBaseURL())
	}
}

func TestNewClient_NilOptionsAreSafe(t *testing.T) {
	// Nil options must not crash the constructor — helps callers
	// that conditionally build an option slice.
	c := NewClient("https://example.com", "tok", nil, WithUserAgent(""))
	if c.userAgent != userAgent {
		t.Errorf("default user-agent should be preserved when WithUserAgent(\"\"), got %q", c.userAgent)
	}
}

func TestNewClient_EmptyTokenErrors(t *testing.T) {
	c := NewClient("https://example.com", "")
	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("err=nil, want error on empty token")
	}
	if !strings.Contains(err.Error(), "token is required") {
		t.Errorf("err=%v, want 'token is required'", err)
	}
}

func TestNewClient_EmptyBaseURLErrors(t *testing.T) {
	c := NewClient("", "jwt")
	_, err := c.Me(context.Background())
	if err == nil {
		t.Fatal("err=nil, want error on empty baseURL")
	}
	if !strings.Contains(err.Error(), "apiBaseURL is required") {
		t.Errorf("err=%v", err)
	}
}

// ----------------------------------------------------------------------------
// Wire-format validation.
// ----------------------------------------------------------------------------

func TestHeaders_JSONContentTypeAndBearerTokenOnEveryRequest(t *testing.T) {
	var (
		saw401 atomic.Int32
	)
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer my-jwt" {
			saw401.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	c := NewClient(ts.URL(), "my-jwt")
	_, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if saw401.Load() != 0 {
		t.Errorf("server saw %d 401s, want 0", saw401.Load())
	}
	if ts.lastAuth != "Bearer my-jwt" {
		t.Errorf("auth=%q", ts.lastAuth)
	}

	_, err = c.SendA2A(context.Background(), "p", "c", map[string]string{"k": "v"},
		WithA2AOperation("dataset.read"), WithA2ACustomerID("cust-1"),
	)
	if err != nil {
		t.Fatalf("SendA2A: %v", err)
	}
	if ts.lastCT != "application/json" {
		t.Errorf("content-type=%q on POST, want application/json", ts.lastCT)
	}
}

func TestMCPEnvelope_SignatureStrippedFromCanonicalJSON(t *testing.T) {
	// Explicitly verify the Signature field's `json:"-"` tag: even
	// if we set Signature, it must not appear inside the envelope
	// object on the wire (it travels as the sibling "signature"
	// field of the request body instead).
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"Signature":"SECRET"`) {
			t.Errorf("envelope leaked Signature into wire body: %s", body)
		}
		if strings.Contains(string(body), `"signature":"SECRET"`) {
			// OK — this is the top-level sibling field, not inside
			// envelope. Confirm it's at the top level by finding it
			// outside an "envelope" object.
			// Quick heuristic: should see "envelope":{...no signature...},"signature":"SECRET".
		}
		writeJSON(t, w, http.StatusOK, MCPContextResponse{Ack: true, CorrelationID: "c"})
	})

	c := NewClient(ts.URL(), "jwt")
	env := MCPEnvelope{
		EnvelopeVersion: "v1",
		RequestID:       "r",
		CorrelationID:   "c",
		Signature:       "SECRET",
	}
	if _, err := c.SendMCPContext(context.Background(), env); err != nil {
		t.Fatalf("SendMCPContext: %v", err)
	}
}

func TestSendA2A_EmptyPayloadIsAllowed(t *testing.T) {
	// payload may be nil — the server-side type is
	// map[string]interface{} which JSON-decodes either nil or
	// absent to an empty map. The client just forwards it.
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusAccepted, A2ASendResponse{
			MessageID: "m", Status: "queued",
		})
	})

	c := NewClient(ts.URL(), "jwt")
	_, err := c.SendA2A(context.Background(), "agent-peer", "corr", nil,
		WithA2AOperation("dataset.read"),
		WithA2ACustomerID("cust-1"),
	)
	if err != nil {
		t.Errorf("SendA2A with nil payload: %v", err)
	}
}

func TestError_ErrorMessagesDescriptive(t *testing.T) {
	// Sanity-check that Error() strings are non-empty and contain
	// the status code / key identifiers — callers log these.
	tests := []struct {
		err  error
		want string
	}{
		{&UnauthorizedError{}, "unauthorized"},
		{&UnauthorizedError{RequestID: "r1"}, "r1"},
		{&KillSwitchOffError{}, "kill-switch"},
		{&RateLimitedError{RetryAfterSeconds: 5}, "5"},
		{&RateLimitedError{RequestID: "r1"}, "r1"},
		{&RateLimitedError{}, "rate limited"},
		{&DuplicateCorrelationError{CorrelationID: "corr-42"}, "corr-42"},
		{&DuplicateCorrelationError{}, "duplicate"},
		{&APIError{StatusCode: 418, Message: "teapot"}, "418"},
		{&APIError{StatusCode: 500, Body: "bad", RequestID: "rx"}, "500"},
		{&APIError{StatusCode: 404}, "404"},
	}
	for i, tc := range tests {
		msg := tc.err.Error()
		if msg == "" {
			t.Errorf("test %d: Error() returned empty string", i)
			continue
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("test %d: Error()=%q, want containing %q", i, msg, tc.want)
		}
	}
}

// ----------------------------------------------------------------------------
// parseRetryAfter unit tests (non-network).
// ----------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		input string
		want  int
		name  string
	}{
		{"", 0, "empty"},
		{"0", 0, "zero"},
		{"1", 1, "one"},
		{"120", 120, "two minutes"},
		{"-1", 0, "negative rejected"},
		{"not-a-number", 0, "non-numeric"},
		{"  5 ", 5, "whitespace trim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRetryAfter(tc.input)
			if got != tc.want {
				t.Errorf("parseRetryAfter(%q)=%d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter_HTTPDatePast(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(past)=%d, want 0", got)
	}
}
