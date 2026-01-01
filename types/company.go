package types

// Company represents a company/customer in the system.
type Company struct {
	ID               string            `json:"_id"`
	CompanyName      string            `json:"company_name"`
	BusinessEmail    string            `json:"business_email"`
	BillingEmail     string            `json:"billing_email,omitempty"`
	CustomerType     string            `json:"customer_type"` // "producer", "consumer", or "both"
	Phone            *string           `json:"phone,omitempty"`
	Address          *Address          `json:"address,omitempty"`
	StripeCustomerID *string           `json:"stripe_customer_id,omitempty"`
	AWSCustomerID    *string           `json:"aws_customer_id,omitempty"`
	S3Bucket         string            `json:"s3_bucket,omitempty"`
	KMSKeyID         string            `json:"kms_key_id,omitempty"`
	SNSTopicARN      string            `json:"sns_topic_arn,omitempty"`
	Status           string            `json:"status"` // "provisioning", "active", "inactive", "provisioning_failed"
	Settings         *CompanySettings  `json:"settings,omitempty"`
	Onboarding       *OnboardingInfo   `json:"onboarding,omitempty"`
	Infrastructure   *InfrastructureInfo `json:"infrastructure,omitempty"`
	CreatedAt        string            `json:"created_at"`
	CreatedBy        string            `json:"created_by,omitempty"`
	UpdatedAt        string            `json:"updated_at"`
	UpdatedBy        string            `json:"updated_by,omitempty"`
	DeletedAt        *string           `json:"deleted_at,omitempty"`
	DeletedBy        *string           `json:"deleted_by,omitempty"`
	UserCount        int               `json:"user_count,omitempty"`
	Users            []CompanyUser     `json:"users,omitempty"`
}

// Address represents a physical address.
type Address struct {
	Street     string `json:"street,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// CompanySettings contains company-specific settings.
type CompanySettings struct {
	NotificationsEnabled bool   `json:"notifications_enabled,omitempty"`
	APIRateLimit         int    `json:"api_rate_limit,omitempty"`
	WebhookURL           string `json:"webhook_url,omitempty"`
	DataRetentionDays    int    `json:"data_retention_days,omitempty"`
}

// OnboardingInfo contains customer onboarding details.
type OnboardingInfo struct {
	CompletedAt              *string `json:"completed_at,omitempty"`
	CredentialsPortalURL     string  `json:"credentials_portal_url,omitempty"`
	CredentialsPortalExpires *string `json:"credentials_portal_expires_at,omitempty"`
	OnboardingSource         string  `json:"onboarding_source,omitempty"`
}

// InfrastructureInfo contains provisioned infrastructure details.
type InfrastructureInfo struct {
	IAMUser     string `json:"iam_user,omitempty"`
	S3Bucket    string `json:"s3_bucket,omitempty"`
	KMSKeyID    string `json:"kms_key_id,omitempty"`
	SQSQueueURL string `json:"sqs_queue_url,omitempty"`
	SQSQueueARN string `json:"sqs_queue_arn,omitempty"`
}

// CompanyUser represents a user within a company.
type CompanyUser struct {
	ID          string            `json:"_id"`
	Email       string            `json:"email"`
	FirstName   string            `json:"first_name,omitempty"`
	LastName    string            `json:"last_name,omitempty"`
	Phone       string            `json:"phone,omitempty"`
	CompanyID   string            `json:"company_id"`
	CompanyName string            `json:"company_name,omitempty"`
	Role        string            `json:"role"` // "superadmin", "admin", "member"
	Status      string            `json:"status"` // "active", "inactive", "suspended", "deleted"
	Permissions *UserPermissions  `json:"permissions,omitempty"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at,omitempty"`
}

// UserPermissions defines what actions a user can perform.
type UserPermissions struct {
	CanCreateDatasets  bool `json:"can_create_datasets,omitempty"`
	CanDeleteDatasets  bool `json:"can_delete_datasets,omitempty"`
	CanManageBilling   bool `json:"can_manage_billing,omitempty"`
	CanInviteUsers     bool `json:"can_invite_users,omitempty"`
}

// CreateCompanyRequest is the payload for POST /v1/companies.
type CreateCompanyRequest struct {
	CompanyName   string   `json:"company_name"`
	BusinessEmail string   `json:"business_email"`
	CustomerType  string   `json:"customer_type"` // "producer", "consumer", or "both"
	Phone         *string  `json:"phone,omitempty"`
	Address       *Address `json:"address,omitempty"`
	BillingEmail  string   `json:"billing_email,omitempty"`
	CreatedBy     string   `json:"created_by,omitempty"`
}

// UpdateCompanyRequest is the payload for PATCH /v1/companies/{id}.
type UpdateCompanyRequest struct {
	CompanyName   *string          `json:"company_name,omitempty"`
	BusinessEmail *string          `json:"business_email,omitempty"`
	BillingEmail  *string          `json:"billing_email,omitempty"`
	Phone         *string          `json:"phone,omitempty"`
	Address       *Address         `json:"address,omitempty"`
	CustomerType  *string          `json:"customer_type,omitempty"`
	Status        *string          `json:"status,omitempty"`
	Settings      *CompanySettings `json:"settings,omitempty"`
}

// CompaniesResponse is the response for GET /v1/companies.
type CompaniesResponse struct {
	Companies []Company `json:"companies"`
	Count     int       `json:"count"`
}

// CreateCompanyResponse is the response for POST /v1/companies.
type CreateCompanyResponse struct {
	Success   bool    `json:"success"`
	CompanyID string  `json:"company_id"`
	Company   Company `json:"company"`
}

// InviteUserRequest is the payload for POST /v1/companies/{id}/users.
type InviteUserRequest struct {
	Email       string           `json:"email"`
	FirstName   string           `json:"first_name,omitempty"`
	LastName    string           `json:"last_name,omitempty"`
	Role        string           `json:"role"` // "owner", "member"
	Phone       string           `json:"phone,omitempty"`
	Permissions *UserPermissions `json:"permissions,omitempty"`
	InvitedBy   string           `json:"invited_by,omitempty"`
}

// CompanyUsersResponse is the response for GET /v1/companies/{id}/users.
type CompanyUsersResponse struct {
	Users []CompanyUser `json:"users"`
	Count int           `json:"count"`
}
