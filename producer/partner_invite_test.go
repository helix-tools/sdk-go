package producer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helix-tools/sdk-go/v2/types"
)

// validInviteInput returns a minimal valid invite payload for reuse across
// tests. Each call returns a fresh value so tests can mutate it safely.
func validInviteInput() types.InviteConsumerInput {
	return types.InviteConsumerInput{
		CompanyName:   "Acme Partner Co",
		BusinessEmail: "partners@acme.example.com",
		Datasets:      []string{"ds-1", "ds-2"},
	}
}

// TestInviteConsumer_HappyPath drives the real InviteConsumer method against
// a mocked transport and pins:
//   - the request is POST /v1/self/invite-consumer,
//   - the body carries exactly the schema's snake_case fields, and
//   - the decoded *types.InviteConsumerResponse is returned.
func TestInviteConsumer_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"consumer_id": "customer-abc123",
			"company_name": "Acme Partner Co",
			"status": "provisioning",
			"invited_by": "test-producer",
			"datasets_granted": ["ds-1", "ds-2"],
			"message": "Consumer invited; provisioning started",
			"email_sent": true
		}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	input := validInviteInput()
	input.ContactName = "Jane Doe"
	input.Tier = "free"

	resp, err := p.InviteConsumer(context.Background(), input)
	if err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/self/invite-consumer" {
		t.Errorf("expected path /v1/self/invite-consumer, got %q", gotPath)
	}

	// Exact wire body: the schema's snake_case keys, nothing else.
	if gotBody["company_name"] != "Acme Partner Co" {
		t.Errorf("expected company_name in body, got %v", gotBody["company_name"])
	}
	if gotBody["business_email"] != "partners@acme.example.com" {
		t.Errorf("expected business_email in body, got %v", gotBody["business_email"])
	}
	if gotBody["contact_name"] != "Jane Doe" {
		t.Errorf("expected contact_name in body, got %v", gotBody["contact_name"])
	}
	if gotBody["tier"] != "free" {
		t.Errorf("expected tier=free in body, got %v", gotBody["tier"])
	}
	datasets, ok := gotBody["datasets"].([]any)
	if !ok || len(datasets) != 2 || datasets[0] != "ds-1" || datasets[1] != "ds-2" {
		t.Errorf("expected datasets [ds-1 ds-2] in body, got %v", gotBody["datasets"])
	}
	if len(gotBody) != 5 {
		t.Errorf("expected exactly 5 keys in body, got %d: %+v", len(gotBody), gotBody)
	}

	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.ConsumerID != "customer-abc123" {
		t.Errorf("expected consumer_id customer-abc123, got %q", resp.ConsumerID)
	}
	if resp.CompanyName != "Acme Partner Co" {
		t.Errorf("expected company_name Acme Partner Co, got %q", resp.CompanyName)
	}
	if resp.Status != "provisioning" {
		t.Errorf("expected status provisioning, got %q", resp.Status)
	}
	if resp.InvitedBy != "test-producer" {
		t.Errorf("expected invited_by test-producer, got %q", resp.InvitedBy)
	}
	if len(resp.DatasetsGranted) != 2 || resp.DatasetsGranted[0] != "ds-1" || resp.DatasetsGranted[1] != "ds-2" {
		t.Errorf("expected datasets_granted [ds-1 ds-2], got %v", resp.DatasetsGranted)
	}
	if !resp.EmailSent {
		t.Error("expected email_sent=true")
	}
	if resp.EmailError != "" {
		t.Errorf("expected empty email_error, got %q", resp.EmailError)
	}
}

// TestInviteConsumer_OmitsOptionalFields pins omitempty behavior: unset
// contact_name and tier must be absent from the wire body.
func TestInviteConsumer_OmitsOptionalFields(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"consumer_id": "customer-abc123",
			"status": "provisioning",
			"invited_by": "test-producer",
			"email_sent": true
		}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.InviteConsumer(context.Background(), validInviteInput()); err != nil {
		t.Fatalf("InviteConsumer: %v", err)
	}

	if _, present := gotBody["contact_name"]; present {
		t.Errorf("unset contact_name should be omitted from body, got %+v", gotBody)
	}
	if _, present := gotBody["tier"]; present {
		t.Errorf("unset tier should be omitted from body, got %+v", gotBody)
	}
	if len(gotBody) != 3 {
		t.Errorf("expected exactly 3 keys (company_name, business_email, datasets), got %d: %+v", len(gotBody), gotBody)
	}
}

// TestInviteConsumer_ValidationRejects covers client-side validation: each
// invalid input must fail with a *ValidationError on the right field and
// must NOT hit the transport.
func TestInviteConsumer_ValidationRejects(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	tests := []struct {
		name      string
		mutate    func(*types.InviteConsumerInput)
		wantField string
	}{
		{
			name:      "empty company name",
			mutate:    func(in *types.InviteConsumerInput) { in.CompanyName = "" },
			wantField: "company_name",
		},
		{
			name:      "company name too short",
			mutate:    func(in *types.InviteConsumerInput) { in.CompanyName = "A" },
			wantField: "company_name",
		},
		{
			name:      "company name too long",
			mutate:    func(in *types.InviteConsumerInput) { in.CompanyName = strings.Repeat("x", 201) },
			wantField: "company_name",
		},
		{
			name:      "empty business email",
			mutate:    func(in *types.InviteConsumerInput) { in.BusinessEmail = "" },
			wantField: "business_email",
		},
		{
			name:      "invalid business email",
			mutate:    func(in *types.InviteConsumerInput) { in.BusinessEmail = "not-an-email" },
			wantField: "business_email",
		},
		{
			name: "business email too long",
			mutate: func(in *types.InviteConsumerInput) {
				in.BusinessEmail = strings.Repeat("a", 250) + "@x.io"
			},
			wantField: "business_email",
		},
		{
			name:      "contact name too long",
			mutate:    func(in *types.InviteConsumerInput) { in.ContactName = strings.Repeat("x", 201) },
			wantField: "contact_name",
		},
		{
			name:      "unsupported tier",
			mutate:    func(in *types.InviteConsumerInput) { in.Tier = "premium" },
			wantField: "tier",
		},
		{
			name:      "empty datasets",
			mutate:    func(in *types.InviteConsumerInput) { in.Datasets = nil },
			wantField: "datasets",
		},
		{
			name: "more than 50 datasets",
			mutate: func(in *types.InviteConsumerInput) {
				ds := make([]string, 51)
				for i := range ds {
					ds[i] = "ds-" + strings.Repeat("x", i+1)
				}
				in.Datasets = ds
			},
			wantField: "datasets",
		},
		{
			name:      "whitespace-only dataset id",
			mutate:    func(in *types.InviteConsumerInput) { in.Datasets = []string{"ds-1", "   "} },
			wantField: "datasets",
		},
		{
			name:      "duplicate dataset id",
			mutate:    func(in *types.InviteConsumerInput) { in.Datasets = []string{"ds-1", "ds-1"} },
			wantField: "datasets",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validInviteInput()
			tt.mutate(&input)

			resp, err := p.InviteConsumer(context.Background(), input)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if resp != nil {
				t.Errorf("expected nil response on validation failure, got %+v", resp)
			}

			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("expected *ValidationError, got %T: %v", err, err)
			}
			if vErr.Field != tt.wantField {
				t.Errorf("expected field %q, got %q (%v)", tt.wantField, vErr.Field, err)
			}
		})
	}

	if requests != 0 {
		t.Errorf("validation failures must not hit the API, got %d request(s)", requests)
	}
}

// TestInviteConsumer_FeatureFlagForbidden pins that a 403 from the
// partner_invite feature-flag gate surfaces cleanly as *APIError.
func TestInviteConsumer_FeatureFlagForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"feature partner_invite is not enabled for this account"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.InviteConsumer(context.Background(), validInviteInput())
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
	if resp != nil {
		t.Errorf("expected nil response on 403, got %+v", resp)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "partner_invite") {
		t.Errorf("expected server error body to pass through, got %q", apiErr.Body)
	}
}

// TestInviteConsumer_EmailSentFalsePassthrough pins that a successful invite
// with a failed welcome-email dispatch (email_sent=false + email_error) is
// returned as success with both fields readable.
func TestInviteConsumer_EmailSentFalsePassthrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"consumer_id": "customer-abc123",
			"company_name": "Acme Partner Co",
			"status": "provisioning",
			"invited_by": "test-producer",
			"datasets_granted": ["ds-1"],
			"message": "Consumer invited but welcome email failed",
			"email_sent": false,
			"email_error": "SES rejected recipient"
		}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.InviteConsumer(context.Background(), validInviteInput())
	if err != nil {
		t.Fatalf("email failure must not fail the invite, got: %v", err)
	}
	if resp.EmailSent {
		t.Error("expected email_sent=false")
	}
	if resp.EmailError != "SES rejected recipient" {
		t.Errorf("expected email_error passthrough, got %q", resp.EmailError)
	}
	if resp.Status != "provisioning" {
		t.Errorf("expected status provisioning, got %q", resp.Status)
	}
}

// TestListConsumers_HappyPath drives the real ListConsumers method and pins:
//   - the request is GET /v1/self/consumers with no body, and
//   - the {consumers, count} envelope is unwrapped to a slice.
func TestListConsumers_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBodyLen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBodyLen = len(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"consumers": [
				{
					"consumer_id": "customer-111",
					"company_name": "Active Partner",
					"business_email": "a@partner.example.com",
					"status": "active",
					"tier": "free",
					"producer_id": "test-producer",
					"invited_at": "2026-07-01T00:00:00Z",
					"datasets_granted": ["ds-1"]
				},
				{
					"consumer_id": "customer-222",
					"company_name": "Former Partner",
					"status": "inactive",
					"producer_id": "test-producer",
					"invited_at": "2026-06-01T00:00:00Z",
					"deactivated_at": "2026-06-15T00:00:00Z"
				}
			],
			"count": 2
		}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	consumers, err := p.ListConsumers(context.Background())
	if err != nil {
		t.Fatalf("ListConsumers: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/v1/self/consumers" {
		t.Errorf("expected path /v1/self/consumers, got %q", gotPath)
	}
	if gotBodyLen != 0 {
		t.Errorf("expected empty request body, got %d bytes", gotBodyLen)
	}

	if len(consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %d", len(consumers))
	}

	active := consumers[0]
	if active.ConsumerID != "customer-111" || active.CompanyName != "Active Partner" {
		t.Errorf("unexpected first consumer: %+v", active)
	}
	if active.Status != "active" || active.Tier != "free" {
		t.Errorf("unexpected first consumer status/tier: %+v", active)
	}
	if active.ProducerID != "test-producer" || active.InvitedAt != "2026-07-01T00:00:00Z" {
		t.Errorf("unexpected first consumer producer/invited_at: %+v", active)
	}
	if active.DeactivatedAt != nil {
		t.Errorf("active consumer should have nil deactivated_at, got %v", *active.DeactivatedAt)
	}
	if len(active.DatasetsGranted) != 1 || active.DatasetsGranted[0] != "ds-1" {
		t.Errorf("unexpected first consumer datasets_granted: %v", active.DatasetsGranted)
	}

	former := consumers[1]
	if former.ConsumerID != "customer-222" || former.Status != "inactive" {
		t.Errorf("unexpected second consumer: %+v", former)
	}
	if former.DeactivatedAt == nil || *former.DeactivatedAt != "2026-06-15T00:00:00Z" {
		t.Errorf("expected deactivated_at 2026-06-15T00:00:00Z, got %v", former.DeactivatedAt)
	}
}

// TestListConsumers_FeatureFlagForbidden pins the 403 feature-flag gate for
// the list endpoint.
func TestListConsumers_FeatureFlagForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"feature partner_invite is not enabled for this account"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	consumers, err := p.ListConsumers(context.Background())
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
	if consumers != nil {
		t.Errorf("expected nil slice on 403, got %v", consumers)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", apiErr.StatusCode)
	}
}

// TestDeactivateConsumer_HappyPath drives the real DeactivateConsumer method
// and pins:
//   - the request is PATCH /v1/self/consumers/{id}/deactivate with no body,
//   - the id is path-escaped, and
//   - the decoded *types.DeactivateConsumerResponse is returned.
func TestDeactivateConsumer_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBodyLen int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		raw, _ := io.ReadAll(r.Body)
		gotBodyLen = len(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"consumer_id": "customer-abc123",
			"status": "inactive",
			"message": "Consumer deactivated"
		}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.DeactivateConsumer(context.Background(), "customer-abc123")
	if err != nil {
		t.Fatalf("DeactivateConsumer: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("expected PATCH, got %s", gotMethod)
	}
	if gotPath != "/v1/self/consumers/customer-abc123/deactivate" {
		t.Errorf("expected path /v1/self/consumers/customer-abc123/deactivate, got %q", gotPath)
	}
	if gotBodyLen != 0 {
		t.Errorf("expected empty request body, got %d bytes", gotBodyLen)
	}

	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.ConsumerID != "customer-abc123" {
		t.Errorf("expected consumer_id customer-abc123, got %q", resp.ConsumerID)
	}
	if resp.Status != "inactive" {
		t.Errorf("expected status inactive, got %q", resp.Status)
	}
	if resp.Message != "Consumer deactivated" {
		t.Errorf("expected message passthrough, got %q", resp.Message)
	}
}

// TestDeactivateConsumer_PathEscapesID pins that a hostile consumer id
// cannot break out of the path segment (mirrors url.PathEscape usage in
// sibling methods like UpdateDataset/RevokeSubscription).
func TestDeactivateConsumer_PathEscapesID(t *testing.T) {
	var gotEscapedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"consumer_id":"x","status":"inactive","message":"ok"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	if _, err := p.DeactivateConsumer(context.Background(), "cust/../admin"); err != nil {
		t.Fatalf("DeactivateConsumer: %v", err)
	}

	if gotEscapedPath != "/v1/self/consumers/cust%2F..%2Fadmin/deactivate" {
		t.Errorf("expected path-escaped id, got %q", gotEscapedPath)
	}
}

// TestDeactivateConsumer_EmptyIDRejected pins client-side validation of the
// consumer id: empty/whitespace ids fail fast without hitting the API.
func TestDeactivateConsumer_EmptyIDRejected(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	for _, id := range []string{"", "   "} {
		resp, err := p.DeactivateConsumer(context.Background(), id)
		if err == nil {
			t.Fatalf("expected validation error for id %q, got nil", id)
		}
		if resp != nil {
			t.Errorf("expected nil response for id %q, got %+v", id, resp)
		}

		var vErr *ValidationError
		if !errors.As(err, &vErr) {
			t.Fatalf("expected *ValidationError for id %q, got %T: %v", id, err, err)
		}
		if vErr.Field != "consumer_id" {
			t.Errorf("expected field consumer_id, got %q", vErr.Field)
		}
	}

	if requests != 0 {
		t.Errorf("validation failures must not hit the API, got %d request(s)", requests)
	}
}

// TestDeactivateConsumer_FeatureFlagForbidden pins the 403 feature-flag gate
// for the deactivate endpoint.
func TestDeactivateConsumer_FeatureFlagForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"feature partner_invite is not enabled for this account"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)

	resp, err := p.DeactivateConsumer(context.Background(), "customer-abc123")
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
	if resp != nil {
		t.Errorf("expected nil response on 403, got %+v", resp)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", apiErr.StatusCode)
	}
}

// TestInviteConsumer_TrimsDatasetsOnWire pins that dataset ids are CANONICALIZED
// (trimmed) in the POST body, not just for validation. codex 2026-07-06 caught
// that validation trimmed but the raw slice was sent, so `[" ds-1 "]` reached
// the server with spaces and risked a failed grant.
func TestInviteConsumer_TrimsDatasetsOnWire(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"consumer_id":"c-1","status":"provisioning","invited_by":"p","company_name":"X","message":"ok","email_sent":true}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	_, err := p.InviteConsumer(context.Background(), types.InviteConsumerInput{
		CompanyName:   "Acme Partner Co",
		BusinessEmail: "partners@acme.example.com",
		Datasets:      []string{"  ds-1  ", "\tds-2\n"},
		Tier:          "free",
	})
	if err != nil {
		t.Fatalf("InviteConsumer returned error: %v", err)
	}
	datasets, ok := gotBody["datasets"].([]any)
	if !ok || len(datasets) != 2 {
		t.Fatalf("expected 2 datasets on the wire, got %v", gotBody["datasets"])
	}
	if datasets[0] != "ds-1" || datasets[1] != "ds-2" {
		t.Fatalf("dataset ids not trimmed on the wire: got %v (want [ds-1 ds-2])", datasets)
	}
}

// TestDeactivateConsumer_TrimsIDInPath pins that the TRIMMED consumer id is
// path-escaped, not the raw arg (codex 2026-07-06: raw escape turned a padded
// id into a %20-laden path that never matches).
func TestDeactivateConsumer_TrimsIDInPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"consumer_id":"customer-abc123","status":"inactive"}`))
	}))
	defer server.Close()

	p := newTestProducer(server.URL)
	_, err := p.DeactivateConsumer(context.Background(), "  customer-abc123  ")
	if err != nil {
		t.Fatalf("DeactivateConsumer returned error: %v", err)
	}
	want := "/v1/self/consumers/customer-abc123/deactivate"
	if gotPath != want {
		t.Fatalf("expected trimmed path %q, got %q", want, gotPath)
	}
}
