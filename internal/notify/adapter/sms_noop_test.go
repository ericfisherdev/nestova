package adapter_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

func TestNoopSMSSender_SendSucceedsWithNoExternalEffect(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := notifyadapter.NewNoopSMSSender(logger)

	to, err := domain.ParseE164Phone("+15551234567")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}

	id, err := sender.Send(context.Background(), to, "Test message")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Error("Send returned an empty provider message id")
	}
}

func TestNewNoopSMSSender_PanicsOnNilLogger(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewNoopSMSSender(nil logger) did not panic")
		}
	}()
	notifyadapter.NewNoopSMSSender(nil)
}
