package producer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/helix-tools/sdk-go/v2/types"
)

// inviteEmailRegex mirrors the Go API's server-side email check for
// invite-consumer requests (self/validation.go).
var inviteEmailRegex = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// ValidationError represents a client-side validation failure detected
// before any request is sent to the API. Callers can use errors.As to
// distinguish it from an *APIError returned by the server.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error: %s: %s", e.Field, e.Message)
}

// validateInviteConsumerInput mirrors the server-side
// ValidateInviteConsumerRequest rules (self/validation.go) so obviously
// invalid invites fail fast without a network round trip. Datasets and
// DatasetTiers are the two mutually-exclusive dataset forms
// (invite-consumer-request.schema.json's oneOf) — exactly one must be set.
func validateInviteConsumerInput(input types.InviteConsumerInput) error {
	if input.CompanyName == "" {
		return &ValidationError{Field: "company_name", Message: "company_name is required"}
	}
	if len(input.CompanyName) < 2 || len(input.CompanyName) > 200 {
		return &ValidationError{Field: "company_name", Message: "must be between 2 and 200 characters"}
	}
	if input.BusinessEmail == "" {
		return &ValidationError{Field: "business_email", Message: "business_email is required"}
	}
	if len(input.BusinessEmail) > 254 || !inviteEmailRegex.MatchString(input.BusinessEmail) {
		return &ValidationError{Field: "business_email", Message: "invalid email format"}
	}
	if input.ContactName != "" && len(input.ContactName) > 200 {
		return &ValidationError{Field: "contact_name", Message: "must be at most 200 characters"}
	}
	if input.Tier != "" && input.Tier != "free" {
		return &ValidationError{Field: "tier", Message: `must be "free" (paid tiers not yet supported)`}
	}

	usingLegacy := len(input.Datasets) > 0
	usingTiers := len(input.DatasetTiers) > 0

	if usingLegacy && usingTiers {
		return &ValidationError{Field: "datasets", Message: "cannot set both datasets and dataset_tiers; use exactly one of the two forms"}
	}
	if !usingLegacy && !usingTiers {
		return &ValidationError{Field: "datasets", Message: "must contain at least 1 dataset"}
	}

	if usingLegacy {
		if len(input.Datasets) > 50 {
			return &ValidationError{Field: "datasets", Message: "must contain at most 50 datasets"}
		}

		seen := make(map[string]struct{}, len(input.Datasets))
		for _, d := range input.Datasets {
			trimmed := strings.TrimSpace(d)
			if trimmed == "" {
				return &ValidationError{Field: "datasets", Message: "dataset id cannot be empty or whitespace-only"}
			}
			if _, dup := seen[trimmed]; dup {
				return &ValidationError{Field: "datasets", Message: fmt.Sprintf("duplicate dataset id %q", trimmed)}
			}
			seen[trimmed] = struct{}{}
		}

		return nil
	}

	// usingTiers: the per-dataset {dataset_id, tier} form.
	if len(input.DatasetTiers) > 50 {
		return &ValidationError{Field: "dataset_tiers", Message: "must contain at most 50 datasets"}
	}

	seen := make(map[string]struct{}, len(input.DatasetTiers))
	for _, grant := range input.DatasetTiers {
		trimmed := strings.TrimSpace(grant.DatasetID)
		if trimmed == "" {
			return &ValidationError{Field: "dataset_tiers", Message: "dataset id cannot be empty or whitespace-only"}
		}
		if _, dup := seen[trimmed]; dup {
			return &ValidationError{Field: "dataset_tiers", Message: fmt.Sprintf("duplicate dataset id %q", trimmed)}
		}
		seen[trimmed] = struct{}{}

		if grant.Tier != "" && grant.Tier != "free" && grant.Tier != "paid" {
			return &ValidationError{Field: "dataset_tiers", Message: fmt.Sprintf(`invalid tier %q for dataset %q: must be "free" or "paid"`, grant.Tier, trimmed)}
		}
	}

	return nil
}

// inviteConsumerWireBody is the actual JSON shape POSTed to
// /v1/self/invite-consumer. types.InviteConsumerInput is the Go-facing
// input type (with its two mutually-exclusive Datasets/DatasetTiers
// fields); this unexported type is what puts EXACTLY ONE shape — a legacy
// []string or a per-dataset-tier []object — onto the wire "datasets" key,
// per invite-consumer-request.schema.json's oneOf. Datasets is `any` so
// json.Marshal picks whichever concrete slice type InviteConsumer sets.
type inviteConsumerWireBody struct {
	CompanyName   string `json:"company_name"`
	BusinessEmail string `json:"business_email"`
	ContactName   string `json:"contact_name,omitempty"`
	Tier          string `json:"tier,omitempty"`
	Datasets      any    `json:"datasets"`
}

// InviteConsumer invites a consumer partner company to the platform,
// auto-granting access to the given datasets at invite time. Datasets can
// be granted either as a flat list of ids (input.Datasets, all at the
// invite-wide input.Tier) or per-dataset (input.DatasetTiers, each dataset
// carrying its own tier — "free" comps that consumer even on a paid
// dataset). Set exactly one of the two; setting both is rejected
// client-side.
//
// The server provisions the consumer account asynchronously and sends a
// welcome email; check InviteConsumerResponse.EmailSent (and EmailError)
// to know whether the email dispatch succeeded — provisioning is not
// rolled back on email failure.
//
// This endpoint is gated behind the partner_invite feature flag; when the
// flag is not enabled for the producer, the server responds 403 and the
// error surfaces as an *APIError with StatusCode 403.
//
// Obviously invalid input (empty company name, no datasets, more than 50
// datasets, a duplicate dataset id, an invalid per-dataset tier, etc.)
// fails fast client-side with a *ValidationError before any request is sent.
func (p *Producer) InviteConsumer(ctx context.Context, input types.InviteConsumerInput) (*types.InviteConsumerResponse, error) {
	if err := validateInviteConsumerInput(input); err != nil {
		return nil, err
	}

	wire := inviteConsumerWireBody{
		CompanyName:   input.CompanyName,
		BusinessEmail: input.BusinessEmail,
		ContactName:   input.ContactName,
		Tier:          input.Tier,
	}

	// Send CANONICAL (trimmed) dataset/dataset-tier ids — validation trims for
	// its checks, so the wire payload must trim too or `[" ds-1 "]` reaches
	// the server with spaces and diverges from the canonical grant (matches
	// Python/TS + the server's own trim in ValidateInviteConsumerRequest).
	// Exactly one of the two forms is non-empty (validated above); the wire
	// "datasets" key carries whichever one the caller used — NEVER both, so
	// the array is never a mix of strings and objects.
	if len(input.DatasetTiers) > 0 {
		grants := make([]types.InviteConsumerDatasetGrant, len(input.DatasetTiers))
		for i, g := range input.DatasetTiers {
			grants[i] = types.InviteConsumerDatasetGrant{
				DatasetID: strings.TrimSpace(g.DatasetID),
				Tier:      g.Tier,
			}
		}
		wire.Datasets = grants
	} else {
		datasets := make([]string, len(input.Datasets))
		for i, d := range input.Datasets {
			datasets[i] = strings.TrimSpace(d)
		}
		wire.Datasets = datasets
	}

	var result types.InviteConsumerResponse
	if err := p.makeAPIRequest(ctx, http.MethodPost, "/v1/self/invite-consumer", wire, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListConsumers lists the consumer partners this producer has invited,
// including deactivated ones (DeactivatedAt is set when deactivated).
//
// This endpoint is gated behind the partner_invite feature flag; when the
// flag is not enabled for the producer, the server responds 403 and the
// error surfaces as an *APIError with StatusCode 403.
func (p *Producer) ListConsumers(ctx context.Context) ([]types.ProducerConsumerRelation, error) {
	var response types.ListConsumersResponse
	if err := p.makeAPIRequest(ctx, http.MethodGet, "/v1/self/consumers", nil, &response); err != nil {
		return nil, err
	}

	return response.Consumers, nil
}

// DeactivateConsumer deactivates a consumer partner previously invited by
// this producer, revoking their access.
//
// This endpoint is gated behind the partner_invite feature flag; when the
// flag is not enabled for the producer, the server responds 403 and the
// error surfaces as an *APIError with StatusCode 403.
func (p *Producer) DeactivateConsumer(ctx context.Context, consumerID string) (*types.DeactivateConsumerResponse, error) {
	consumerID = strings.TrimSpace(consumerID)
	if consumerID == "" {
		return nil, &ValidationError{Field: "consumer_id", Message: "consumer id is required"}
	}

	// Escape the TRIMMED id — escaping the raw arg would turn "  id  " into a
	// %20-padded path segment that never matches the intended consumer.
	path := fmt.Sprintf("/v1/self/consumers/%s/deactivate", url.PathEscape(consumerID))

	var result types.DeactivateConsumerResponse
	if err := p.makeAPIRequest(ctx, http.MethodPatch, path, nil, &result); err != nil {
		return nil, err
	}

	return &result, nil
}
