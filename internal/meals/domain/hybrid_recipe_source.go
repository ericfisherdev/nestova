package domain

import (
	"context"
	"errors"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// HybridRecipeSource composes the recipe box with an external "discover more"
// source: it returns the box matches first, then appends the external matches the
// box did not already cover (de-duplicated by recipe id). A failure of the
// external source degrades gracefully to box-only results — a provider outage must
// never break the finder, so the external error is intentionally dropped — while a
// failure of the local source is returned, since the box is the primary result.
type HybridRecipeSource struct {
	local    RecipeSource
	external RecipeSource
}

// Compile-time assurance the composite satisfies the port.
var _ RecipeSource = (*HybridRecipeSource)(nil)

// NewHybridRecipeSource constructs the composite from a local and an external
// source.
func NewHybridRecipeSource(local, external RecipeSource) (*HybridRecipeSource, error) {
	if local == nil {
		return nil, errors.New("domain: NewHybridRecipeSource requires a non-nil local source")
	}
	if external == nil {
		return nil, errors.New("domain: NewHybridRecipeSource requires a non-nil external source")
	}
	return &HybridRecipeSource{local: local, external: external}, nil
}

// FindByIngredients returns the box matches followed by the external matches the
// box did not already include. If the external source errors, it returns the box
// matches alone (graceful degradation).
func (h *HybridRecipeSource) FindByIngredients(ctx context.Context, householdID household.HouseholdID, have []tracking.IngredientID) ([]RecipeMatch, error) {
	local, err := h.local.FindByIngredients(ctx, householdID, have)
	if err != nil {
		return nil, err
	}
	external, err := h.external.FindByIngredients(ctx, householdID, have)
	if err != nil {
		// Degrade to box-only: a provider outage should not fail the finder.
		return local, nil
	}

	seen := make(map[RecipeID]struct{}, len(local))
	for _, match := range local {
		seen[match.Recipe.ID] = struct{}{}
	}
	// Copy rather than alias the local source's slice: appending external matches
	// must not write into the backing array of a slice another caller may hold.
	combined := append([]RecipeMatch(nil), local...)
	for _, match := range external {
		if _, dup := seen[match.Recipe.ID]; dup {
			continue
		}
		seen[match.Recipe.ID] = struct{}{}
		combined = append(combined, match)
	}
	return combined, nil
}
