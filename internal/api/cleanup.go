package api

import (
	"context"
	"sync"
	"testing"
)

// CleanupFunc defines a cleanup function that is called during test teardown.
type CleanupFunc func(ctx context.Context) error

// CleanupRegistry tracks resources created during tests for cleanup.
// Cleanup functions are executed in LIFO (Last-In-First-Out) order,
// ensuring dependent resources are cleaned up before their dependencies.
type CleanupRegistry struct {
	mu       sync.Mutex
	cleanups []CleanupFunc
	t        *testing.T
}

// NewCleanupRegistry creates a new cleanup registry for a test.
func NewCleanupRegistry(t *testing.T) *CleanupRegistry {
	return &CleanupRegistry{
		cleanups: make([]CleanupFunc, 0),
		t:        t,
	}
}

// Register adds a cleanup function to be called during teardown.
// Functions are executed in reverse order (LIFO).
func (r *CleanupRegistry) Register(fn CleanupFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cleanups = append(r.cleanups, fn)
}

// RunAll executes all cleanup functions in reverse order (LIFO).
// Errors are logged but do not stop subsequent cleanups.
func (r *CleanupRegistry) RunAll(ctx context.Context) []error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errors []error

	// Execute in reverse order (LIFO).
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		if err := r.cleanups[i](ctx); err != nil {
			errors = append(errors, err)
			r.t.Logf("Cleanup error: %v", err)
		}
	}

	// Clear the cleanup list.
	r.cleanups = nil

	return errors
}

// Count returns the number of registered cleanup functions.
func (r *CleanupRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.cleanups)
}

// RegisterCompanyCleanup registers a cleanup function to delete a company.
func (r *CleanupRegistry) RegisterCompanyCleanup(client *Client, companyID string) {
	r.Register(func(ctx context.Context) error {
		r.t.Logf("Cleaning up company: %s", companyID)

		err := client.Delete(ctx, "/v1/companies/"+companyID)
		if err != nil && !IsNotFoundError(err) {
			return err
		}

		return nil
	})
}

// RegisterDatasetCleanup registers a cleanup function to delete a dataset.
func (r *CleanupRegistry) RegisterDatasetCleanup(client *Client, datasetID string) {
	r.Register(func(ctx context.Context) error {
		r.t.Logf("Cleaning up dataset: %s", datasetID)

		// Note: Dataset deletion might not be supported by the API.
		// This is a placeholder for when it becomes available.
		err := client.Delete(ctx, "/v1/datasets/"+datasetID)
		if err != nil && !IsNotFoundError(err) {
			// Log but don't fail if deletion is not supported.
			r.t.Logf("Dataset cleanup warning: %v", err)
		}

		return nil
	})
}

// RegisterSubscriptionCleanup registers a cleanup function to revoke a subscription.
func (r *CleanupRegistry) RegisterSubscriptionCleanup(client *Client, subscriptionID string) {
	r.Register(func(ctx context.Context) error {
		r.t.Logf("Cleaning up subscription (revoking): %s", subscriptionID)

		err := client.Put(ctx, "/v1/subscriptions/"+subscriptionID+"/revoke", map[string]string{}, nil)
		if err != nil && !IsNotFoundError(err) {
			return err
		}

		return nil
	})
}

// RegisterSubscriptionRequestCleanup registers a cleanup function to cancel a subscription request.
func (r *CleanupRegistry) RegisterSubscriptionRequestCleanup(client *Client, requestID string) {
	r.Register(func(ctx context.Context) error {
		r.t.Logf("Cleaning up subscription request: %s", requestID)

		// Subscription requests might not have a delete endpoint.
		// Try to reject it if it's still pending.
		payload := map[string]string{
			"action": "reject",
			"reason": "Test cleanup",
		}

		err := client.Post(ctx, "/v1/subscription-requests/"+requestID, payload, nil)
		if err != nil && !IsNotFoundError(err) {
			// Log but don't fail - request might already be approved/rejected.
			r.t.Logf("Subscription request cleanup warning: %v", err)
		}

		return nil
	})
}
