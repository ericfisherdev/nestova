package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// testLogger returns a slog.Logger that discards output, for tests that
// need one only to satisfy a constructor's non-nil requirement.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSMSSender is a domain.SMSSender test double whose Send behavior is
// scripted per-test via id/err.
type fakeSMSSender struct {
	id  string
	err error
}

var _ domain.SMSSender = (*fakeSMSSender)(nil)

func (f *fakeSMSSender) Send(context.Context, domain.E164Phone, string) (string, error) {
	return f.id, f.err
}

// fakeSMSRecorder is a metrics.SMSRecorder test double that counts each
// method's invocations, so a decorator test can assert exactly one outcome
// was recorded per Send call.
type fakeSMSRecorder struct {
	sent, failed, optedOut int
}

func (f *fakeSMSRecorder) IncSent()     { f.sent++ }
func (f *fakeSMSRecorder) IncFailed()   { f.failed++ }
func (f *fakeSMSRecorder) IncOptedOut() { f.optedOut++ }

// testE164Phone builds a valid E164Phone for use as an arbitrary Send
// destination; the exact value is never asserted on by these tests.
func testE164Phone(t *testing.T) domain.E164Phone {
	t.Helper()
	phone, err := domain.ParseE164Phone("+15551234567")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}
	return phone
}

// TestInstrumentedSMSSender_Send_Success verifies a successful inner Send
// records exactly one IncSent and passes the provider message id through
// unchanged.
func TestInstrumentedSMSSender_Send_Success(t *testing.T) {
	rec := &fakeSMSRecorder{}
	sender := newInstrumentedSMSSender(&fakeSMSSender{id: "provider-id-1"}, rec)

	id, err := sender.Send(context.Background(), testE164Phone(t), "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "provider-id-1" {
		t.Errorf("Send id = %q, want %q", id, "provider-id-1")
	}
	if rec.sent != 1 || rec.failed != 0 || rec.optedOut != 0 {
		t.Errorf("recorder counts = {sent:%d failed:%d optedOut:%d}, want {1 0 0}", rec.sent, rec.failed, rec.optedOut)
	}
}

// TestInstrumentedSMSSender_Send_OptedOut verifies ErrRecipientOptedOut
// records IncOptedOut, not IncFailed.
func TestInstrumentedSMSSender_Send_OptedOut(t *testing.T) {
	rec := &fakeSMSRecorder{}
	sender := newInstrumentedSMSSender(&fakeSMSSender{err: domain.ErrRecipientOptedOut}, rec)

	_, err := sender.Send(context.Background(), testE164Phone(t), "hello")
	if !errors.Is(err, domain.ErrRecipientOptedOut) {
		t.Fatalf("Send error = %v, want ErrRecipientOptedOut", err)
	}
	if rec.optedOut != 1 || rec.sent != 0 || rec.failed != 0 {
		t.Errorf("recorder counts = {sent:%d failed:%d optedOut:%d}, want {0 0 1}", rec.sent, rec.failed, rec.optedOut)
	}
}

// TestInstrumentedSMSSender_Send_GenericFailure verifies any other error
// records IncFailed.
func TestInstrumentedSMSSender_Send_GenericFailure(t *testing.T) {
	rec := &fakeSMSRecorder{}
	sender := newInstrumentedSMSSender(&fakeSMSSender{err: errors.New("boom")}, rec)

	_, err := sender.Send(context.Background(), testE164Phone(t), "hello")
	if err == nil {
		t.Fatal("Send error = nil, want non-nil")
	}
	if rec.failed != 1 || rec.sent != 0 || rec.optedOut != 0 {
		t.Errorf("recorder counts = {sent:%d failed:%d optedOut:%d}, want {0 1 0}", rec.sent, rec.failed, rec.optedOut)
	}
}
