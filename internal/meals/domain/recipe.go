package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// Recipe errors.
var (
	// ErrRecipeNotFound is returned when a recipe does not exist (or is not
	// visible to the household performing the lookup).
	ErrRecipeNotFound = errors.New("meals: recipe not found")
	// ErrInvalidRecipe is returned when a recipe violates its invariants: a blank
	// title, non-positive servings, or a source/ownership combination that does
	// not match RecipeSourceKind (local recipes are household-owned with no
	// external ref; external recipes are household-agnostic and carry one).
	ErrInvalidRecipe = errors.New("meals: invalid recipe")
)

// RecipeIngredient is one normalized ingredient line of a recipe, keyed to the
// shared catalogue (NES-38). Optional lines are not required to cook the recipe
// and never count against an ingredient-finder "missing" set. IngredientID must
// reference a catalogue ingredient: callers obtain it from the IngredientEnsurer
// (NES-57 normalizes each free-text line via EnsureIngredient) or NewIngredientID,
// and the recipe_ingredient → ingredient foreign key enforces its existence at
// the schema — so, as with the pantry and shopping-list lines, it is not
// re-validated here.
type RecipeIngredient struct {
	IngredientID tracking.IngredientID
	Quantity     household.Quantity
	Optional     bool
}

// Validate reports whether the line is well-formed: a valid, strictly positive
// Quantity (household.ErrInvalidQuantity otherwise — a zero amount does not
// describe an ingredient a recipe uses). IngredientID is not checked here — see
// the type doc for why the catalogue foreign key owns that invariant.
func (l RecipeIngredient) Validate() error {
	if err := l.Quantity.Validate(); err != nil {
		return err
	}
	if l.Quantity.Amount <= 0 {
		return fmt.Errorf("%w: recipe ingredient amount must be positive", household.ErrInvalidQuantity)
	}
	return nil
}

// Recipe is a cooking recipe: a household's box recipe (Source == SourceLocal,
// HouseholdID set) or an external/cached recipe shared across households
// (Source == SourceExternal, HouseholdID nil, ExternalRef set). Ingredients are
// normalized to the shared catalogue. Servings is the yield the ingredient
// amounts are expressed for, used to scale when planning (NES-61).
type Recipe struct {
	ID           RecipeID
	HouseholdID  *household.HouseholdID
	Title        string
	Source       RecipeSourceKind
	ExternalRef  *string
	Servings     int
	Instructions string
	Ingredients  []RecipeIngredient
}

// Validate reports whether the recipe is well-formed, returning ErrInvalidRecipe
// for a blank title, non-positive servings, or a source/ownership mismatch, and
// the wrapped household.ErrInvalidQuantity for a malformed ingredient line. The
// ingredient set may be empty: a box recipe can be saved before its lines are
// filled and an external/cached recipe may arrive without parsed ingredients, so
// any "must list ingredients" rule belongs to the use case that needs it (the
// recipe-box service, NES-57), not the aggregate invariant.
func (r *Recipe) Validate() error {
	if strings.TrimSpace(r.Title) == "" {
		return fmt.Errorf("%w: title must not be blank", ErrInvalidRecipe)
	}
	if r.Servings <= 0 {
		return fmt.Errorf("%w: servings must be positive, got %d", ErrInvalidRecipe, r.Servings)
	}
	if !r.Source.Valid() {
		return fmt.Errorf("%w: unknown source %q", ErrInvalidRecipe, r.Source)
	}
	if err := r.validateOwnership(); err != nil {
		return err
	}
	for _, line := range r.Ingredients {
		if err := line.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateOwnership enforces the source/ownership invariant mirrored by the
// recipe_source_identity_chk CHECK constraint.
func (r *Recipe) validateOwnership() error {
	// ExternalRef keys the external-recipe cache (UpsertExternal), so it must be
	// stored normalized — an untrimmed value would split the cache. Reject any
	// leading/trailing whitespace, mirroring the catalogue's canonical_name CHECK.
	if r.ExternalRef != nil && *r.ExternalRef != strings.TrimSpace(*r.ExternalRef) {
		return fmt.Errorf("%w: external ref must not have leading or trailing whitespace", ErrInvalidRecipe)
	}
	hasExternalRef := r.ExternalRef != nil && strings.TrimSpace(*r.ExternalRef) != ""
	switch r.Source {
	case SourceLocal:
		if r.HouseholdID == nil || hasExternalRef {
			return fmt.Errorf("%w: a local recipe is household-owned with no external ref", ErrInvalidRecipe)
		}
	case SourceExternal:
		if r.HouseholdID != nil || !hasExternalRef {
			return fmt.Errorf("%w: an external recipe is household-agnostic and carries an external ref", ErrInvalidRecipe)
		}
	default:
		// Unreachable while Validate guards r.Source.Valid() first, but fail loudly
		// if a new RecipeSourceKind is added without an ownership rule here rather
		// than silently treating it as valid.
		return fmt.Errorf("%w: unhandled source %q", ErrInvalidRecipe, r.Source)
	}
	return nil
}

// RecipeRepository persists recipes together with their ingredient lines.
//
// Persistence contracts (the caller sets identity and a valid Recipe):
//   - Create inserts a household box recipe and its ingredient lines atomically
//     (all-or-nothing). It expects Source == SourceLocal with HouseholdID set.
//   - Update rewrites a box recipe's fields and replaces its ingredient set
//     atomically, returning ErrRecipeNotFound when the id is unknown in the
//     household.
//   - UpsertExternal inserts or refreshes an external/cached recipe keyed by
//     ExternalRef (Source == SourceExternal, HouseholdID nil), returning the
//     stored recipe. Re-caching the same ExternalRef replaces the prior row's
//     fields and lines rather than duplicating it. External recipes are a
//     write-through cache for the discover-more flow: they are surfaced to
//     members through the RecipeSource finder (NES-58/59), never assigned to a
//     plan slot (the meal_plan_entry FK admits only household-owned recipes), so
//     this port intentionally exposes no by-id retrieval for them. A cache read
//     path is added alongside its first consumer in NES-59.
//
// Error contracts:
//   - Get and Delete look up a household-owned (local) recipe scoped to
//     householdID and return ErrRecipeNotFound when the id is unknown in that
//     household (so a member cannot read or remove another household's recipe, and
//     external recipes — which carry no household — are out of their scope by
//     design). Get returns the recipe with its ingredient lines populated.
//   - ListByHousehold returns the household's box recipes ordered by title, or an
//     empty slice when none exist.
type RecipeRepository interface {
	Create(ctx context.Context, recipe *Recipe) error
	Update(ctx context.Context, recipe *Recipe) error
	UpsertExternal(ctx context.Context, recipe *Recipe) (*Recipe, error)
	Get(ctx context.Context, householdID household.HouseholdID, id RecipeID) (*Recipe, error)
	Delete(ctx context.Context, householdID household.HouseholdID, id RecipeID) error
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*Recipe, error)
}
