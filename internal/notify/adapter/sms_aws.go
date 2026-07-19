package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2"
	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2/types"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// AWSEndUserMessagingSMSParams configures NewAWSEndUserMessagingSender
// (NES-138). It mirrors config.SMSConfig field-for-field but is its own
// type: an adapter package depends on configuration only through the
// composition root passing plain values in, never by importing
// internal/platform/config directly (DIP — mirrors
// media/adapter.S3Params' identical reasoning).
type AWSEndUserMessagingSMSParams struct {
	// Region is required.
	Region string
	// OriginationIdentity is the verified toll-free number (or its ARN, or
	// a pool id/ARN) SendTextMessage sends from.
	OriginationIdentity string
	// AccessKeyID / SecretAccessKey are optional static credentials; when
	// both are blank, the AWS SDK's default credential chain (environment,
	// shared config/credentials file, EC2/ECS instance role, etc.)
	// supplies credentials instead — mirrors S3Params' identical field
	// pair and its own doc.
	AccessKeyID     string
	SecretAccessKey string
	// RetryMaxAttempts caps the SDK's own built-in retryer (exponential
	// backoff + jitter on throttling/5xx — already handled by the SDK, see
	// this type's own doc for why no backoff is hand-rolled here). Kept
	// tight deliberately: SMS is billed per attempt handed to the carrier,
	// so an unbounded retry loop against a persistently failing
	// destination is a real spend risk, not just a latency one.
	RetryMaxAttempts int
}

// AWSEndUserMessagingSender is a domain.SMSSender backed by AWS End User
// Messaging (the successor service to Amazon Pinpoint SMS) via
// aws-sdk-go-v2/service/pinpointsmsvoicev2's SendTextMessage.
//
// Retries: configured through the SDK's OWN built-in retryer
// (RetryMaxAttempts) rather than a hand-rolled backoff loop — the SDK
// already retries throttling and 5xx responses with exponential backoff
// and jitter; duplicating that logic here would only risk getting it
// subtly wrong. RetryMaxAttempts bounds how many attempts the SDK makes
// PER Send call before giving up and returning the last error.
//
// Opted-out destinations
// (types.ConflictExceptionReasonDestinationPhoneNumberOptedOut) are mapped
// to domain.ErrRecipientOptedOut and are never retried by the SDK itself
// either — a 409 conflict, not a throttling/5xx condition the SDK's
// retryer targets — see Send's own doc.
type AWSEndUserMessagingSender struct {
	client              *pinpointsmsvoicev2.Client
	originationIdentity string
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.SMSSender = (*AWSEndUserMessagingSender)(nil)

// NewAWSEndUserMessagingSender builds an AWSEndUserMessagingSender against
// params. Unlike media/adapter.NewS3PhotoStore, this does not make a
// startup reachability call: pinpointsmsvoicev2 has no equivalent of S3's
// cheap, side-effect-free HeadBucket — the closest read call (e.g.
// DescribePhoneNumbers) costs real API quota for no real safety benefit —
// so a misconfigured Region/OriginationIdentity is instead caught by the
// FIRST real SendTextMessage attempt, surfaced through the normal
// notification-failure path (Outbox.MarkFailed) rather than blocking boot.
func NewAWSEndUserMessagingSender(ctx context.Context, params AWSEndUserMessagingSMSParams) (*AWSEndUserMessagingSender, error) {
	switch {
	case strings.TrimSpace(params.Region) == "":
		return nil, errors.New("notify/adapter: sms sender region must not be blank")
	case strings.TrimSpace(params.OriginationIdentity) == "":
		return nil, errors.New("notify/adapter: sms sender origination identity must not be blank")
	case params.RetryMaxAttempts <= 0:
		return nil, fmt.Errorf("notify/adapter: sms sender retry max attempts must be positive, got %d", params.RetryMaxAttempts)
	}

	optFns := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(params.Region),
		awsconfig.WithRetryMaxAttempts(params.RetryMaxAttempts),
	}
	if params.AccessKeyID != "" && params.SecretAccessKey != "" {
		optFns = append(optFns, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(params.AccessKeyID, params.SecretAccessKey, ""),
		))
	}
	// Otherwise the SDK's default credential chain applies unchanged — see
	// AWSEndUserMessagingSMSParams.AccessKeyID's own doc.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("notify/adapter: load AWS config: %w", err)
	}

	return &AWSEndUserMessagingSender{
		client:              pinpointsmsvoicev2.NewFromConfig(awsCfg),
		originationIdentity: params.OriginationIdentity,
	}, nil
}

// Send truncates body to a single SMS segment under whichever encoding the
// carrier will use for it (see truncateSMSBody — GSM-7 or UCS-2, each with
// its own capacity) and sends it to `to` via SendTextMessage as a
// TRANSACTIONAL message —
// household notifications (chore reminders, security codes, spend alerts)
// are all time-sensitive/operational, never marketing, so
// MessageTypeTransactional is used unconditionally rather than exposed as
// a per-call option.
//
// Returns domain.ErrRecipientOptedOut when AWS reports the destination has
// opted out (a ConflictException with reason
// DESTINATION_PHONE_NUMBER_OPTED_OUT) — never retried, since the carrier
// will not deliver to that number no matter the attempt count. Any other
// failure is wrapped and returned after the SDK's own configured retry
// budget (RetryMaxAttempts) is exhausted.
func (s *AWSEndUserMessagingSender) Send(ctx context.Context, to domain.E164Phone, body string) (string, error) {
	out, err := s.client.SendTextMessage(ctx, &pinpointsmsvoicev2.SendTextMessageInput{
		DestinationPhoneNumber: aws.String(to.String()),
		MessageBody:            aws.String(truncateSMSBody(body)),
		OriginationIdentity:    aws.String(s.originationIdentity),
		MessageType:            types.MessageTypeTransactional,
	})
	if err != nil {
		var conflictErr *types.ConflictException
		if errors.As(err, &conflictErr) && conflictErr.Reason == types.ConflictExceptionReasonDestinationPhoneNumberOptedOut {
			return "", domain.ErrRecipientOptedOut
		}
		return "", fmt.Errorf("notify/adapter: send sms: %w", err)
	}
	if out.MessageId == nil {
		return "", nil
	}
	return *out.MessageId, nil
}
