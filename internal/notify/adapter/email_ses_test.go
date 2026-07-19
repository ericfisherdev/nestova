package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// ---------------------------------------------------------------------------
// Send — exercised against a local fake SES v2 endpoint, mirroring
// sms_aws_test.go's identical approach: SES v2 has no open-source-emulatable
// equivalent to MinIO either, so these tests point the sesv2 client at an
// in-process httptest.Server via the SDK's own BaseEndpoint override
// (sesv2.Options, identical mechanism to pinpointsmsvoicev2's), serving
// hand-built responses that match the exact wire shape the SDK's generated
// deserializers expect (verified by reading deserializers.go directly: SES
// v2 uses the SAME restJson1 protocol as pinpointsmsvoicev2 — X-Amzn-ErrorType
// resolves the error type, and awsRestjson1_deserializeErrorMessageRejected
// decodes the body into *types.MessageRejected).
// ---------------------------------------------------------------------------

// newTestEmailSender builds an SESEmailSender whose client talks to server
// instead of real AWS, with retries disabled (aws.NopRetryer) so a test
// exercising a failure response completes in one round trip instead of
// waiting through the SDK's real backoff schedule.
func newTestEmailSender(server *httptest.Server) *SESEmailSender {
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	client := sesv2.NewFromConfig(cfg, func(o *sesv2.Options) {
		o.BaseEndpoint = aws.String(server.URL)
	})
	return &SESEmailSender{client: client, fromAddress: "sender@example.com"}
}

func TestSESEmailSender_Send_Success(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusOK, nil, `{"MessageId":"test-message-id-123"}`)
	sender := newTestEmailSender(srv)

	id, err := sender.Send(context.Background(), "recipient@example.com", "Subject", "<p>html</p>", "text")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "test-message-id-123" {
		t.Errorf("providerMessageID = %q, want %q", id, "test-message-id-123")
	}
}

func TestSESEmailSender_Send_Rejected(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusBadRequest,
		map[string]string{"X-Amzn-ErrorType": "MessageRejected"},
		`{"message":"Email address is not verified. The following identities failed the check in region US-EAST-1: recipient@example.com"}`)
	sender := newTestEmailSender(srv)

	_, err := sender.Send(context.Background(), "recipient@example.com", "Subject", "<p>html</p>", "text")
	if !errors.Is(err, domain.ErrRecipientRejected) {
		t.Errorf("Send(unverified recipient): err = %v, want ErrRecipientRejected", err)
	}
}

// TestSESEmailSender_Send_SenderIdentityRejected_NotRecipientRejected is
// the CodeRabbit round-2 regression test (major finding #1): AWS's own
// docs confirm the identical "Email address is not verified" text and
// MessageRejected type fire for a misconfigured SENDER identity too, not
// just an unverified recipient. That must NOT be reported as
// ErrRecipientRejected — the recipient here did nothing wrong, and
// downgrading their preferences over an operator-side sender
// misconfiguration would be exactly backwards.
func TestSESEmailSender_Send_SenderIdentityRejected_NotRecipientRejected(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusBadRequest,
		map[string]string{"X-Amzn-ErrorType": "MessageRejected"},
		`{"message":"Email address is not verified. The following identities failed the check in region US-EAST-1: sender@example.com"}`)
	sender := newTestEmailSender(srv) // fromAddress is "sender@example.com"

	_, err := sender.Send(context.Background(), "recipient@example.com", "Subject", "<p>html</p>", "text")
	if err == nil {
		t.Fatal("Send(sender identity rejected) must still return an error")
	}
	if errors.Is(err, domain.ErrRecipientRejected) {
		t.Error("a rejection naming the SENDER address, not the recipient, must not be reported as ErrRecipientRejected")
	}
}

// TestSESEmailSender_Send_InvalidContentRejected_NotRecipientRejected
// covers MessageRejected's OTHER documented cause — invalid message
// content — which names neither address and must likewise not be
// reported as a recipient rejection.
func TestSESEmailSender_Send_InvalidContentRejected_NotRecipientRejected(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusBadRequest,
		map[string]string{"X-Amzn-ErrorType": "MessageRejected"},
		`{"message":"The message contains invalid content"}`)
	sender := newTestEmailSender(srv)

	_, err := sender.Send(context.Background(), "recipient@example.com", "Subject", "<p>html</p>", "text")
	if err == nil {
		t.Fatal("Send(invalid content) must still return an error")
	}
	if errors.Is(err, domain.ErrRecipientRejected) {
		t.Error("an invalid-content rejection naming no address must not be reported as ErrRecipientRejected")
	}
}

func TestSESEmailSender_Send_OtherFailure_Wrapped(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusInternalServerError,
		map[string]string{"X-Amzn-ErrorType": "InternalServiceErrorException"},
		`{"message":"internal error"}`)
	sender := newTestEmailSender(srv)

	_, err := sender.Send(context.Background(), "recipient@example.com", "Subject", "<p>html</p>", "text")
	if err == nil {
		t.Fatal("Send(internal server error) must return an error")
	}
	if errors.Is(err, domain.ErrRecipientRejected) {
		t.Error("a generic internal server error must not be misreported as ErrRecipientRejected")
	}
}

// TestSESEmailSender_Send_WiresSubjectAndBothBodies proves Send passes
// subject/htmlBody/textBody through to the wire request exactly as given —
// SendEmailInput's Simple message shape (Subject, Body.Html, Body.Text) is
// already verified against the SDK's own types via `go doc`; this confirms
// the adapter actually populates every one of those fields, not just some.
func TestSESEmailSender_Send_WiresSubjectAndBothBodies(t *testing.T) {
	var payload struct {
		FromEmailAddress string
		Destination      struct {
			ToAddresses []string
		}
		Content struct {
			Simple struct {
				Subject struct {
					Data string
				}
				Body struct {
					HTML struct {
						Data string
					}
					Text struct {
						Data string
					}
				}
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"MessageId":"id"}`))
	}))
	t.Cleanup(srv.Close)
	sender := newTestEmailSender(srv)

	if _, err := sender.Send(context.Background(), "recipient@example.com", "Test Subject", "<p>HTML body</p>", "text body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if payload.FromEmailAddress != "sender@example.com" {
		t.Errorf("FromEmailAddress = %q, want %q", payload.FromEmailAddress, "sender@example.com")
	}
	if len(payload.Destination.ToAddresses) != 1 || payload.Destination.ToAddresses[0] != "recipient@example.com" {
		t.Errorf("Destination.ToAddresses = %v, want [recipient@example.com]", payload.Destination.ToAddresses)
	}
	if payload.Content.Simple.Subject.Data != "Test Subject" {
		t.Errorf("Subject.Data = %q, want %q", payload.Content.Simple.Subject.Data, "Test Subject")
	}
	if payload.Content.Simple.Body.HTML.Data != "<p>HTML body</p>" {
		t.Errorf("Body.HTML.Data = %q, want %q", payload.Content.Simple.Body.HTML.Data, "<p>HTML body</p>")
	}
	if payload.Content.Simple.Body.Text.Data != "text body" {
		t.Errorf("Body.Text.Data = %q, want %q", payload.Content.Simple.Body.Text.Data, "text body")
	}
}

// ---------------------------------------------------------------------------
// NewSESEmailSender — guard-clause validation, no AWS dependency (these all
// return before any network/credential call).
// ---------------------------------------------------------------------------

func TestNewSESEmailSender_RejectsInvalidParams(t *testing.T) {
	valid := SESEmailParams{
		Region:      "us-east-1",
		FromAddress: "sender@example.com",
	}

	tests := []struct {
		name    string
		mutate  func(p SESEmailParams) SESEmailParams
		wantErr string
	}{
		{
			name:    "blank region",
			mutate:  func(p SESEmailParams) SESEmailParams { p.Region = ""; return p },
			wantErr: "region",
		},
		{
			name:    "blank from address",
			mutate:  func(p SESEmailParams) SESEmailParams { p.FromAddress = ""; return p },
			wantErr: "from address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSESEmailSender(context.Background(), tt.mutate(valid))
			if err == nil {
				t.Fatal("NewSESEmailSender: want an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewSESEmailSender error = %q, want it to mention %q", err.Error(), tt.wantErr)
			}
		})
	}
}
