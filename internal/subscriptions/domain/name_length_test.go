package domain_test

import (
	"errors"
	"strings"
	"testing"

	subscriptions "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

func TestSubscriptionValidate_NameLength(t *testing.T) {
	cases := []struct {
		label string
		name  string
		want  error
	}{
		{"at the limit", strings.Repeat("a", subscriptions.MaxNameLength), nil},
		{"multibyte at the limit", strings.Repeat("家", subscriptions.MaxNameLength), nil},
		{"one over", strings.Repeat("a", subscriptions.MaxNameLength+1), subscriptions.ErrSubscriptionNameTooLong},
		{"multibyte one over", strings.Repeat("家", subscriptions.MaxNameLength+1), subscriptions.ErrSubscriptionNameTooLong},
		{"far over", strings.Repeat("a", 10_000), subscriptions.ErrSubscriptionNameTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			sub := validSubscription(t)
			sub.Name = tc.name
			err := sub.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() error = %v, want %v", err, tc.want)
			}
			if tc.want != nil && !errors.Is(err, subscriptions.ErrInvalidSubscription) {
				t.Fatalf("Validate() error = %v, want it to wrap ErrInvalidSubscription", err)
			}
		})
	}
}
