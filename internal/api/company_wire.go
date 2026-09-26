package api

import "github.com/helix-tools/sdk-go/v2/types"

// The admin-only company request/response bodies the harness sends and reads.
// They used to be exported from package types; that surface is deprecated
// there (see types.CreateCompanyRequest) and the harness owns its copies.

// CreateCompanyRequest is the payload for POST /v1/companies.
type CreateCompanyRequest struct {
	CompanyName   string         `json:"company_name"`
	BusinessEmail string         `json:"business_email"`
	CustomerType  string         `json:"customer_type"` // "producer", "consumer", or "both"
	Phone         *string        `json:"phone,omitempty"`
	Address       *types.Address `json:"address,omitempty"`
	BillingEmail  string         `json:"billing_email,omitempty"`
	CreatedBy     string         `json:"created_by,omitempty"`
}

// UpdateCompanyRequest is the payload for PATCH /v1/companies/{id}.
type UpdateCompanyRequest struct {
	CompanyName   *string                `json:"company_name,omitempty"`
	BusinessEmail *string                `json:"business_email,omitempty"`
	BillingEmail  *string                `json:"billing_email,omitempty"`
	Phone         *string                `json:"phone,omitempty"`
	Address       *types.Address         `json:"address,omitempty"`
	CustomerType  *string                `json:"customer_type,omitempty"`
	Status        *string                `json:"status,omitempty"`
	Settings      *types.CompanySettings `json:"settings,omitempty"`
}

// CompaniesResponse is the response for GET /v1/companies.
type CompaniesResponse struct {
	Companies []types.Company `json:"companies"`
	Count     int             `json:"count"`
}

// CreateCompanyResponse is the response for POST /v1/companies.
type CreateCompanyResponse struct {
	Success   bool          `json:"success"`
	CompanyID string        `json:"company_id"`
	Company   types.Company `json:"company"`
}

// InviteUserRequest is the payload for POST /v1/companies/{id}/users.
type InviteUserRequest struct {
	Email       string                 `json:"email"`
	FirstName   string                 `json:"first_name,omitempty"`
	LastName    string                 `json:"last_name,omitempty"`
	Role        string                 `json:"role"` // "owner", "admin", "member"
	Phone       string                 `json:"phone,omitempty"`
	Permissions *types.UserPermissions `json:"permissions,omitempty"`
	InvitedBy   string                 `json:"invited_by,omitempty"`
}

// CompanyUsersResponse is the response for GET /v1/companies/{id}/users.
type CompanyUsersResponse struct {
	Users []types.CompanyUser `json:"users"`
	Count int                 `json:"count"`
}
