package domain_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

type stubSource struct {
	matches []domain.RecipeMatch
	err     error
}

func (s stubSource) FindByIngredients(context.Context, household.HouseholdID, []tracking.IngredientID) ([]domain.RecipeMatch, error) {
	return s.matches, s.err
}

func matchWithID(id domain.RecipeID, title string, pct float64) domain.RecipeMatch {
	return domain.RecipeMatch{
		Recipe:   domain.Recipe{ID: id, Title: title, Source: domain.SourceLocal, Servings: 1},
		MatchPct: pct,
	}
}

func mustHybrid(t *testing.T, local, external domain.RecipeSource) *domain.HybridRecipeSource {
	t.Helper()
	h, err := domain.NewHybridRecipeSource(local, external)
	if err != nil {
		t.Fatalf("NewHybridRecipeSource: %v", err)
	}
	return h
}

func TestNewHybridRejectsNilSources(t *testing.T) {
	if _, err := domain.NewHybridRecipeSource(nil, stubSource{}); err == nil {
		t.Error("expected error for nil local source, got nil")
	}
	if _, err := domain.NewHybridRecipeSource(stubSource{}, nil); err == nil {
		t.Error("expected error for nil external source, got nil")
	}
}

func TestHybridReturnsLocalThenExternal(t *testing.T) {
	boxMatch := matchWithID(domain.NewRecipeID(), "Box Recipe", 1.0)
	extMatch := matchWithID(domain.NewRecipeID(), "Discovered", 0.5)
	hybrid := mustHybrid(t,
		stubSource{matches: []domain.RecipeMatch{boxMatch}},
		stubSource{matches: []domain.RecipeMatch{extMatch}},
	)

	got, err := hybrid.FindByIngredients(context.Background(), household.NewHouseholdID(), nil)
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}
	if len(got) != 2 || got[0].Recipe.Title != "Box Recipe" || got[1].Recipe.Title != "Discovered" {
		t.Errorf("combined = %v, want [Box Recipe, Discovered] (local first)", titlesOf(got))
	}
}

func TestHybridDedupsByRecipeID(t *testing.T) {
	shared := domain.NewRecipeID()
	hybrid := mustHybrid(t,
		stubSource{matches: []domain.RecipeMatch{matchWithID(shared, "Shared", 1.0)}},
		stubSource{matches: []domain.RecipeMatch{
			matchWithID(shared, "Shared (external copy)", 0.9),
			matchWithID(domain.NewRecipeID(), "New", 0.5),
		}},
	)

	got, err := hybrid.FindByIngredients(context.Background(), household.NewHouseholdID(), nil)
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("combined = %d, want 2 (shared id de-duplicated)", len(got))
	}
	if got[0].Recipe.Title != "Shared" || got[1].Recipe.Title != "New" {
		t.Errorf("combined = %v, want [Shared (local kept), New]", titlesOf(got))
	}
}

func TestHybridDegradesOnExternalError(t *testing.T) {
	boxMatch := matchWithID(domain.NewRecipeID(), "Box Recipe", 1.0)
	hybrid := mustHybrid(t,
		stubSource{matches: []domain.RecipeMatch{boxMatch}},
		stubSource{err: errors.New("provider down")},
	)

	got, err := hybrid.FindByIngredients(context.Background(), household.NewHouseholdID(), nil)
	if err != nil {
		t.Fatalf("FindByIngredients should degrade, got error: %v", err)
	}
	if len(got) != 1 || got[0].Recipe.Title != "Box Recipe" {
		t.Errorf("combined = %v, want [Box Recipe] (box-only on external failure)", titlesOf(got))
	}
}

func TestHybridPropagatesLocalError(t *testing.T) {
	sentinel := errors.New("box read failed")
	hybrid := mustHybrid(t,
		stubSource{err: sentinel},
		stubSource{matches: []domain.RecipeMatch{matchWithID(domain.NewRecipeID(), "X", 1.0)}},
	)
	if _, err := hybrid.FindByIngredients(context.Background(), household.NewHouseholdID(), nil); !errors.Is(err, sentinel) {
		t.Errorf("FindByIngredients error = %v, want local sentinel", err)
	}
}

func titlesOf(matches []domain.RecipeMatch) []string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Recipe.Title
	}
	return out
}
