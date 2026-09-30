package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

func TestRecipeValidate_TitleLength(t *testing.T) {
	cases := []struct {
		label string
		title string
		want  error
	}{
		{"at the limit", strings.Repeat("a", domain.MaxRecipeTitleLength), nil},
		{"multibyte at the limit", strings.Repeat("家", domain.MaxRecipeTitleLength), nil},
		{"one over", strings.Repeat("a", domain.MaxRecipeTitleLength+1), domain.ErrRecipeTitleTooLong},
		{"multibyte one over", strings.Repeat("家", domain.MaxRecipeTitleLength+1), domain.ErrRecipeTitleTooLong},
		{"far over", strings.Repeat("a", 10_000), domain.ErrRecipeTitleTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			r := validLocalRecipe(t)
			r.Title = tc.title
			err := r.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
			if tc.want != nil && !errors.Is(err, domain.ErrInvalidRecipe) {
				t.Fatalf("Validate() = %v, want it to wrap ErrInvalidRecipe", err)
			}
		})
	}
}
