package types

// InviteConsumerInput is the request body for POST /v1/self/invite-consumer.
// It matches invite-consumer-request.schema.json (sdk-schemas).
//
// Required: CompanyName (2-200 chars), BusinessEmail (valid email, max 254
// chars), and Datasets (1-50 unique, non-blank dataset IDs that belong to
// the inviting producer). Optional: ContactName (max 200 chars) and Tier —
// currently only "free" is supported; empty defaults to "free" server-side.
type InviteConsumerInput struct {
	CompanyName   string   `json:"company_name"`
	BusinessEmail string   `json:"business_email"`
	ContactName   string   `json:"contact_name,omitempty"`
	Tier          string   `json:"tier,omitempty"` // SubscriptionTier — only "free" is currently supported
	Datasets      []string `json:"datasets"`
}

// InviteConsumerResponse is the response for POST /v1/self/invite-consumer.
// It matches invite-consumer-response.schema.json plus the email-dispatch
// fields from the Go API source of truth (self.InviteConsumerResponse).
//
// EmailSent / EmailError surface the welcome-email dispatch outcome so the
// producer can tell whether the invite actually reached the partner. The
// invite still returns success (Status "provisioning") on email failure —
// provisioning is not rolled back — but EmailSent=false (plus EmailError
// context) makes a failed dispatch visible.
type InviteConsumerResponse struct {
	ConsumerID      string   `json:"consumer_id"`
	CompanyName     string   `json:"company_name"`
	Status          string   `json:"status"` // "provisioning" or "active"
	InvitedBy       string   `json:"invited_by"`
	DatasetsGranted []string `json:"datasets_granted,omitempty"`
	Message         string   `json:"message"`
	EmailSent       bool     `json:"email_sent"`
	EmailError      string   `json:"email_error,omitempty"`
}

// ProducerConsumerRelation is one producer→consumer partner relation, as
// returned by GET /v1/self/consumers. It matches the Go API source of
// truth (self.ProducerConsumerRelation).
type ProducerConsumerRelation struct {
	ConsumerID      string   `json:"consumer_id"`
	CompanyName     string   `json:"company_name,omitempty"`
	BusinessEmail   string   `json:"business_email,omitempty"`
	Status          string   `json:"status,omitempty"`
	Tier            string   `json:"tier,omitempty"` // SubscriptionTier — canonical write value is "free"
	ProducerID      string   `json:"producer_id"`
	InvitedAt       string   `json:"invited_at"`
	DeactivatedAt   *string  `json:"deactivated_at,omitempty"`
	DatasetsGranted []string `json:"datasets_granted,omitempty"`
}

// ListConsumersResponse is the envelope for GET /v1/self/consumers.
type ListConsumersResponse struct {
	Consumers []ProducerConsumerRelation `json:"consumers"`
	Count     int                        `json:"count"`
}

// DeactivateConsumerResponse is the response for
// PATCH /v1/self/consumers/{id}/deactivate.
type DeactivateConsumerResponse struct {
	ConsumerID string `json:"consumer_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}
