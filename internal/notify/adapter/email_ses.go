package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// SESEmailParams configures NewSESEmailSender (NES-141). It mirrors
// config.EmailConfig field-for-field but is its own type: an adapter
// package depends on configuration only through the composition root
// passing plain values in, never by importing internal/platform/config
// directly (DIP — mirrors AWSEndUserMessagingSMSParams' identical
// reasoning).
type SESEmailParams struct {
	// Region is required.
	Region string
	// FromAddress is the verified sending address SendEmail sends from —
	// in this deployment's SES sandbox scope, one of the four verified
	// family addresses (see docs/aws-email.md).
	FromAddress string
	// AccessKeyID / SecretAccessKey are optional static credentials; when
	// both are blank, the AWS SDK's default credential chain (environment,
	// shared config/credentials file, EC2/ECS instance role, etc.)
	// supplies credentials instead — mirrors AWSEndUserMessagingSMSParams'
	// identical field pair and its own doc.
	AccessKeyID     string
	SecretAccessKey string
}

// sesRetryMaxAttempts caps the SDK's own built-in retryer for every SES
// send. Unlike SMS (billed per carrier attempt handed off, so kept tight
// and configurable), SES sandbox sends are effectively free at family
// volume (docs/aws-email.md), so a fixed, generous default is used here
// rather than exposing another environment variable nobody needs to tune
// yet.
const sesRetryMaxAttempts = 3

// SESEmailSender is a domain.EmailSender backed by Amazon SES v2's
// SendEmail (NES-141). It sends a "Simple" message (a subject plus
// separate HTML/text bodies) rather than a raw MIME message or a
// template — neither attachments nor personalization tags are needed for
// a generic notification email (see the web/components email templates'
// own doc).
//
// Retries: configured through the SDK's OWN built-in retryer, mirroring
// AWSEndUserMessagingSender's identical reasoning (see that type's own
// doc) — the SDK already retries throttling and 5xx responses with
// exponential backoff and jitter; duplicating that logic here would only
// risk getting it subtly wrong.
//
// Rejections: SES's MessageRejected error carries no structured reason
// code the way pinpointsmsvoicev2's ConflictException does for an
// opted-out SMS destination (confirmed via `go doc` — the type carries
// only a free-text Message, no reason enum), and — critically — AWS's own
// docs confirm MessageRejected's dominant real-world text, "Email address
// is not verified. The following identities failed the check...", fires
// IDENTICALLY whether the unverified identity is the DESTINATION address
// or this deployment's OWN From/Source/Sender/Return-Path address
// (https://docs.aws.amazon.com/ses/latest/dg/troubleshoot-error-messages.html:
// "This applies to 'From', 'Source', 'Sender', or 'Return-Path'
// addresses. In the sandbox, all recipient addresses must also be
// verified..."). Blindly mapping every MessageRejected to
// domain.ErrRecipientRejected would therefore let an operator's OWN
// misconfiguration (e.g. FromAddress drifting out of verification)
// trigger EmailNotificationSender's bounce handling — downgrading a
// perfectly good member's preferences and warning owners — over a
// problem that has nothing to do with that member's address at all.
// isDestinationRejection (below) narrows the mapping to cases where the
// error message actually NAMES the destination address being sent to;
// every other MessageRejected (a misconfigured sender identity, invalid
// message content, etc.) is instead wrapped and returned as a plain
// failure — still terminal (SES's retryer only retries throttling/5xx,
// never MessageRejected), just not one that implicates the recipient.
type SESEmailSender struct {
	client      *sesv2.Client
	fromAddress string
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.EmailSender = (*SESEmailSender)(nil)

// NewSESEmailSender builds an SESEmailSender against params. Like
// NewAWSEndUserMessagingSender, this does not make a startup
// reachability call: SES has no equivalent of S3's cheap, side-effect-free
// HeadBucket, so a misconfigured Region/FromAddress is instead caught by
// the FIRST real SendEmail attempt, surfaced through the normal
// notification-failure path (Outbox.MarkFailed) rather than blocking
// boot.
func NewSESEmailSender(ctx context.Context, params SESEmailParams) (*SESEmailSender, error) {
	switch {
	case strings.TrimSpace(params.Region) == "":
		return nil, errors.New("notify/adapter: email sender region must not be blank")
	case strings.TrimSpace(params.FromAddress) == "":
		return nil, errors.New("notify/adapter: email sender from address must not be blank")
	}

	optFns := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(params.Region),
		awsconfig.WithRetryMaxAttempts(sesRetryMaxAttempts),
	}
	if params.AccessKeyID != "" && params.SecretAccessKey != "" {
		optFns = append(optFns, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(params.AccessKeyID, params.SecretAccessKey, ""),
		))
	}
	// Otherwise the SDK's default credential chain applies unchanged — see
	// SESEmailParams.AccessKeyID's own doc.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("notify/adapter: load AWS config: %w", err)
	}

	return &SESEmailSender{
		client:      sesv2.NewFromConfig(awsCfg),
		fromAddress: params.FromAddress,
	}, nil
}

// Send sends a Simple email message — subject, HTML body, and text body —
// to the single destination address `to` via SendEmail.
//
// Returns domain.ErrRecipientRejected only when SES reports MessageRejected
// AND the error text actually names `to` (see isDestinationRejection and
// this type's own doc). Every other failure — including a MessageRejected
// that does NOT name the destination — is wrapped and returned as a plain
// error, after the SDK's own configured retry budget (sesRetryMaxAttempts)
// is exhausted for the retryable classes it covers.
func (s *SESEmailSender) Send(ctx context.Context, to, subject, htmlBody, textBody string) (string, error) {
	out, err := s.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.fromAddress),
		Destination: &types.Destination{
			ToAddresses: []string{to},
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: aws.String(subject)},
				Body: &types.Body{
					Html: &types.Content{Data: aws.String(htmlBody)},
					Text: &types.Content{Data: aws.String(textBody)},
				},
			},
		},
	})
	if err != nil {
		var rejected *types.MessageRejected
		if errors.As(err, &rejected) && isDestinationRejection(rejected, to) {
			return "", domain.ErrRecipientRejected
		}
		return "", fmt.Errorf("notify/adapter: send email: %w", err)
	}
	if out.MessageId == nil {
		return "", nil
	}
	return *out.MessageId, nil
}

// isDestinationRejection reports whether rejected's own message
// specifically names `to` as a failed identity — the only reliable way
// (given MessageRejected's lack of a structured reason field) to tell
// "this destination address itself was rejected" apart from "our own
// sender identity is misconfigured" or "the message content was invalid,
// unrelated to either address" (see Send's own doc for why both of THOSE
// must NOT trigger EmailNotificationSender's bounce handling). Requiring
// BOTH the documented "not verified" phrase AND the destination address
// to appear guards against an incidental substring match in some
// unrelated MessageRejected text.
//
// This heuristic is inherently coupled to AWS's current free-text wording
// for MessageRejected, which carries no structured reason code. If this
// deployment ever grows past sandbox/family scale, prefer SES event
// publishing (SNS bounce/complaint notifications) for a structured,
// wording-independent signal instead — see the graduation-path note in
// docs/aws-email.md.
func isDestinationRejection(rejected *types.MessageRejected, to string) bool {
	if rejected.Message == nil {
		return false
	}
	msg := *rejected.Message
	return strings.Contains(msg, "not verified") && strings.Contains(msg, to)
}
