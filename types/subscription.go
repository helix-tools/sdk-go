package types

// SubscriptionStatus is the canonical lifecycle state of a subscription.
// Canonical contract values: active, paused, cancelled, expired.
type SubscriptionStatus = string

// Canonical SubscriptionStatus values. These mirror the company.schema.json /
// Go API source of truth exactly — there is no "suspended" or "inactive"
// subscription status (those were stale comment artifacts).
const (
	SubscriptionStatusActive    SubscriptionStatus = "active"
	SubscriptionStatusPaused    SubscriptionStatus = "paused"
	SubscriptionStatusCancelled SubscriptionStatus = "cancelled"
	SubscriptionStatusExpired   SubscriptionStatus = "expired"
)

// SubscriptionTier is the access tier on a subscription and on a subscription
// request. Its only value is "free", as the subscription schema defines it.
type SubscriptionTier = string

// Canonical SubscriptionTier values.
const (
	TierFree SubscriptionTier = "free"
)

// ConsumerInfo is the server-side enrichment of the consumer that owns a
// subscription (subscription.consumer_info). Populated by the API on read
// paths (e.g. GET /v1/subscriptions) so producers can render their
// Subscribers tab without an N+1 fan-out per consumer_id. Mirrors the
// producer-side producer_info enrichment.
type ConsumerInfo struct {
	CompanyName string `json:"company_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

// ProducerInfo is the server-side enrichment of the producer behind a
// subscription (subscription.producer_info). It is where the producer's name
// arrives on GET /v1/subscriptions: the wire has no producer_name key.
type ProducerInfo struct {
	CompanyName string `json:"company_name,omitempty"`
}

// DatasetInfo is the server-side enrichment of the dataset a subscription
// points at (subscription.dataset_info). Populated by the API on read paths
// (e.g. GET /v1/subscriptions) so a consumer can show when the dataset was
// last uploaded and how large it is without an N+1 fetch per dataset_id.
// Every key is optional; the object is exactly these five keys.
type DatasetInfo struct {
	Name        string `json:"name,omitempty"`
	LastUpdated string `json:"last_updated,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	RecordCount int64  `json:"record_count,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
}

// Subscription represents an active subscription to a dataset or producer.
type Subscription struct {
	ID          string             `json:"_id"`
	ConsumerID  string             `json:"consumer_id"`
	CustomerID  string             `json:"customer_id,omitempty"` // Legacy field
	DatasetID   *string            `json:"dataset_id"`            // Required field, null for all-datasets subscription
	DatasetName string             `json:"dataset_name,omitempty"`
	ProducerID  string             `json:"producer_id"`
	RequestID   string             `json:"request_id,omitempty"`
	Tier        SubscriptionTier   `json:"tier"`   // "free" is the only value
	Status      SubscriptionStatus `json:"status"` // "active", "paused", "cancelled", "expired"
	SQSQueueURL *string            `json:"sqs_queue_url,omitempty"`
	// ProducerInfo is optional server-side enrichment carrying the producer's
	// company name; absent unless the API populated it on this read path.
	ProducerInfo *ProducerInfo `json:"producer_info,omitempty"`
	// ConsumerInfo is optional server-side enrichment; absent unless the API
	// populated it on this read path.
	ConsumerInfo *ConsumerInfo `json:"consumer_info,omitempty"`
	// DatasetInfo is optional server-side enrichment of the subscribed
	// dataset; absent unless the API populated it on this read path, and nil
	// when dataset_id is null (an all-datasets subscription) or unresolved.
	DatasetInfo *DatasetInfo `json:"dataset_info,omitempty"`
	// AccessCount, AccessesThisMonth, MonthlyAccessCap, RemainingAccesses and
	// LastAccessedAt are the usage-counter enrichment on a subscription,
	// populated by the API on read and never incremented by this SDK. All
	// five are optional: absent on a response served by an API build
	// predating usage counters. Pointers so an explicit 0 (a real "no
	// downloads yet" or "no accesses remaining") is distinct from absent —
	// never a fabricated 0.
	// AccessCount is the total downloads recorded for this subscription.
	AccessCount *int64 `json:"access_count,omitempty"`
	// AccessesThisMonth is downloads so far in the current UTC calendar
	// month; it resets when the month rolls over.
	AccessesThisMonth *int64 `json:"accesses_this_month,omitempty"`
	// MonthlyAccessCap is the maximum downloads allowed per UTC calendar
	// month for this subscription.
	MonthlyAccessCap *int64 `json:"monthly_access_cap,omitempty"`
	// RemainingAccesses is downloads remaining this month before
	// MonthlyAccessCap is reached. -1 = unlimited (no monthly cap).
	RemainingAccesses *int64 `json:"remaining_accesses,omitempty"`
	// LastAccessedAt is when the consumer last downloaded from this
	// subscription, RFC 3339, absent if the consumer has never downloaded.
	// Carried as *string (not *time.Time) so it round-trips any RFC 3339
	// value the API emits byte-for-byte and never fails decoding on a
	// nonconforming timestamp — the same pattern this SDK already uses for
	// its other *_at timestamp fields (e.g. ApprovedAt/RejectedAt in
	// subscription_request.go).
	LastAccessedAt *string `json:"last_accessed_at,omitempty"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	// Billing is the marketplace billing (payment) state.
	// Optional: free/legacy subscriptions omit it or carry billing_status
	// "free". Distinct from Status (the ACCESS state). Tolerate absence (nil).
	Billing *SubscriptionBilling `json:"billing,omitempty"`
	// AutoRenew reports whether the subscription renews at the end of its term.
	// The API omits it when false, so a decoded false also means "not set".
	AutoRenew bool `json:"auto_renew,omitempty"`
	// CancellationReason is the free-text reason recorded when the subscription
	// was cancelled. Empty unless one was recorded.
	CancellationReason string `json:"cancellation_reason,omitempty"`
	// SubscribedAt is when the subscription was created, RFC 3339. The API's
	// response name for the stored created_at. Empty when the API omits it.
	SubscribedAt string `json:"subscribed_at,omitempty"`
}

// SubscriptionsResponse is the response for GET /v1/subscriptions.
type SubscriptionsResponse struct {
	Subscriptions []Subscription `json:"subscriptions"`
	Count         int            `json:"count"`
}

// CreateSubscriptionRequest is the payload for POST /v1/subscriptions.
// Note: Subscriptions are typically created through subscription request approval.
type CreateSubscriptionRequest struct {
	DatasetID string `json:"dataset_id"`
	Tier      string `json:"tier,omitempty"` // SubscriptionTier — defaults to "free" (canonical write value)
}

// RevokeSubscriptionResponse is the response for PUT /v1/subscriptions/{id}/revoke.
type RevokeSubscriptionResponse struct {
	Message        string `json:"message"`
	SubscriptionID string `json:"subscription_id"`
	Status         string `json:"status"`
}

// SubscribersResponse is the response for GET /v1/producers/subscribers.
type SubscribersResponse struct {
	Subscribers []Subscriber `json:"subscribers"`
	Count       int          `json:"count"`
}

// Subscriber represents a consumer who has subscribed to the producer's datasets.
type Subscriber struct {
	ConsumerID        string              `json:"consumer_id"`
	ConsumerName      string              `json:"consumer_name"`
	ConsumerEmail     string              `json:"consumer_email"`
	SubscriptionCount int                 `json:"subscription_count"`
	Datasets          []SubscriberDataset `json:"datasets"`
	FirstSubscribedAt string              `json:"first_subscribed_at"`
	LastSubscribedAt  string              `json:"last_subscribed_at"`
}

// SubscriberDataset represents a dataset that a subscriber has access to.
type SubscriberDataset struct {
	SubscriptionID string `json:"subscription_id"`
	DatasetID      string `json:"dataset_id"`
	DatasetName    string `json:"dataset_name"`
	Tier           string `json:"tier"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
}
