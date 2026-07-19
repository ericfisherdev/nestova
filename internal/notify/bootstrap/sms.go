// Package bootstrap builds the notify bounded context's composition-root-only
// dependencies — today, just the SMS sender (NewSMSSender) — so cmd/server
// constructs it from config.SMSConfig without either internal/notify/adapter
// or internal/notify/domain depending on internal/platform/config or
// internal/platform/metrics directly. It is deliberately its own package,
// not folded into internal/notify/adapter, mirroring
// internal/media/bootstrap's identical split for the SAME reason: adapter's
// own doc (see AWSEndUserMessagingSMSParams) states that package never
// depends on internal/platform/config, so a config-and-metrics-consuming
// builder belongs in a peer package instead.
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

// NewSMSSender builds the domain.SMSSender this deployment uses (NES-138):
// NoopSMSSender when smsCfg.Enabled is false (the default — zero AWS
// dependency, see NoopSMSSender's own doc), or an
// AWSEndUserMessagingSender, instrumented with recorder, when true.
//
// The instrumentation wrap happens HERE, not inside
// AWSEndUserMessagingSender itself, for the same reason config stays out
// of internal/notify/adapter: recording Prometheus metrics is a
// composition-root-adjacent concern, not the AWS client's own job — see
// internal/platform/metrics's own package doc ("the only platform package
// that imports the Prometheus client directly"). Wrapping the returned
// SMSSender (rather than requiring every future caller to remember to
// instrument it) means metrics stay consistent regardless of which caller
// (NES-139's eventual Sender wrapper, or a manual verification path)
// ends up invoking Send. The Noop sender is deliberately NOT wrapped: its
// calls have no real-world outcome to track.
//
// ctx bounds AWS config loading (LoadDefaultConfig may reach out to the
// EC2/ECS instance metadata service to resolve credentials) — the caller
// is expected to derive it with a bounded timeout, mirroring
// media/bootstrap.NewPhotoStoreResolver's identical ctx contract.
func NewSMSSender(ctx context.Context, smsCfg config.SMSConfig, recorder metrics.SMSRecorder, logger *slog.Logger) (notifydomain.SMSSender, error) {
	if !smsCfg.Enabled {
		return notifyadapter.NewNoopSMSSender(logger), nil
	}
	// A nil recorder is only safe on the Noop path above, which never
	// wraps in instrumentedSMSSender at all. Once SMS is enabled, every
	// Send call reaches instrumentedSMSSender.Send, which calls a method
	// on recorder unconditionally — a nil recorder would panic there, at
	// send time, rather than failing fast here at construction.
	if recorder == nil {
		return nil, errors.New("sms recorder must not be nil when sms is enabled")
	}

	sender, err := notifyadapter.NewAWSEndUserMessagingSender(ctx, notifyadapter.AWSEndUserMessagingSMSParams{
		Region:              smsCfg.Region,
		OriginationIdentity: smsCfg.OriginationIdentity,
		AccessKeyID:         smsCfg.AccessKeyID,
		SecretAccessKey:     smsCfg.SecretAccessKey,
		RetryMaxAttempts:    smsCfg.RetryMaxAttempts,
	})
	if err != nil {
		return nil, fmt.Errorf("create aws end user messaging sender: %w", err)
	}
	return newInstrumentedSMSSender(sender, recorder), nil
}
