package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// B-02: /v1/agents/me returns a 12-field projection, not a full AgentRecord.
// ----------------------------------------------------------------------------

// meWireKeys is the exact key set of the API's MeResponse (agents-api
// types.go): the pruned registry record — no version, rate_limit,
// forbidden_operations, jwks_uri or mcp_servers.
var meWireKeys = []string{
	"agent_id", "allowed_customers", "allowed_operations", "actor_class", "created_at",
	"default_token_ttl_seconds", "display_name", "owner_org_id", "status", "trust_tier",
	"updated_at", "webhook_url",
}

const meWireBody = `{
	"agent_id": "agent-nova", "display_name": "Nova", "actor_class": "internal_agent",
	"owner_org_id": "org-1", "trust_tier": "T1", "status": "active",
	"allowed_customers": ["*"], "allowed_operations": ["dataset.read"],
	"default_token_ttl_seconds": 900, "webhook_url": "https://nova.example/hook",
	"created_at": "2026-04-15T10:00:00Z", "updated_at": "2026-04-16T10:00:00Z"
}`

func TestGetMe_DecodesTheTwelveFieldProjection(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(meWireBody))
	})

	me, err := NewClient(ts.URL(), "jwt").GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if ts.lastPath != "/v1/agents/me" {
		t.Errorf("path = %q, want /v1/agents/me", ts.lastPath)
	}
	if me.AgentID != "agent-nova" || me.OwnerOrgID != "org-1" || me.TrustTier != "T1" || me.Status != "active" {
		t.Errorf("identity fields = %+v", me)
	}
	if me.DefaultTokenTTLSeconds != 900 || me.WebhookURL != "https://nova.example/hook" {
		t.Errorf("ttl/webhook = %d / %q", me.DefaultTokenTTLSeconds, me.WebhookURL)
	}
	if !reflect.DeepEqual(me.AllowedCustomers, []string{"*"}) || !reflect.DeepEqual(me.AllowedOperations, []string{"dataset.read"}) {
		t.Errorf("allow-lists = %v / %v", me.AllowedCustomers, me.AllowedOperations)
	}
	if me.CreatedAt.IsZero() || me.UpdatedAt.IsZero() {
		t.Errorf("timestamps not decoded: %v / %v", me.CreatedAt, me.UpdatedAt)
	}
}

// The type must not invent fields the endpoint never sends: AgentRecord.Version
// decoded to 0 (below the schema minimum of 1) on every /me call.
func TestAgentMe_HasExactlyTheWireKeys(t *testing.T) {
	typ := reflect.TypeOf(AgentMe{})
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		keys = append(keys, name)
	}
	sort.Strings(keys)
	want := append([]string(nil), meWireKeys...)
	sort.Strings(want)
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("AgentMe json keys = %v, want exactly %v", keys, want)
	}
	for _, phantom := range []string{"Version", "RateLimit", "ForbiddenOperations", "JWKSURI", "MCPServers"} {
		if _, found := typ.FieldByName(phantom); found {
			t.Errorf("AgentMe declares %s, which /v1/agents/me never returns", phantom)
		}
	}
}

// Me keeps its (*AgentRecord, error) signature for source compatibility and
// still returns what the server sent.
func TestMe_StillReturnsAgentRecord(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(meWireBody))
	})
	c := NewClient(ts.URL(), "jwt")

	var _ func(context.Context) (*AgentRecord, error) = c.Me

	rec, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if rec.AgentID != "agent-nova" || rec.DefaultTokenTTLSeconds != 900 || rec.TrustTier != "T1" {
		t.Errorf("Me = %+v", rec)
	}
}

// ----------------------------------------------------------------------------
// A-16: 503 is a kill-switch only when the body says so.
// ----------------------------------------------------------------------------

func TestDo_503ClassifiedByMarkers(t *testing.T) {
	killSwitch := map[string]string{
		"flat agents_disabled":        `{"error":"agents_disabled"}`,
		"flat agent_kill_switch":      `{"error":"agent_kill_switch"}`,
		"flat kill_switch":            `{"error":"kill_switch"}`,
		"nested code":                 `{"error":{"code":"agent_kill_switch"}}`,
		"nested code agents_disabled": `{"error":{"code":"agents_disabled"}}`,
		"service_unavailable + msg":   `{"error":{"code":"service_unavailable","message":"agent surface is disabled by operator"}}`,
		"top-level message":           `{"message":"agent kill-switch unavailable"}`,
		"non-JSON text":               `agent surface is disabled`,
	}
	for name, body := range killSwitch {
		t.Run("kill-switch/"+name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req-ks")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(body))
			})
			_, err := NewClient(ts.URL(), "jwt").GetMe(context.Background())
			var ks *KillSwitchOffError
			if !errors.As(err, &ks) {
				t.Fatalf("err = %T %v, want *KillSwitchOffError", err, err)
			}
			if ks.RequestID != "req-ks" {
				t.Errorf("RequestID = %q, want req-ks", ks.RequestID)
			}
			if IsServiceUnavailable(err) {
				t.Error("a kill-switch 503 must not also read as a generic outage")
			}
		})
	}

	// Load-balancer / upstream 503s: the operator did NOT disable anything.
	outage := map[string]string{
		"html from a load balancer": `<html><body><h1>503 Service Temporarily Unavailable</h1></body></html>`,
		"upstream error json":       `{"error":"upstream connect error or disconnect/reset before headers"}`,
		"empty body":                ``,
		"unrelated nested code":     `{"error":{"code":"service_unavailable","message":"try again shortly"}}`,
	}
	for name, body := range outage {
		t.Run("outage/"+name, func(t *testing.T) {
			ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Helix-Request-ID", "req-lb")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(body))
			})
			_, err := NewClient(ts.URL(), "jwt").GetMe(context.Background())
			if IsKillSwitchOff(err) {
				t.Fatalf("a bare 503 (%s) was reported as an operator kill-switch: %v", name, err)
			}
			var su *ServiceUnavailableError
			if !errors.As(err, &su) {
				t.Fatalf("err = %T %v, want *ServiceUnavailableError", err, err)
			}
			if su.RetryAfterSeconds != 7 || su.RequestID != "req-lb" {
				t.Errorf("RetryAfterSeconds/RequestID = %d/%q, want 7/req-lb", su.RetryAfterSeconds, su.RequestID)
			}
			if !IsServiceUnavailable(err) {
				t.Error("IsServiceUnavailable = false")
			}
		})
	}
}

func TestServiceUnavailableError_Message(t *testing.T) {
	for _, tc := range []struct {
		err  *ServiceUnavailableError
		want string
	}{
		{&ServiceUnavailableError{}, "service unavailable"},
		{&ServiceUnavailableError{RequestID: "r9"}, "r9"},
		{&ServiceUnavailableError{RetryAfterSeconds: 12}, "12"},
	} {
		if msg := tc.err.Error(); !strings.Contains(msg, tc.want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, tc.want)
		}
	}
}

// ----------------------------------------------------------------------------
// A-16: an empty base URL falls back to HELIX_API_ENDPOINT, then production.
// ----------------------------------------------------------------------------

func TestNewClient_EmptyBaseURLFallsBackToEnvThenDefault(t *testing.T) {
	t.Setenv("HELIX_API_ENDPOINT", "https://env.example/")
	if got := NewClient("", "jwt").APIBaseURL(); got != "https://env.example" {
		t.Errorf("APIBaseURL() = %q, want the HELIX_API_ENDPOINT value (trailing slash trimmed)", got)
	}
	if got := NewClient("   ", "jwt").APIBaseURL(); got != "https://env.example" {
		t.Errorf("whitespace-only base URL: APIBaseURL() = %q, want the env value", got)
	}

	t.Setenv("HELIX_API_ENDPOINT", "")
	if got := NewClient("", "jwt").APIBaseURL(); got != DefaultAPIBaseURL {
		t.Errorf("APIBaseURL() = %q, want DefaultAPIBaseURL %q", got, DefaultAPIBaseURL)
	}
	if DefaultAPIBaseURL != "https://api-go.helix.tools" {
		t.Errorf("DefaultAPIBaseURL = %q, want the production endpoint", DefaultAPIBaseURL)
	}
}

// An explicit base URL always wins over the environment.
func TestNewClient_ExplicitBaseURLBeatsEnv(t *testing.T) {
	t.Setenv("HELIX_API_ENDPOINT", "https://env.example")
	if got := NewClient("https://explicit.example", "jwt").APIBaseURL(); got != "https://explicit.example" {
		t.Errorf("APIBaseURL() = %q, want the explicit value", got)
	}
}

func TestGetMe_RequestActuallyGoesToTheDefaultedBaseURL(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": "a"})
	})
	t.Setenv("HELIX_API_ENDPOINT", ts.URL())
	me, err := NewClient("", "jwt").GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe via env base URL: %v", err)
	}
	if me.AgentID != "a" {
		t.Errorf("AgentID = %q", me.AgentID)
	}
}
