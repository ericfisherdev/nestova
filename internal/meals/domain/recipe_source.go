package domain

import (
	"context"
	"sort"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// RecipeMatch is a recipe ranked against a set of on-hand ingredients: MatchPct is
// the fraction of the recipe's required (non-optional) ingredients the cook has,
// and Missing lists the required ingredients they still need (optional lines never
// appear here).
type RecipeMatch struct {
	Recipe   Recipe
	MatchPct float64
	Missing  []tracking.IngredientID
}

// RecipeSource is the swappable port behind the ingredient-driven "what can I
// make?" finder (ISP, one method). Implementations rank recipes against the
// household's on-hand ingredients: LocalRecipeSource over the recipe box (NES-58),
// ExternalRecipeSource over a provider, and HybridRecipeSource over both (NES-59).
// have is the de-duplicated set of catalogue ingredient ids the cook has.
type RecipeSource interface {
	FindByIngredients(ctx context.Context, householdID household.HouseholdID, have []tracking.IngredientID) ([]RecipeMatch, error)
}

// MatchByIngredients ranks recipes against the on-hand ingredient set have. For
// each recipe it scores MatchPct as matched / required over the non-optional
// lines (a recipe with no required lines scores 1.0) and collects the unmet
// required ingredients into Missing. Recipes the cook cannot make any of
// (MatchPct == 0) are dropped; the rest are returned ordered by MatchPct
// descending, then title. It performs no I/O, so the RecipeSource implementations
// share one ranking definition.
func MatchByIngredients(recipes []*Recipe, have []tracking.IngredientID) []RecipeMatch {
	haveSet := make(map[tracking.IngredientID]struct{}, len(have))
	for _, id := range have {
		haveSet[id] = struct{}{}
	}

	matches := make([]RecipeMatch, 0, len(recipes))
	for _, recipe := range recipes {
		required := 0
		matched := 0
		missing := make([]tracking.IngredientID, 0, len(recipe.Ingredients))
		for _, line := range recipe.Ingredients {
			if line.Optional {
				continue
			}
			required++
			if _, ok := haveSet[line.IngredientID]; ok {
				matched++
			} else {
				missing = append(missing, line.IngredientID)
			}
		}

		pct := 1.0
		if required > 0 {
			pct = float64(matched) / float64(required)
		}
		if pct == 0 {
			continue
		}
		matches = append(matches, RecipeMatch{Recipe: *recipe, MatchPct: pct, Missing: missing})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].MatchPct != matches[j].MatchPct {
			return matches[i].MatchPct > matches[j].MatchPct
		}
		return matches[i].Recipe.Title < matches[j].Recipe.Title
	})
	return matches
}
