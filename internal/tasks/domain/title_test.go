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
