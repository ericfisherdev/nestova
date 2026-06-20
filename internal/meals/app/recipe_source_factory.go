package app

import (
	"errors"

	"github.com/ericfisherdev/nestova/internal/meals/domain"
)

// SelectRecipeSource returns the RecipeSource the finder should use given the
// external-provider toggle: the recipe box alone when external lookups are
// disabled, or the hybrid (box first, external as "discover more") when enabled.
// Callers depend only on the RecipeSource port; the composition root supplies the
// concrete local and external sources and the toggle from config (DIP).
func SelectRecipeSource(externalEnabled bool, local, external domain.RecipeSource) (domain.RecipeSource, error) {
	if local == nil {
		return nil, errors.New("app: SelectRecipeSource requires a non-nil local source")
	}
	if !externalEnabled {
		return local, nil
	}
	if external == nil {
		return nil, errors.New("app: SelectRecipeSource requires a non-nil external source when external lookups are enabled")
	}
	hybrid, err := domain.NewHybridRecipeSource(local, external)
	if err != nil {
		return nil, err
	}
	return hybrid, nil
}
