package app_test

import (
	"context"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

func sourceMatch(title string, source domain.RecipeSourceKind) domain.RecipeMatch {
	return domain.RecipeMatch{Recipe: domain.Recipe{ID: domain.NewRecipeID(), Title: title, Source: source, Servings: 1}}
}

func TestSelectRecipeSourceDisabledServesBoxOnly(t *testing.T) {
	local := &fakeRecipeSource{matches: []domain.RecipeMatch{sourceMatch("Box", domain.SourceLocal)}}
	external := &fakeRecipeSource{matches: []domain.RecipeMatch{sourceMatch("External", domain.SourceExternal)}}

	src, err := app.SelectRecipeSource(false, local, external)
	if err != nil {
		t.Fatalf("SelectRecipeSource: %v", err)
	}
	got, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), nil)
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}
	if len(got) != 1 || got[0].Recipe.Title != "Box" {
		t.Errorf("disabled finder = %v, want [Box] only", got)
	}
	// The external source must not be consulted when disabled.
	if external.gotHousehold != (household.HouseholdID{}) {
		t.Error("external source was queried while disabled")
	}
}

func TestSelectRecipeSourceEnabledUsesHybrid(t *testing.T) {
	local := &fakeRecipeSource{matches: []domain.RecipeMatch{sourceMatch("Box", domain.SourceLocal)}}
	external := &fakeRecipeSource{matches: []domain.RecipeMatch{sourceMatch("External", domain.SourceExternal)}}

	src, err := app.SelectRecipeSource(true, local, external)
	if err != nil {
		t.Fatalf("SelectRecipeSource: %v", err)
	}
	got, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), nil)
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}
	if len(got) != 2 || got[0].Recipe.Title != "Box" || got[1].Recipe.Title != "External" {
		t.Errorf("enabled finder = %v, want [Box, External] (hybrid)", got)
	}
	if external.gotHousehold == (household.HouseholdID{}) {
		t.Error("external source was not consulted while enabled")
	}
}

func TestSelectRecipeSourceEnabledRejectsNilExternal(t *testing.T) {
	local := &fakeRecipeSource{matches: []domain.RecipeMatch{sourceMatch("Box", domain.SourceLocal)}}
	if _, err := app.SelectRecipeSource(true, local, nil); err == nil {
		t.Error("expected error for nil external source when enabled, got nil")
	}
}

func TestSelectRecipeSourceRejectsNilLocal(t *testing.T) {
	external := &fakeRecipeSource{matches: []domain.RecipeMatch{sourceMatch("External", domain.SourceExternal)}}
	if _, err := app.SelectRecipeSource(false, nil, external); err == nil {
		t.Error("expected error for nil local source (disabled), got nil")
	}
	if _, err := app.SelectRecipeSource(true, nil, external); err == nil {
		t.Error("expected error for nil local source (enabled), got nil")
	}
}
