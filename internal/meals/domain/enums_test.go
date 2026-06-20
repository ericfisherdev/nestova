package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

func TestRecipeSourceKindRoundTrip(t *testing.T) {
	for _, k := range domain.RecipeSourceKinds() {
		if !k.Valid() {
			t.Errorf("RecipeSourceKinds() returned invalid kind %q", k)
		}
		got, err := domain.ParseRecipeSourceKind(k.String())
		if err != nil || got != k {
			t.Errorf("ParseRecipeSourceKind(%q) = (%q, %v), want (%q, nil)", k, got, err, k)
		}
	}
}

func TestParseRecipeSourceKindRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"", "imported"} {
		if _, err := domain.ParseRecipeSourceKind(bad); err == nil {
			t.Errorf("ParseRecipeSourceKind(%q) = nil error, want error", bad)
		}
		if domain.RecipeSourceKind(bad).Valid() {
			t.Errorf("RecipeSourceKind(%q).Valid() = true, want false", bad)
		}
	}
}

func TestMealRoundTrip(t *testing.T) {
	for _, m := range domain.Meals() {
		if !m.Valid() {
			t.Errorf("Meals() returned invalid meal %q", m)
		}
		got, err := domain.ParseMeal(m.String())
		if err != nil || got != m {
			t.Errorf("ParseMeal(%q) = (%q, %v), want (%q, nil)", m, got, err, m)
		}
	}
}

func TestParseMealRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"", "brunch"} {
		if _, err := domain.ParseMeal(bad); err == nil {
			t.Errorf("ParseMeal(%q) = nil error, want error", bad)
		}
		if domain.Meal(bad).Valid() {
			t.Errorf("Meal(%q).Valid() = true, want false", bad)
		}
	}
}
