// Tests that the AWS-SDK calls this package makes directly (KMS Decrypt,
// SQS ReceiveMessage/DeleteMessage/PurgeQueue) sanitize a genuine
// no-response failure (a refused connection, DNS, a timeout) the same way
// every SDK-owned HTTP call site already does via sdkerr.SanitizeCause: the
// endpoint host/URL must not be reachable through the error chain. A
// responding-service error (an AWS API error with a real status) is a
// different case, covered separately in error_cause_test.go, and must stay
// unchanged — this file never touches that path.
package consumer

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// noResponseEndpoint is a closed port: every call against it gets no HTTP
// response at all (connection refused), the same shape a real outage, DNS
// failure, or deadline takes for SanitizeCause's purposes.
const noResponseEndpoint = "http://127.0.0.1:1"

// fakeSQSUnreachable points an SQS client at a closed port.
func fakeSQSUnreachable(t *testing.T) *sqs.Client {
	t.Helper()
	return sqs.New(sqs.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(noResponseEndpoint),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRETTEST", ""),
		RetryMaxAttempts: 1,
	})
}

// assertNoResponseCauseSanitized confirms err carries no *url.Error
// reachable via errors.As (SanitizeCause's own guarantee for a
// transport-shaped failure), and that a sanitized stand-in cause is still
// reachable for debugging without the endpoint host anywhere in the chain.
func assertNoResponseCauseSanitized(t *testing.T, err error, neverContains string) {
	t.Helper()
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, *url.Error) = true, want the no-response cause sanitized away: %v", urlErr)
	}
	assertSanitizedCauseReachable(t, err, neverContains)
}

// ----------------------------------------------------------------------------
// decryptData (KMS Decrypt).
// ----------------------------------------------------------------------------

func TestDecryptData_KMSNoResponseCauseIsSanitized(t *testing.T) {
	c := newTestConsumer("https://api.test")
	useFakeKMS(c, noResponseEndpoint, nil)

	_, err := c.decryptData(context.Background(), encryptedObject([]byte("row\n")))

	assertClean(t, err, "decryption failed")
	assertNoResponseCauseSanitized(t, err, "127.0.0.1")
}

// TestDecryptData_KMSNoResponseNegativeControl proves the fabricated
// no-response KMS call really does carry the endpoint host, reachable via
// errors.As(*url.Error) before sanitizing — exactly the leak the
// verification report found — so a green assertion above is not vacuous.
func TestDecryptData_KMSNoResponseNegativeControl(t *testing.T) {
	c := &Consumer{}
	useFakeKMS(c, noResponseEndpoint, nil)

	_, rawErr := c.kmsClient.Decrypt(context.Background(), &kms.DecryptInput{
		CiphertextBlob: []byte("wrapped-key"),
	})
	if rawErr == nil {
		t.Fatal("expected the refused connection to fail")
	}

	var urlErr *url.Error
	if !errors.As(rawErr, &urlErr) {
		t.Fatalf("expected rawErr to carry a *url.Error, got %T: %v", rawErr, rawErr)
	}
	if !strings.Contains(urlErr.Error(), "127.0.0.1:1") {
		t.Fatalf("negative control did not reproduce the leak: %v", urlErr)
	}
}

// ----------------------------------------------------------------------------
// PollNotifications / DeleteNotification / ClearQueue (SQS).
// ----------------------------------------------------------------------------

func TestPollNotifications_SQSNoResponseCauseIsSanitized(t *testing.T) {
	c := newTestConsumer("https://api.test")
	queue := "https://sqs.example/q-1"
	c.queueURL = &queue
	c.sqsClient = fakeSQSUnreachable(t)

	_, err := c.PollNotifications(context.Background(), PollNotificationsOptions{ShortPoll: true})

	assertClean(t, err, "failed to poll SQS queue")
	assertNoResponseCauseSanitized(t, err, "127.0.0.1")
}

func TestDeleteNotification_SQSNoResponseCauseIsSanitized(t *testing.T) {
	c := newTestConsumer("https://api.test")
	queue := "https://sqs.example/q-1"
	c.queueURL = &queue
	c.sqsClient = fakeSQSUnreachable(t)

	err := c.DeleteNotification(context.Background(), "receipt-1")

	assertClean(t, err, "failed to delete notification")
	assertNoResponseCauseSanitized(t, err, "127.0.0.1")
}

func TestClearQueue_SQSNoResponseCauseIsSanitized(t *testing.T) {
	c := newTestConsumer("https://api.test")
	queue := "https://sqs.example/q-1"
	c.queueURL = &queue
	c.sqsClient = fakeSQSUnreachable(t)

	err := c.ClearQueue(context.Background())

	assertClean(t, err, "failed to clear queue")
	assertNoResponseCauseSanitized(t, err, "127.0.0.1")
}

// TestSQSNoResponseNegativeControl proves the fabricated no-response SQS
// call really does carry the endpoint host, reachable via
// errors.As(*url.Error) before sanitizing — the same mechanism underlies
// ReceiveMessage, DeleteMessage, and PurgeQueue, so one reproduction of the
// raw leak covers all three call sites above.
func TestSQSNoResponseNegativeControl(t *testing.T) {
	sqsClient := fakeSQSUnreachable(t)

	_, rawErr := sqsClient.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
		QueueUrl: aws.String("https://sqs.example/q-1"),
	})
	if rawErr == nil {
		t.Fatal("expected the refused connection to fail")
	}

	var urlErr *url.Error
	if !errors.As(rawErr, &urlErr) {
		t.Fatalf("expected rawErr to carry a *url.Error, got %T: %v", rawErr, rawErr)
	}
	if !strings.Contains(urlErr.Error(), "127.0.0.1:1") {
		t.Fatalf("negative control did not reproduce the leak: %v", urlErr)
	}
}
