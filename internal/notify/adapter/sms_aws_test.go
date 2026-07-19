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
	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// ---------------------------------------------------------------------------
// Send — exercised against a local fake AWS End User Messaging endpoint.
//
// AWS End User Messaging has no open-source-emulatable equivalent to MinIO
// (which media/adapter's own S3 tests point at for a real, disposable S3
// server — see photo_store_s3_test.go's own doc), so these tests instead
// point the pinpointsmsvoicev2 client at an in-process httptest.Server via
// the SDK's own BaseEndpoint override (pinpointsmsvoicev2.Options,
// identical mechanism to S3's), serving hand-built responses that match
// the exact wire shape the SDK's generated deserializers expect (verified
// by reading deserializers.go directly: the AWS JSON 1.0 protocol resolves
// the error type from the X-Amzn-ErrorType response header, then decodes
// ConflictException's body via its "Reason" field — see
// awsAwsjson10_deserializeDocumentConflictException).
// ---------------------------------------------------------------------------

// newTestSMSSender builds an AWSEndUserMessagingSender whose client talks
// to server instead of real AWS, with retries disabled (aws.NopRetryer) so
// a test exercising a failure response completes in one round trip instead
// of waiting through the SDK's real backoff schedule.
func newTestSMSSender(server *httptest.Server) *AWSEndUserMessagingSender {
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
	}
	client := pinpointsmsvoicev2.NewFromConfig(cfg, func(o *pinpointsmsvoicev2.Options) {
		o.BaseEndpoint = aws.String(server.URL)
	})
	return &AWSEndUserMessagingSender{client: client, originationIdentity: "test-origination-identity"}
}

// fakeAWSJSONServer returns an httptest.Server that always responds with
// status/body, set to close automatically at the end of t.
func fakeAWSJSONServer(t *testing.T, status int, headers map[string]string, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testE164Phone(t *testing.T) domain.E164Phone {
	t.Helper()
	to, err := domain.ParseE164Phone("+15551234567")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}
	return to
}

func TestAWSEndUserMessagingSender_Send_Success(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusOK, nil, `{"MessageId":"test-message-id-123"}`)
	sender := newTestSMSSender(srv)

	id, err := sender.Send(context.Background(), testE164Phone(t), "Test message")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "test-message-id-123" {
		t.Errorf("providerMessageID = %q, want %q", id, "test-message-id-123")
	}
}

func TestAWSEndUserMessagingSender_Send_OptedOut(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusConflict,
		map[string]string{"X-Amzn-ErrorType": "ConflictException"},
		`{"message":"destination opted out","Reason":"DESTINATION_PHONE_NUMBER_OPTED_OUT"}`)
	sender := newTestSMSSender(srv)

	_, err := sender.Send(context.Background(), testE164Phone(t), "Test message")
	if !errors.Is(err, domain.ErrRecipientOptedOut) {
		t.Errorf("Send(opted-out destination): err = %v, want ErrRecipientOptedOut", err)
	}
}

// TestAWSEndUserMessagingSender_Send_OtherConflict_NotOptedOut proves the
// mapping checks the SPECIFIC reason, not just "any ConflictException" — a
// different conflict (e.g. a message-type mismatch) must not be
// misreported as an opt-out, which would incorrectly suppress a retry a
// genuinely-transient conflict might deserve.
func TestAWSEndUserMessagingSender_Send_OtherConflict_NotOptedOut(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusConflict,
		map[string]string{"X-Amzn-ErrorType": "ConflictException"},
		`{"message":"message type mismatch","Reason":"MESSAGE_TYPE_MISMATCH"}`)
	sender := newTestSMSSender(srv)

	_, err := sender.Send(context.Background(), testE164Phone(t), "Test message")
	if err == nil {
		t.Fatal("Send(conflict) must return an error")
	}
	if errors.Is(err, domain.ErrRecipientOptedOut) {
		t.Errorf("Send(non-opted-out conflict): err = %v, must NOT be ErrRecipientOptedOut", err)
	}
}

func TestAWSEndUserMessagingSender_Send_OtherFailure_Wrapped(t *testing.T) {
	srv := fakeAWSJSONServer(t, http.StatusInternalServerError,
		map[string]string{"X-Amzn-ErrorType": "InternalServerException"},
		`{"message":"internal error"}`)
	sender := newTestSMSSender(srv)

	_, err := sender.Send(context.Background(), testE164Phone(t), "Test message")
	if err == nil {
		t.Fatal("Send(internal server error) must return an error")
	}
	if errors.Is(err, domain.ErrRecipientOptedOut) {
		t.Error("a generic internal server error must not be misreported as ErrRecipientOptedOut")
	}
}

// TestAWSEndUserMessagingSender_Send_TruncatesOversizedBody proves Send
// wires truncateSMSBody's output all the way to the wire request, using a
// plain-ASCII (GSM-7) body — truncateSMSBody's own encoding-aware
// behavior, including the UCS-2 path, is covered directly in
// sms_encoding_test.go without any AWS dependency.
func TestAWSEndUserMessagingSender_Send_TruncatesOversizedBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			MessageBody string
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		gotBody = payload.MessageBody
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"MessageId":"id"}`))
	}))
	t.Cleanup(srv.Close)
	sender := newTestSMSSender(srv)

	oversized := strings.Repeat("a", 200)
	if _, err := sender.Send(context.Background(), testE164Phone(t), oversized); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len([]rune(gotBody)) != maxGSM7Septets {
		t.Errorf("sent body length = %d runes, want %d (one SMS segment, 1 septet per 'a')", len([]rune(gotBody)), maxGSM7Septets)
	}
	if !strings.HasSuffix(gotBody, gsm7Ellipsis) {
		t.Errorf("sent body = %q, want it to end with %q", gotBody, gsm7Ellipsis)
	}
}

// ---------------------------------------------------------------------------
// NewAWSEndUserMessagingSender — guard-clause validation, no AWS
// dependency (these all return before any network/credential call).
// ---------------------------------------------------------------------------

func TestNewAWSEndUserMessagingSender_RejectsInvalidParams(t *testing.T) {
	valid := AWSEndUserMessagingSMSParams{
		Region:              "us-east-1",
		OriginationIdentity: "+18005551234",
		RetryMaxAttempts:    3,
	}

	tests := []struct {
		name    string
		mutate  func(p AWSEndUserMessagingSMSParams) AWSEndUserMessagingSMSParams
		wantErr string
	}{
		{
			name:    "blank region",
			mutate:  func(p AWSEndUserMessagingSMSParams) AWSEndUserMessagingSMSParams { p.Region = ""; return p },
			wantErr: "region",
		},
		{
			name: "blank origination identity",
			mutate: func(p AWSEndUserMessagingSMSParams) AWSEndUserMessagingSMSParams {
				p.OriginationIdentity = ""
				return p
			},
			wantErr: "origination identity",
		},
		{
			name:    "zero retry max attempts",
			mutate:  func(p AWSEndUserMessagingSMSParams) AWSEndUserMessagingSMSParams { p.RetryMaxAttempts = 0; return p },
			wantErr: "retry max attempts",
		},
		{
			name:    "negative retry max attempts",
			mutate:  func(p AWSEndUserMessagingSMSParams) AWSEndUserMessagingSMSParams { p.RetryMaxAttempts = -1; return p },
			wantErr: "retry max attempts",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAWSEndUserMessagingSender(context.Background(), tt.mutate(valid))
			if err == nil {
				t.Fatal("NewAWSEndUserMessagingSender: want an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("NewAWSEndUserMessagingSender error = %q, want it to mention %q", err.Error(), tt.wantErr)
			}
		})
	}
}
