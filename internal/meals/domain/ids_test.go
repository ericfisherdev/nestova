package domain_test

import (
	"testing"

	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

func TestRecipeIDRoundTrip(t *testing.T) {
	id := domain.NewRecipeID()
	got, err := domain.ParseRecipeID(id.String())
	if err != nil {
		t.Fatalf("ParseRecipeID(%q): %v", id, err)
	}
	if got != id {
		t.Errorf("ParseRecipeID round trip = %q, want %q", got, id)
	}
}

func TestParseRecipeIDRejectsInvalid(t *testing.T) {
	if _, err := domain.ParseRecipeID("not-a-uuid"); err == nil {
		t.Error("ParseRecipeID(not-a-uuid) = nil error, want error")
	}
}

func TestMealPlanEntryIDRoundTrip(t *testing.T) {
	id := domain.NewMealPlanEntryID()
	got, err := domain.ParseMealPlanEntryID(id.String())
	if err != nil {
		t.Fatalf("ParseMealPlanEntryID(%q): %v", id, err)
	}
	if got != id {
		t.Errorf("ParseMealPlanEntryID round trip = %q, want %q", got, id)
	}
}

func TestParseMealPlanEntryIDRejectsInvalid(t *testing.T) {
	if _, err := domain.ParseMealPlanEntryID(""); err == nil {
		t.Error("ParseMealPlanEntryID(empty) = nil error, want error")
	}
}
