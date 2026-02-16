package types

// Subscription represents an active subscription to a dataset or producer.
type Subscription struct {
	ID               string  `json:"_id"`
	ConsumerID       string  `json:"consumer_id"`
	CustomerID       string  `json:"customer_id,omitempty"` // Legacy field
	DatasetID        *string `json:"dataset_id"` // Required field, null for all-datasets subscription
	DatasetName      string  `json:"dataset_name,omitempty"`
	ProducerID       string  `json:"producer_id"`
	RequestID        string  `json:"request_id,omitempty"`
	Tier             string  `json:"tier"` // "basic", "professional", "enterprise"
	Status           string  `json:"status"` // "active", "suspended", "cancelled", "expired"
	KMSGrantID       *string `json:"kms_grant_id,omitempty"`
	SNSSubscriptionARN *string `json:"sns_subscription_arn,omitempty"`
	SQSQueueARN      *string `json:"sqs_queue_arn,omitempty"`
	SQSQueueURL      *string `json:"sqs_queue_url,omitempty"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
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
	Tier      string `json:"tier,omitempty"` // Defaults to "basic"
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
	ConsumerID        string             `json:"consumer_id"`
	ConsumerName      string             `json:"consumer_name"`
	ConsumerEmail     string             `json:"consumer_email"`
	SubscriptionCount int                `json:"subscription_count"`
	Datasets          []SubscriberDataset `json:"datasets"`
	FirstSubscribedAt string             `json:"first_subscribed_at"`
	LastSubscribedAt  string             `json:"last_subscribed_at"`
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
