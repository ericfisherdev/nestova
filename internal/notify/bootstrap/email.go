package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// NewEmailSender builds the domain.EmailSender this deployment uses
// (NES-141): NoopEmailSender when emailCfg.Enabled is false (the
// default — zero AWS dependency, see NoopEmailSender's own doc), or an
// SESEmailSender, instrumented with recorder, when true. Mirrors
// NewSMSSender's identical structure and reasoning — see that
// function's own doc for why the instrumentation wrap happens HERE, not
// inside SESEmailSender itself, and why the Noop sender is deliberately
// NOT wrapped.
//
// ctx bounds AWS config loading (LoadDefaultConfig may reach out to the
// EC2/ECS instance metadata service to resolve credentials) — the caller
// is expected to derive it with a bounded timeout, mirroring
// NewSMSSender's identical ctx contract.
func NewEmailSender(ctx context.Context, emailCfg config.EmailConfig, recorder metrics.EmailRecorder, logger *slog.Logger) (notifydomain.EmailSender, error) {
	if !emailCfg.Enabled {
		return notifyadapter.NewNoopEmailSender(logger), nil
	}
	// A nil recorder is only safe on the Noop path above, which never
	// wraps in instrumentedEmailSender at all. Once email is enabled,
	// every Send call reaches instrumentedEmailSender.Send, which calls a
	// method on recorder unconditionally — a nil recorder would panic
	// there, at send time, rather than failing fast here at construction.
	if recorder == nil {
		return nil, errors.New("email recorder must not be nil when email is enabled")
	}

	sender, err := notifyadapter.NewSESEmailSender(ctx, notifyadapter.SESEmailParams{
		Region:          emailCfg.Region,
		FromAddress:     emailCfg.FromAddress,
		AccessKeyID:     emailCfg.AccessKeyID,
		SecretAccessKey: emailCfg.SecretAccessKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create ses email sender: %w", err)
	}
	return newInstrumentedEmailSender(sender, recorder), nil
}
