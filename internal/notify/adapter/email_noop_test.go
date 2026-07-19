package adapter_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
)

func TestNoopEmailSender_SendSucceedsWithNoExternalEffect(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := notifyadapter.NewNoopEmailSender(logger)

	id, err := sender.Send(context.Background(), "recipient@example.com", "Subject", "<p>html</p>", "text")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id == "" {
		t.Error("Send returned an empty provider message id")
	}
}

func TestNewNoopEmailSender_PanicsOnNilLogger(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewNoopEmailSender(nil logger) did not panic")
		}
	}()
	notifyadapter.NewNoopEmailSender(nil)
}
