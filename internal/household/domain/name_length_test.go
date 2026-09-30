package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/household/domain"
)

func TestValidateHouseholdAndDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label      string
		validate   func(string) error
		maxLength  int
		errTooLong error
	}{
		{"household name", domain.ValidateHouseholdName, domain.MaxHouseholdNameLength, domain.ErrHouseholdNameTooLong},
		{"display name", domain.ValidateDisplayName, domain.MaxDisplayNameLength, domain.ErrDisplayNameTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name  string
				value string
				want  error
			}{
				{"at the limit", strings.Repeat("a", tc.maxLength), nil},
				{"multibyte at the limit", strings.Repeat("家", tc.maxLength), nil},
				{"one over", strings.Repeat("a", tc.maxLength+1), tc.errTooLong},
				{"multibyte one over", strings.Repeat("家", tc.maxLength+1), tc.errTooLong},
				{"far over", strings.Repeat("a", 10_000), tc.errTooLong},
			}
			for _, c := range cases {
				if err := tc.validate(c.value); !errors.Is(err, c.want) {
					t.Errorf("%s: validate() = %v, want %v", c.name, err, c.want)
				}
			}
		})
	}
}
