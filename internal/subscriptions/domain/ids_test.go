package domain_test

import (
	"testing"

	subscriptions "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

func TestSubscriptionIDRoundTrip(t *testing.T) {
	id := subscriptions.NewSubscriptionID()
	parsed, err := subscriptions.ParseSubscriptionID(id.String())
	if err != nil {
		t.Fatalf("ParseSubscriptionID(%q) error = %v", id.String(), err)
	}
	if parsed != id {
		t.Errorf("ParseSubscriptionID round-trip: got %v, want %v", parsed, id)
	}
}

func TestParseSubscriptionIDInvalid(t *testing.T) {
	if _, err := subscriptions.ParseSubscriptionID("not-a-uuid"); err == nil {
		t.Fatal("ParseSubscriptionID(\"not-a-uuid\") error = nil, want non-nil")
	}
}
