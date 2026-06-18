package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// senderRegistry maps Channel to Sender. It is built once at construction time
// from the provided sender slice and is read-only thereafter.
type senderRegistry map[domain.Channel]domain.Sender

// newSenderRegistry builds a registry from the supplied senders. Returns an
// error if the slice is empty.
func newSenderRegistry(senders []domain.Sender) (senderRegistry, error) {
	if len(senders) == 0 {
		return nil, errors.New("app: dispatcher requires at least one sender")
	}
	reg := make(senderRegistry, len(senders))
	for _, s := range senders {
		if s == nil {
			return nil, errors.New("app: dispatcher received a nil sender")
		}
		if _, exists := reg[s.Channel()]; exists {
			return nil, fmt.Errorf("app: duplicate sender for channel %s", s.Channel())
		}
		reg[s.Channel()] = s
	}
	return reg, nil
}

// resolve returns the Sender for channel, or domain.ErrUnknownChannel when no
// sender is registered for that channel.
func (r senderRegistry) resolve(channel domain.Channel) (domain.Sender, error) {
	s, ok := r[channel]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrUnknownChannel, channel)
	}
	return s, nil
}

// Dispatcher polls the notification outbox and delivers due notifications
// through the appropriate channel Sender. It is safe to run multiple instances
// concurrently: the Outbox.ClaimDue uses FOR UPDATE SKIP LOCKED so each
// dispatcher claims a disjoint batch.
type Dispatcher struct {
	outbox       domain.Outbox
	senders      senderRegistry
	logger       *slog.Logger
	batchSize    int
	pollInterval time.Duration
}

// NewDispatcher constructs a Dispatcher with injected dependencies.
// batchSize caps the number of notifications claimed per RunOnce call.
// pollInterval controls how often Run polls the outbox.
func NewDispatcher(
	outbox domain.Outbox,
	senders []domain.Sender,
	logger *slog.Logger,
	batchSize int,
	pollInterval time.Duration,
) (*Dispatcher, error) {
	if outbox == nil {
		return nil, errors.New("app: NewDispatcher requires a non-nil outbox")
	}
	if logger == nil {
		return nil, errors.New("app: NewDispatcher requires a non-nil logger")
	}
	if batchSize <= 0 {
		return nil, fmt.Errorf("app: NewDispatcher batchSize must be positive, got %d", batchSize)
	}
	if pollInterval <= 0 {
		return nil, fmt.Errorf("app: NewDispatcher pollInterval must be positive, got %v", pollInterval)
	}
	reg, err := newSenderRegistry(senders)
	if err != nil {
		return nil, err
	}
	return &Dispatcher{
		outbox:       outbox,
		senders:      reg,
		logger:       logger,
		batchSize:    batchSize,
		pollInterval: pollInterval,
	}, nil
}

// RunOnce claims up to batchSize due notifications, attempts delivery for each,
// and returns the count of notifications processed (claimed), regardless of
// whether individual deliveries succeeded. A failed delivery on one notification
// does not abort the rest of the batch.
//
// On send success the notification was already marked StatusSent by ClaimDue
// (optimistic claim); MarkSent is called again as an idempotent no-op to record
// sent_at accurately when the sender returned after ClaimDue's update. On send
// failure, MarkFailed downgrades the status so operators can identify undelivered
// notifications.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	notifications, err := d.outbox.ClaimDue(ctx, d.batchSize)
	if err != nil {
		return 0, fmt.Errorf("dispatcher: claim due: %w", err)
	}
	if len(notifications) == 0 {
		return 0, nil
	}

	for _, n := range notifications {
		d.deliver(ctx, n)
	}
	return len(notifications), nil
}

// markWriteTimeout bounds the status-write calls (MarkSent/MarkFailed). They run
// under their own context so a notification's outcome is still recorded even if
// the batch's work context has already expired or been cancelled.
const markWriteTimeout = 5 * time.Second

// deliver attempts to send a single notification and updates its status.
// Errors are logged with the notification id (not PII); they do not propagate.
func (d *Dispatcher) deliver(ctx context.Context, n *domain.Notification) {
	sender, err := d.senders.resolve(n.Channel)
	if err != nil {
		d.logger.Error("dispatcher: no sender for channel",
			"notification_id", n.ID.String(),
			"channel", n.Channel.String(),
			"error", err,
		)
		d.markFailed(n.ID, "mark failed after unknown channel")
		return
	}

	if err := sender.Send(ctx, n); err != nil {
		d.logger.Error("dispatcher: send failed",
			"notification_id", n.ID.String(),
			"channel", n.Channel.String(),
			"error", err,
		)
		d.markFailed(n.ID, "mark failed after send error")
		return
	}

	// Record success under an independent context so a timed-out work context
	// cannot leave a delivered row stuck in the claimed (sent_at IS NULL) state.
	markCtx, cancel := context.WithTimeout(context.Background(), markWriteTimeout)
	defer cancel()
	if err := d.outbox.MarkSent(markCtx, n.ID); err != nil {
		d.logger.Error("dispatcher: mark sent",
			"notification_id", n.ID.String(),
			"error", err,
		)
	}
}

// markFailed records a delivery failure under an independent, bounded context so
// the status write survives expiry/cancellation of the batch's work context.
func (d *Dispatcher) markFailed(id domain.NotificationID, logMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), markWriteTimeout)
	defer cancel()
	if err := d.outbox.MarkFailed(ctx, id); err != nil {
		d.logger.Error("dispatcher: "+logMsg,
			"notification_id", id.String(),
			"error", err,
		)
	}
}

// Run polls the outbox every pollInterval until ctx is cancelled. It logs start
// and stop events. Errors from RunOnce are logged but do not stop the loop —
// transient database failures resolve on the next tick.
//
// Cancelling ctx stops the loop but does NOT abort a batch already in progress:
// each tick runs under its own context (see runTick), so an in-flight batch
// finishes its database writes cleanly before Run returns. Callers can therefore
// wait for Run to return to know the dispatcher has fully drained.
func (d *Dispatcher) Run(ctx context.Context) {
	d.logger.Info("dispatcher: starting", "poll_interval", d.pollInterval, "batch_size", d.batchSize)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("dispatcher: stopped")
			return
		case <-ticker.C:
			d.runTick()
		}
	}
}

// runTick executes a single RunOnce under a fresh bounded context that is
// independent of Run's lifecycle context. Decoupling the work context from the
// shutdown signal lets an in-flight batch complete its claim/send/mark database
// writes even while the process is shutting down, while the timeout still caps
// how long a stalled batch can delay shutdown.
func (d *Dispatcher) runTick() {
	runCtx, cancel := context.WithTimeout(context.Background(), d.pollInterval)
	defer cancel()

	count, err := d.RunOnce(runCtx)
	if err != nil {
		d.logger.Error("dispatcher: run once failed", "error", err)
		return
	}
	if count > 0 {
		d.logger.Info("dispatcher: processed batch", "count", count)
	}
}
