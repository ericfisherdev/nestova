package app_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/app"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// ----------------------------------------------------------------------------
// Fakes
// ----------------------------------------------------------------------------

// fakeOutbox is an in-memory domain.Outbox for hermetic dispatcher tests.
type fakeOutbox struct {
	due       []*domain.Notification
	sentIDs   []domain.NotificationID
	failedIDs []domain.NotificationID
	claimErr  error
}

func (f *fakeOutbox) Enqueue(_ context.Context, n *domain.Notification) error {
	f.due = append(f.due, n)
	return nil
}

func (f *fakeOutbox) ClaimDue(_ context.Context, limit int) ([]*domain.Notification, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if len(f.due) == 0 {
		return nil, nil
	}
	end := limit
	if end > len(f.due) {
		end = len(f.due)
	}
	batch := f.due[:end]
	f.due = f.due[end:]
	return batch, nil
}

func (f *fakeOutbox) MarkSent(_ context.Context, id domain.NotificationID) error {
	f.sentIDs = append(f.sentIDs, id)
	return nil
}

func (f *fakeOutbox) MarkFailed(_ context.Context, id domain.NotificationID) error {
	f.failedIDs = append(f.failedIDs, id)
	return nil
}

// toggleFakeSender is a domain.Sender whose Send behaviour is determined by a
// closure, enabling per-call control in tests.
type toggleFakeSender struct {
	ch     domain.Channel
	sendFn func(n *domain.Notification) error
}

func (s *toggleFakeSender) Channel() domain.Channel { return s.ch }

func (s *toggleFakeSender) Send(_ context.Context, n *domain.Notification) error {
	return s.sendFn(n)
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

func silentLogger() *slog.Logger {
	// Discard all log output; tests should not produce noise.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 10}))
}

func newInAppNotification() *domain.Notification {
	return &domain.Notification{
		ID:           domain.NewNotificationID(),
		HouseholdID:  household.NewHouseholdID(),
		Channel:      domain.ChannelInApp,
		Title:        "Test Title",
		Body:         "Test Body",
		ScheduledFor: time.Now().Add(-time.Second),
		Status:       domain.StatusPending,
	}
}

func newDispatcher(t *testing.T, outbox domain.Outbox, senders []domain.Sender) *app.Dispatcher {
	t.Helper()
	d, err := app.NewDispatcher(outbox, senders, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

func alwaysSucceedSender() *toggleFakeSender {
	return &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return nil },
	}
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestRunOnce_Success_CallsMarkSent(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	if len(outbox.sentIDs) != 1 || outbox.sentIDs[0] != n.ID {
		t.Errorf("outbox.sentIDs = %v, want [%v]", outbox.sentIDs, n.ID)
	}
	if len(outbox.failedIDs) != 0 {
		t.Errorf("outbox.failedIDs = %v, want empty", outbox.failedIDs)
	}
}

func TestRunOnce_SendFailure_CallsMarkFailed(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return errors.New("network down") },
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Errorf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	if len(outbox.sentIDs) != 0 {
		t.Errorf("outbox.sentIDs = %v, want empty", outbox.sentIDs)
	}
}

// newSMSNotification mirrors newInAppNotification but on the SMS channel,
// with a member and a source reference set — the shape a real
// RoutingEnqueuer-routed SMS notification would have.
func newSMSNotification() *domain.Notification {
	memberID := household.NewMemberID()
	sourceID := uuid.New()
	return &domain.Notification{
		ID:           domain.NewNotificationID(),
		HouseholdID:  household.NewHouseholdID(),
		MemberID:     &memberID,
		Channel:      domain.ChannelSMS,
		Title:        "Claim expiring soon",
		Body:         "Complete it soon.",
		ScheduledFor: time.Now().Add(-time.Second),
		Status:       domain.StatusPending,
		SourceType:   "task_instance",
		SourceID:     &sourceID,
	}
}

func TestRunOnce_SMSSendFailure_FallsBackToInApp(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newSMSNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelSMS,
		sendFn: func(_ *domain.Notification) error { return domain.ErrMemberNotSMSReady },
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	// The original SMS notification is marked failed — nothing here
	// pretends the SMS itself was delivered.
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Fatalf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	// NES-139 AC: "nothing silently lost" — a NEW in-app notification was
	// enqueued carrying the same content.
	if len(outbox.due) != 1 {
		t.Fatalf("outbox.due len = %d, want 1 (the fallback notification)", len(outbox.due))
	}
	fallback := outbox.due[0]
	if fallback.ID == n.ID {
		t.Error("fallback notification reuses the original ID; want a fresh one")
	}
	if fallback.Channel != domain.ChannelInApp {
		t.Errorf("fallback.Channel = %v, want ChannelInApp", fallback.Channel)
	}
	if fallback.Title != n.Title || fallback.Body != n.Body {
		t.Errorf("fallback content = (%q, %q), want (%q, %q)", fallback.Title, fallback.Body, n.Title, n.Body)
	}
	if fallback.HouseholdID != n.HouseholdID {
		t.Error("fallback.HouseholdID does not match the original notification")
	}
	if fallback.MemberID == nil || n.MemberID == nil || *fallback.MemberID != *n.MemberID {
		t.Error("fallback.MemberID does not match the original notification")
	}
	if fallback.SourceType != n.SourceType {
		t.Errorf("fallback.SourceType = %q, want %q", fallback.SourceType, n.SourceType)
	}
	if fallback.SourceID == nil || n.SourceID == nil || *fallback.SourceID != *n.SourceID {
		t.Error("fallback.SourceID does not match the original notification")
	}
	if fallback.Status != domain.StatusPending {
		t.Errorf("fallback.Status = %v, want StatusPending", fallback.Status)
	}
}

// newEmailNotification mirrors newSMSNotification but on the email
// channel, with a member and a source reference set — the shape a real
// RoutingEnqueuer-routed email notification would have.
func newEmailNotification() *domain.Notification {
	memberID := household.NewMemberID()
	sourceID := uuid.New()
	return &domain.Notification{
		ID:           domain.NewNotificationID(),
		HouseholdID:  household.NewHouseholdID(),
		MemberID:     &memberID,
		Channel:      domain.ChannelEmail,
		Title:        "Claim expiring soon",
		Body:         "Complete it soon.",
		ScheduledFor: time.Now().Add(-time.Second),
		Status:       domain.StatusPending,
		SourceType:   "task_instance",
		SourceID:     &sourceID,
	}
}

func TestRunOnce_EmailSendFailure_FallsBackToInApp(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newEmailNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelEmail,
		sendFn: func(_ *domain.Notification) error { return domain.ErrRecipientRejected },
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	// The original email notification is marked failed — nothing here
	// pretends the email itself was delivered.
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Fatalf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	// NES-139 AC (generalized to email in NES-141): "nothing silently
	// lost" — a NEW in-app notification was enqueued carrying the same
	// content.
	if len(outbox.due) != 1 {
		t.Fatalf("outbox.due len = %d, want 1 (the fallback notification)", len(outbox.due))
	}
	fallback := outbox.due[0]
	if fallback.ID == n.ID {
		t.Error("fallback notification reuses the original ID; want a fresh one")
	}
	if fallback.Channel != domain.ChannelInApp {
		t.Errorf("fallback.Channel = %v, want ChannelInApp", fallback.Channel)
	}
	if fallback.Title != n.Title || fallback.Body != n.Body {
		t.Errorf("fallback content = (%q, %q), want (%q, %q)", fallback.Title, fallback.Body, n.Title, n.Body)
	}
	if fallback.HouseholdID != n.HouseholdID {
		t.Error("fallback.HouseholdID does not match the original notification")
	}
	if fallback.MemberID == nil || n.MemberID == nil || *fallback.MemberID != *n.MemberID {
		t.Error("fallback.MemberID does not match the original notification")
	}
	if fallback.SourceType != n.SourceType {
		t.Errorf("fallback.SourceType = %q, want %q", fallback.SourceType, n.SourceType)
	}
	if fallback.SourceID == nil || n.SourceID == nil || *fallback.SourceID != *n.SourceID {
		t.Error("fallback.SourceID does not match the original notification")
	}
	if fallback.Status != domain.StatusPending {
		t.Errorf("fallback.Status = %v, want StatusPending", fallback.Status)
	}
}

func TestRunOnce_NonSMSSendFailure_NoFallback(t *testing.T) {
	// A non-SMS/email channel's send failure must NOT trigger the in-app
	// fallback — only SMS and email have real-world preconditions that can go stale
	// between enqueue and delivery (see deliver's own doc).
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return errors.New("boom") },
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if len(outbox.failedIDs) != 1 {
		t.Fatalf("outbox.failedIDs len = %d, want 1", len(outbox.failedIDs))
	}
	if len(outbox.due) != 0 {
		t.Errorf("outbox.due len = %d, want 0 (no fallback for a non-SMS channel)", len(outbox.due))
	}
}

// spySMSRecorder records IncFallback call counts, for asserting
// Dispatcher's own metrics behavior (CodeRabbit PR #109 round 3, trivial
// finding #3) without depending on a real Prometheus registry — the other
// three SMSRecorder methods are inert since the dispatcher itself only
// ever calls IncFallback (IncSent/IncFailed/IncOptedOut are recorded
// deeper, inside the instrumented SMS sender NES-138 built — see
// notify/bootstrap.instrumentedSMSSender).
type spySMSRecorder struct {
	sent, failed, optedOut, fallback int
}

func (s *spySMSRecorder) IncSent()     { s.sent++ }
func (s *spySMSRecorder) IncFailed()   { s.failed++ }
func (s *spySMSRecorder) IncOptedOut() { s.optedOut++ }
func (s *spySMSRecorder) IncFallback() { s.fallback++ }

func TestRunOnce_SMSSendFailure_RecordsFallbackMetric(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newSMSNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelSMS,
		sendFn: func(_ *domain.Notification) error { return domain.ErrMemberNotSMSReady },
	}
	spy := &spySMSRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{sender}, silentLogger(), metrics.NopTickRecorder{}, spy, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if spy.fallback != 1 {
		t.Errorf("IncFallback calls = %d, want 1", spy.fallback)
	}
	if spy.sent != 0 || spy.failed != 0 || spy.optedOut != 0 {
		t.Errorf("unexpected recorder calls: sent=%d failed=%d optedOut=%d, want all 0 (the dispatcher itself only records IncFallback)", spy.sent, spy.failed, spy.optedOut)
	}
}

func TestRunOnce_NonSMSSendFailure_DoesNotRecordFallbackMetric(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return errors.New("boom") },
	}
	spy := &spySMSRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{sender}, silentLogger(), metrics.NopTickRecorder{}, spy, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if spy.fallback != 0 {
		t.Errorf("IncFallback calls = %d, want 0 for a non-SMS channel failure", spy.fallback)
	}
}

// spyEmailRecorder mirrors spySMSRecorder's identical role (CodeRabbit PR
// #109 round 3, trivial finding #3), for the email channel added in
// NES-141.
type spyEmailRecorder struct {
	sent, failed, rejected, fallback int
}

func (s *spyEmailRecorder) IncSent()     { s.sent++ }
func (s *spyEmailRecorder) IncFailed()   { s.failed++ }
func (s *spyEmailRecorder) IncRejected() { s.rejected++ }
func (s *spyEmailRecorder) IncFallback() { s.fallback++ }

func TestRunOnce_EmailSendFailure_RecordsFallbackMetric(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newEmailNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelEmail,
		sendFn: func(_ *domain.Notification) error { return domain.ErrRecipientRejected },
	}
	spy := &spyEmailRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{sender}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, spy, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if spy.fallback != 1 {
		t.Errorf("IncFallback calls = %d, want 1", spy.fallback)
	}
	if spy.sent != 0 || spy.failed != 0 || spy.rejected != 0 {
		t.Errorf("unexpected recorder calls: sent=%d failed=%d rejected=%d, want all 0 (the dispatcher itself only records IncFallback)", spy.sent, spy.failed, spy.rejected)
	}
}

func TestRunOnce_NonEmailSendFailure_DoesNotRecordEmailFallbackMetric(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	outbox.due = []*domain.Notification{n}

	sender := &toggleFakeSender{
		ch:     domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error { return errors.New("boom") },
	}
	spy := &spyEmailRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{sender}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, spy, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if spy.fallback != 0 {
		t.Errorf("IncFallback calls = %d, want 0 for a non-email channel failure", spy.fallback)
	}
}

// TestRunOnce_UnknownChannel_CallsMarkFailed uses push, not sms/email, as
// its "genuinely unregistered, no fallback path" example: since NES-141
// round 2 (CodeRabbit major finding #3), an unregistered SMS or EMAIL
// channel specifically DOES fall back to in-app (see
// TestRunOnce_UnregisteredSMSChannel_FallsBackToInApp and
// TestRunOnce_UnregisteredEmailChannel_FallsBackToInApp below) — push has
// no wired Sender ANYWHERE in this deployment and no fallback path, so it
// stays the right example for "no sender, no fallback, just marked
// failed."
func TestRunOnce_UnknownChannel_CallsMarkFailed(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newInAppNotification()
	n.Channel = domain.ChannelPush // no push sender registered anywhere
	outbox.due = []*domain.Notification{n}

	// Only inapp sender registered — push channel is unknown.
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 1 {
		t.Errorf("RunOnce count = %d, want 1", count)
	}
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Errorf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	if len(outbox.sentIDs) != 0 {
		t.Errorf("outbox.sentIDs = %v, want empty", outbox.sentIDs)
	}
	if len(outbox.due) != 0 {
		t.Errorf("outbox.due len = %d, want 0 (push has no fallback path)", len(outbox.due))
	}
}

// TestRunOnce_UnregisteredSMSChannel_FallsBackToInApp is the CodeRabbit
// round-2 regression test (major finding #3): when NOTIFY_SMS_ENABLED is
// false, cmd/server/main.go does not register an SMS sender with the
// dispatcher at all (see that file's own comment) — an sms-channel
// notification must still fall back to in-app, exactly as a real send
// failure would, not silently succeed or silently vanish.
func TestRunOnce_UnregisteredSMSChannel_FallsBackToInApp(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newSMSNotification()
	outbox.due = []*domain.Notification{n}

	// No SMS sender registered at all — mirrors main.go's behavior when
	// NOTIFY_SMS_ENABLED=false.
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Fatalf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	if len(outbox.due) != 1 {
		t.Fatalf("outbox.due len = %d, want 1 (the fallback notification)", len(outbox.due))
	}
	if outbox.due[0].Channel != domain.ChannelInApp {
		t.Errorf("fallback.Channel = %v, want ChannelInApp", outbox.due[0].Channel)
	}
}

// TestRunOnce_UnregisteredEmailChannel_FallsBackToInApp mirrors
// TestRunOnce_UnregisteredSMSChannel_FallsBackToInApp for the email
// channel and NOTIFY_EMAIL_ENABLED=false.
func TestRunOnce_UnregisteredEmailChannel_FallsBackToInApp(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newEmailNotification()
	outbox.due = []*domain.Notification{n}

	// No email sender registered at all — mirrors main.go's behavior when
	// NOTIFY_EMAIL_ENABLED=false.
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if len(outbox.failedIDs) != 1 || outbox.failedIDs[0] != n.ID {
		t.Fatalf("outbox.failedIDs = %v, want [%v]", outbox.failedIDs, n.ID)
	}
	if len(outbox.due) != 1 {
		t.Fatalf("outbox.due len = %d, want 1 (the fallback notification)", len(outbox.due))
	}
	if outbox.due[0].Channel != domain.ChannelInApp {
		t.Errorf("fallback.Channel = %v, want ChannelInApp", outbox.due[0].Channel)
	}
}

// TestRunOnce_UnregisteredSMSChannel_RecordsFallbackMetric confirms the
// unregistered-channel fallback path (finding #3) records the SAME
// IncFallback metric a real send-failure fallback would, via the SAME
// fallbackForChannel helper.
func TestRunOnce_UnregisteredSMSChannel_RecordsFallbackMetric(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newSMSNotification()
	outbox.due = []*domain.Notification{n}

	spy := &spySMSRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, spy, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if spy.fallback != 1 {
		t.Errorf("IncFallback calls = %d, want 1", spy.fallback)
	}
}

// TestRunOnce_UnregisteredEmailChannel_RecordsFallbackMetric mirrors
// TestRunOnce_UnregisteredSMSChannel_RecordsFallbackMetric for the email
// channel: an unregistered email channel must record the SAME IncFallback
// metric a real send-failure fallback would, via the SAME
// fallbackForChannel helper.
func TestRunOnce_UnregisteredEmailChannel_RecordsFallbackMetric(t *testing.T) {
	outbox := &fakeOutbox{}
	n := newEmailNotification()
	outbox.due = []*domain.Notification{n}

	spy := &spyEmailRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, spy, 10, time.Minute)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	if _, err := d.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if spy.fallback != 1 {
		t.Errorf("IncFallback calls = %d, want 1", spy.fallback)
	}
}

func TestRunOnce_OneFailureDoesNotAbortBatch(t *testing.T) {
	// Two notifications: first send fails, second succeeds. Both must be
	// processed: count must be 2, one in failedIDs, one in sentIDs.
	outbox := &fakeOutbox{}
	n1 := newInAppNotification()
	n2 := newInAppNotification()
	outbox.due = []*domain.Notification{n1, n2}

	calls := 0
	sender := &toggleFakeSender{
		ch: domain.ChannelInApp,
		sendFn: func(_ *domain.Notification) error {
			calls++
			if calls == 1 {
				return errors.New("transient error on first")
			}
			return nil
		},
	}
	d := newDispatcher(t, outbox, []domain.Sender{sender})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 2 {
		t.Errorf("RunOnce count = %d, want 2", count)
	}
	if len(outbox.failedIDs) != 1 {
		t.Errorf("failedIDs len = %d, want 1", len(outbox.failedIDs))
	}
	if len(outbox.sentIDs) != 1 {
		t.Errorf("sentIDs len = %d, want 1", len(outbox.sentIDs))
	}
}

func TestRunOnce_NoDueNotifications_ReturnsZero(t *testing.T) {
	outbox := &fakeOutbox{} // empty due slice
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	count, err := d.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce error = %v", err)
	}
	if count != 0 {
		t.Errorf("RunOnce count = %d, want 0", count)
	}
}

func TestRunOnce_ClaimDueError_ReturnsError(t *testing.T) {
	outbox := &fakeOutbox{claimErr: errors.New("db error")}
	d := newDispatcher(t, outbox, []domain.Sender{alwaysSucceedSender()})

	_, err := d.RunOnce(context.Background())
	if err == nil {
		t.Error("RunOnce error = nil, want non-nil when ClaimDue fails")
	}
}

func TestNewDispatcher_NilOutbox_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(nil, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil outbox) error = nil, want non-nil")
	}
}

func TestNewDispatcher_EmptySenders_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(empty senders) error = nil, want non-nil")
	}
}

func TestNewDispatcher_NilLogger_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, nil, metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil logger) error = nil, want non-nil")
	}
}

func TestNewDispatcher_InvalidBatchSize_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 0, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(batchSize=0) error = nil, want non-nil")
	}
}

func TestNewDispatcher_InvalidPollInterval_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, 0)
	if err == nil {
		t.Error("NewDispatcher(pollInterval=0) error = nil, want non-nil")
	}
}

func TestNewDispatcher_DuplicateChannel_ReturnsError(t *testing.T) {
	s := alwaysSucceedSender()
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{s, s}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(duplicate channel) error = nil, want non-nil")
	}
}

func TestNewDispatcher_NilSenderEntry_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{nil}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil sender) error = nil, want non-nil")
	}
}

func TestRun_ReturnsWhenContextCancelled(t *testing.T) {
	d, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestNewDispatcher_NilTickRecorder_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), nil, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil tick recorder) error = nil, want non-nil")
	}
}

func TestNewDispatcher_NilSMSRecorder_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, nil, metrics.NopEmailRecorder{}, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil sms recorder) error = nil, want non-nil")
	}
}

func TestNewDispatcher_NilEmailRecorder_ReturnsError(t *testing.T) {
	_, err := app.NewDispatcher(&fakeOutbox{}, []domain.Sender{alwaysSucceedSender()}, silentLogger(), metrics.NopTickRecorder{}, metrics.NopSMSRecorder{}, nil, 10, time.Minute)
	if err == nil {
		t.Error("NewDispatcher(nil email recorder) error = nil, want non-nil")
	}
}

// spyTickRecorder records ObserveTick calls for assertion. It is used instead
// of asserting on a real PromTickRecorder because the ObserveTick seam lives in
// the unexported runTick, so the observable contract here is "the tick recorder
// saw the failing cycle"; PromTickRecorder's own error/success behaviour is
// covered once in the metrics package.
type spyTickRecorder struct {
	mu    sync.Mutex
	calls []spyTickCall
}

type spyTickCall struct {
	scheduler metrics.SchedulerName
	err       error
}

func (s *spyTickRecorder) ObserveTick(scheduler metrics.SchedulerName, _ time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, spyTickCall{scheduler: scheduler, err: err})
}

// snapshot returns a copy of the recorded calls.
func (s *spyTickRecorder) snapshot() []spyTickCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spyTickCall(nil), s.calls...)
}

// waitForCall polls until at least one call is recorded or the deadline hits.
func (s *spyTickRecorder) waitForCall(t *testing.T) spyTickCall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls := s.snapshot(); len(calls) > 0 {
			return calls[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ObserveTick was never called")
	return spyTickCall{}
}

func TestRun_FailingTick_ObservedWithError(t *testing.T) {
	outbox := &fakeOutbox{claimErr: errors.New("db error")}
	spy := &spyTickRecorder{}
	d, err := app.NewDispatcher(outbox, []domain.Sender{alwaysSucceedSender()}, silentLogger(), spy, metrics.NopSMSRecorder{}, metrics.NopEmailRecorder{}, 10, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	call := spy.waitForCall(t)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	if call.scheduler != metrics.SchedulerDispatcher {
		t.Errorf("ObserveTick scheduler = %q, want %q", call.scheduler, metrics.SchedulerDispatcher)
	}
	if call.err == nil {
		t.Error("ObserveTick err = nil, want the failing tick's error")
	}
}
