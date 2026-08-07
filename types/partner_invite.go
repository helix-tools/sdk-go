package types

// InviteConsumerInput is the request body for POST /v1/self/invite-consumer.
// It matches invite-consumer-request.schema.json (sdk-schemas).
//
// Required: CompanyName (2-200 chars), BusinessEmail (valid email, max 254
// chars), and EITHER Datasets OR DatasetTiers (never both) with 1-50
// unique, non-blank dataset IDs that belong to the inviting producer.
// Optional: ContactName (max 200 chars) and Tier — currently only "free"
// is supported; empty defaults to "free" server-side.
type InviteConsumerInput struct {
	CompanyName   string `json:"company_name"`
	BusinessEmail string `json:"business_email"`
	ContactName   string `json:"contact_name,omitempty"`
	Tier          string `json:"tier,omitempty"` // SubscriptionTier — only "free" is currently supported

	// Datasets is the LEGACY id-only form: a flat list of dataset ids, all
	// granted at the invite-wide Tier above. Mutually exclusive with
	// DatasetTiers — set exactly one of the two.
	Datasets []string `json:"datasets"`

	// DatasetTiers is the per-dataset form: each dataset gets its own tier,
	// which wins over the invite-wide Tier. "free" comps the consumer even
	// on a paid dataset; "paid" is rejected server-side on a non-paid
	// dataset. Mutually exclusive with Datasets — set exactly one of the
	// two. Not marshalled directly (json:"-"): Producer.InviteConsumer
	// serializes it onto the wire "datasets" key as an array of
	// {dataset_id, tier} objects, per invite-consumer-request.schema.json's
	// oneOf.
	DatasetTiers []InviteConsumerDatasetGrant `json:"-"`
}

// InviteConsumerDatasetGrant is one entry of the per-dataset-tier form of
// InviteConsumerInput.DatasetTiers. Tier is "free" (comp this consumer,
// allowed even on a paid dataset) or "paid" (rejected server-side unless
// the dataset itself is priced); empty defaults to "free" server-side,
// matching the invite-wide Tier default.
type InviteConsumerDatasetGrant struct {
	DatasetID string `json:"dataset_id"`
	Tier      string `json:"tier,omitempty"`
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
