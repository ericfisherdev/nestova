package bootstrap

import (
	"context"
	"errors"
	"testing"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// fakeEmailSender is a domain.EmailSender test double whose Send behavior
// is scripted per-test via id/err.
type fakeEmailSender struct {
	id  string
	err error
}

var _ domain.EmailSender = (*fakeEmailSender)(nil)

func (f *fakeEmailSender) Send(context.Context, string, string, string, string) (string, error) {
	return f.id, f.err
}

// fakeEmailRecorder is a metrics.EmailRecorder test double that counts
// each method's invocations, so a decorator test can assert exactly one
// outcome was recorded per Send call.
type fakeEmailRecorder struct {
	sent, failed, rejected, fallback int
}

func (f *fakeEmailRecorder) IncSent()     { f.sent++ }
func (f *fakeEmailRecorder) IncFailed()   { f.failed++ }
func (f *fakeEmailRecorder) IncRejected() { f.rejected++ }
func (f *fakeEmailRecorder) IncFallback() { f.fallback++ }

// TestInstrumentedEmailSender_Send_Success verifies a successful inner
// Send records exactly one IncSent and passes the provider message id
// through unchanged.
func TestInstrumentedEmailSender_Send_Success(t *testing.T) {
	rec := &fakeEmailRecorder{}
	sender := newInstrumentedEmailSender(&fakeEmailSender{id: "provider-id-1"}, rec)

	id, err := sender.Send(context.Background(), "to@example.com", "Subject", "<p>html</p>", "text")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "provider-id-1" {
		t.Errorf("Send id = %q, want %q", id, "provider-id-1")
	}
	if rec.sent != 1 || rec.failed != 0 || rec.rejected != 0 {
		t.Errorf("recorder counts = {sent:%d failed:%d rejected:%d}, want {1 0 0}", rec.sent, rec.failed, rec.rejected)
	}
}

// TestInstrumentedEmailSender_Send_Rejected verifies ErrRecipientRejected
// records IncRejected, not IncFailed.
func TestInstrumentedEmailSender_Send_Rejected(t *testing.T) {
	rec := &fakeEmailRecorder{}
	sender := newInstrumentedEmailSender(&fakeEmailSender{err: domain.ErrRecipientRejected}, rec)

	_, err := sender.Send(context.Background(), "to@example.com", "Subject", "<p>html</p>", "text")
	if !errors.Is(err, domain.ErrRecipientRejected) {
		t.Fatalf("Send error = %v, want ErrRecipientRejected", err)
	}
	if rec.rejected != 1 || rec.sent != 0 || rec.failed != 0 {
		t.Errorf("recorder counts = {sent:%d failed:%d rejected:%d}, want {0 0 1}", rec.sent, rec.failed, rec.rejected)
	}
}

// TestInstrumentedEmailSender_Send_GenericFailure verifies any other error
// records IncFailed.
func TestInstrumentedEmailSender_Send_GenericFailure(t *testing.T) {
	rec := &fakeEmailRecorder{}
	sender := newInstrumentedEmailSender(&fakeEmailSender{err: errors.New("boom")}, rec)

	_, err := sender.Send(context.Background(), "to@example.com", "Subject", "<p>html</p>", "text")
	if err == nil {
		t.Fatal("Send error = nil, want non-nil")
	}
	if rec.failed != 1 || rec.sent != 0 || rec.rejected != 0 {
		t.Errorf("recorder counts = {sent:%d failed:%d rejected:%d}, want {0 1 0}", rec.sent, rec.failed, rec.rejected)
	}
}
