package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

func TestValidateRewardText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		label       string
		name        string
		description string
		want        error
	}{
		{"both at the limit", strings.Repeat("a", domain.MaxRewardNameLength), strings.Repeat("d", domain.MaxRewardDescriptionLength), nil},
		{"multibyte at the limit", strings.Repeat("家", domain.MaxRewardNameLength), strings.Repeat("家", domain.MaxRewardDescriptionLength), nil},
		{"empty description", "Toy", "", nil},
		{"name one over", strings.Repeat("a", domain.MaxRewardNameLength+1), "", domain.ErrRewardNameTooLong},
		{"description one over", "Toy", strings.Repeat("d", domain.MaxRewardDescriptionLength+1), domain.ErrRewardDescriptionTooLong},
		{"far over", strings.Repeat("a", 10_000), strings.Repeat("d", 10_000), domain.ErrRewardNameTooLong},
	}
	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			if err := domain.ValidateRewardText(tc.name, tc.description); !errors.Is(err, tc.want) {
				t.Errorf("ValidateRewardText() = %v, want %v", err, tc.want)
			}
		})
	}
}
