package domain_test

import (
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

func TestDateOf(t *testing.T) {
	t.Parallel()
	// A non-midnight time in a non-UTC zone normalizes to that wall-calendar
	// day at midnight UTC, so a DATE round-trip cannot shift the day.
	loc := time.FixedZone("plus2", 2*60*60)
	in := time.Date(2026, 1, 15, 23, 30, 0, 0, loc)
	got := domain.DateOf(in)
	want := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("DateOf(%s) = %s, want %s", in.Format(time.RFC3339), got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("DateOf location = %v, want UTC", got.Location())
	}
	// Idempotent: normalizing an already-normalized value is a no-op.
	if again := domain.DateOf(got); !again.Equal(got) {
		t.Errorf("DateOf not idempotent: %s -> %s", got, again)
	}
}
