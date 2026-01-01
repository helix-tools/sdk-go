package types

// SubscriptionRequest represents a request from a consumer to access a producer's datasets.
type SubscriptionRequest struct {
	ID              string  `json:"_id"`
	RequestID       string  `json:"request_id"`
	ConsumerID      string  `json:"consumer_id"`
	ConsumerName    string  `json:"consumer_name,omitempty"`
	ConsumerEmail   string  `json:"consumer_email,omitempty"`
	ProducerID      string  `json:"producer_id"`
	ProducerName    string  `json:"producer_name,omitempty"`
	DatasetID       *string `json:"dataset_id,omitempty"` // Null for all-datasets access
	Tier            string  `json:"tier"` // "free", "basic", "premium", "professional", "enterprise"
	Message         *string `json:"message,omitempty"`
	Status          string  `json:"status"` // "pending", "approved", "rejected", "cancelled"
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	ApprovedAt      *string `json:"approved_at,omitempty"`
	ApprovedBy      *string `json:"approved_by,omitempty"`
	RejectedAt      *string `json:"rejected_at,omitempty"`
	RejectionReason *string `json:"rejection_reason,omitempty"`
	Notes           *string `json:"notes,omitempty"`
	SubscriptionID  *string `json:"subscription_id,omitempty"` // Set when approved
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
	Action string  `json:"action"` // "approve" or "reject"
	Reason *string `json:"reason,omitempty"` // Required for rejection
	Notes  *string `json:"notes,omitempty"` // Optional notes for approval
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
