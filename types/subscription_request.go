package types

// SubscriptionRequestStatus is the canonical lifecycle state of a
// subscription request. Canonical contract values: pending, approved,
// rejected.
type SubscriptionRequestStatus = string

// Canonical SubscriptionRequestStatus values.
const (
	SubscriptionRequestStatusPending  SubscriptionRequestStatus = "pending"
	SubscriptionRequestStatusApproved SubscriptionRequestStatus = "approved"
	SubscriptionRequestStatusRejected SubscriptionRequestStatus = "rejected"
)

// SubscriptionRequest represents a request from a consumer to access a producer's datasets.
type SubscriptionRequest struct {
	ID            string  `json:"_id"`
	RequestID     string  `json:"request_id"`
	ConsumerID    string  `json:"consumer_id"`
	ConsumerName  string  `json:"consumer_name,omitempty"`
	ConsumerEmail string  `json:"consumer_email,omitempty"`
	ProducerID    string  `json:"producer_id"`
	ProducerName  string  `json:"producer_name,omitempty"`
	DatasetID     *string `json:"dataset_id,omitempty"` // Null for all-datasets access
	Tier          string  `json:"tier"`                 // SubscriptionTier — canonical write value is "free"
	Message       *string `json:"message,omitempty"`
	Status        string  `json:"status"` // SubscriptionRequestStatus: "pending", "approved", "rejected"
	// PriceMonthlyCents is the per-consumer monthly USD-cents price the
	// producer approved this request at (schema: subscription-request
	// price_monthly_cents). nil/absent means the dataset's own marketplace
	// price applies; 0 means a free grant with no Stripe charge. Only set
	// once the request has been approved with an explicit price.
	PriceMonthlyCents *int64  `json:"price_monthly_cents,omitempty"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
	ApprovedAt        *string `json:"approved_at,omitempty"`
	ApprovedBy        *string `json:"approved_by,omitempty"`
	RejectedAt        *string `json:"rejected_at,omitempty"`
	RejectionReason   *string `json:"rejection_reason,omitempty"`
	Notes             *string `json:"notes,omitempty"`
	SubscriptionID    *string `json:"subscription_id,omitempty"` // Set when approved
}

// CreateSubscriptionRequestPayload is the payload for POST /v1/subscription-requests.
type CreateSubscriptionRequestPayload struct {
	ProducerID string  `json:"producer_id"`
	DatasetID  *string `json:"dataset_id,omitempty"` // Null for all-datasets access
	Tier       string  `json:"tier"`
	Message    *string `json:"message,omitempty"`
}

// ApproveRejectPayload is the payload for POST /v1/subscription-requests/{id}.
type ApproveRejectPayload struct {
	Action string  `json:"action"`           // "approve" or "reject"
	Reason *string `json:"reason,omitempty"` // Required for rejection
	Notes  *string `json:"notes,omitempty"`  // Optional notes for approval
}

// SubscriptionRequestsResponse is the response for GET /v1/subscription-requests.
type SubscriptionRequestsResponse struct {
	Requests []SubscriptionRequest `json:"requests"`
	Count    int                   `json:"count"`
}

// ApproveRequestResponse is the response for approving a subscription request.
type ApproveRequestResponse struct {
	Request      SubscriptionRequest `json:"request"`
	Subscription *Subscription       `json:"subscription,omitempty"`
}

// CreateSubscriptionRequestInput is the input for Consumer.CreateSubscriptionRequest.
type CreateSubscriptionRequestInput struct {
	ProducerID string  // Required: ID of the producer to request access from
	DatasetID  *string // Optional: Specific dataset ID (nil for all-datasets access)
	Tier       string  // Optional: SubscriptionTier (defaults to "free", the canonical write value)
	Message    *string // Optional: Message to the producer
}

// ApproveSubscriptionRequestOptions contains options for approving a subscription request.
type ApproveSubscriptionRequestOptions struct {
	Notes     *string // Optional: Internal notes about the approval
	DatasetID *string // Optional: Specific dataset ID to grant access to

	// PriceMonthlyCents sets the per-consumer monthly USD-cents price for
	// THIS approval, overriding the dataset's own marketplace price for
	// this one consumer. Pointer semantics matter and are validated
	// client-side before any request is sent:
	//   - nil (absent):   the dataset's own marketplace price applies.
	//   - pointer to 0:    a free grant — provisioned immediately, even on
	//                      a paid dataset (how a producer comps a specific
	//                      consumer).
	//   - pointer to > 0: the request moves to approved_pending_payment —
	//                      the consumer must complete checkout, even on a
	//                      dataset that is otherwise free.
	//   - negative:        rejected client-side as a *ValidationError; the
	//                      server also rejects it (400) but the SDK fails
	//                      fast without a network round trip.
	PriceMonthlyCents *int64
}
