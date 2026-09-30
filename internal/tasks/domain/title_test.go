package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

func TestValidateTitle(t *testing.T) {
	t.Parallel()

	// A 200-rune multi-byte title is the case a byte-based length check would
	// wrongly reject: it is 200 characters but 600 bytes.
	multibyte := strings.Repeat("家", domain.MaxTitleLength)

	tests := []struct {
		name  string
		title string
		want  error
	}{
		{name: "plain title", title: "Take out the bins", want: nil},
		{name: "at the limit", title: strings.Repeat("a", domain.MaxTitleLength), want: nil},
		{name: "multibyte at the limit", title: multibyte, want: nil},
		{name: "trimmed to the limit", title: "  " + strings.Repeat("a", domain.MaxTitleLength) + "  ", want: nil},
		{name: "empty", title: "", want: domain.ErrTitleRequired},
		{name: "whitespace only", title: "   \t\n ", want: domain.ErrTitleRequired},
		{name: "one over the limit", title: strings.Repeat("a", domain.MaxTitleLength+1), want: domain.ErrTitleTooLong},
		{name: "multibyte one over the limit", title: multibyte + "家", want: domain.ErrTitleTooLong},
		{name: "far over the limit", title: strings.Repeat("a", 10_000), want: domain.ErrTitleTooLong},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := domain.ValidateTitle(tc.title); !errors.Is(err, tc.want) {
				t.Errorf("ValidateTitle(%d chars) = %v, want %v", len([]rune(tc.title)), err, tc.want)
			}
		})
	}
}

func TestValidateTaskCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		points   int
		leadDays int
		want     error
	}{
		{name: "zeroes", want: nil},
		{name: "largest in-range values", points: domain.MaxInt4, leadDays: domain.MaxLeadTimeDays, want: nil},
		{name: "negative points", points: -1, want: domain.ErrInvalidTaskPoints},
		{name: "points one over", points: domain.MaxInt4 + 1, want: domain.ErrInvalidTaskPoints},
		{name: "negative lead time", leadDays: -1, want: domain.ErrInvalidLeadTime},
		{name: "lead time one over", leadDays: domain.MaxLeadTimeDays + 1, want: domain.ErrInvalidLeadTime},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := domain.ValidateTaskCounts(tc.points, tc.leadDays); !errors.Is(err, tc.want) {
				t.Errorf("ValidateTaskCounts(%d, %d) = %v, want %v", tc.points, tc.leadDays, err, tc.want)
			}
		})
	}
}
