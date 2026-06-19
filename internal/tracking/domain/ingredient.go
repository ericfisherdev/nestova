package domain

import (
	"context"
	"errors"
	"strings"
)

// Ingredient errors.
var (
	// ErrIngredientNotFound is returned by IngredientResolver.Resolve when no
	// canonical ingredient matches the supplied name (after normalization and
	// plural/alias lookup).
	ErrIngredientNotFound = errors.New("tracking: ingredient not found")
	// ErrInvalidIngredient is returned when a name is empty after normalization.
	ErrInvalidIngredient = errors.New("tracking: invalid ingredient name")
)

// Ingredient is the canonical, household-agnostic catalogue entry that pantry,
// shopping, and (later) meals all key off of. CanonicalName is the normalized
// primary name; Aliases are additional normalized names that resolve to it.
type Ingredient struct {
	ID            IngredientID
	CanonicalName string
	Aliases       []string
}

// IngredientResolver is the read side (CQS query port): it maps a free-text name
// to an existing canonical ingredient without creating anything.
type IngredientResolver interface {
	// Resolve returns the ingredient whose canonical name or aliases match name
	// after normalization and plural folding, or ErrIngredientNotFound. Empty
	// input yields ErrInvalidIngredient.
	Resolve(ctx context.Context, name string) (*Ingredient, error)
}

// IngredientEnsurer is the write side (CQS command port): it idempotently
// creates the canonical ingredient for a name, returning the existing row when
// one already exists. Implementations must be race-safe under concurrent
// EnsureIngredient calls for the same name.
type IngredientEnsurer interface {
	// EnsureIngredient normalizes name to a canonical name and upserts it,
	// returning the resulting ingredient. Empty input yields ErrInvalidIngredient.
	EnsureIngredient(ctx context.Context, name string) (*Ingredient, error)
}

// IngredientNamer is the read side (CQS query port) for display: it batch-maps
// ingredient ids to their canonical names so a pantry or shopping list can label
// ingredient-keyed rows without an N+1 lookup. It exists separately from
// IngredientResolver (which maps names → ingredients) so a UI that only needs
// names does not depend on the resolver's matching surface (ISP).
type IngredientNamer interface {
	// NamesByIDs returns a map from each supplied ingredient id to its canonical
	// name. Ids that do not exist are omitted from the map (not an error), so the
	// caller can fall back gracefully. An empty input yields an empty map.
	NamesByIDs(ctx context.Context, ids []IngredientID) (map[IngredientID]string, error)
}

// NormalizeName lower-cases, trims, and collapses internal whitespace so that
// "  Olive   Oil " and "olive oil" map to the same canonical form. It does not
// fold plurals — that is the resolver's job via ResolutionCandidates.
func NormalizeName(raw string) string {
	return strings.Join(strings.Fields(strings.ToLower(raw)), " ")
}

// ResolutionCandidates returns the distinct normalized names to match a lookup
// against: the normalized input plus any best-effort singular forms. The
// resolver matches any candidate against canonical_name or aliases, so emitting
// several plausible singulars is safe — an extra candidate only matches if a row
// with that exact name actually exists. Returns nil for input that normalizes to
// empty.
func ResolutionCandidates(raw string) []string {
	n := NormalizeName(raw)
	if n == "" {
		return nil
	}
	candidates := []string{n}
	for _, sg := range singularCandidates(n) {
		if sg != "" && !contains(candidates, sg) {
			candidates = append(candidates, sg)
		}
	}
	return candidates
}

// singularCandidates returns plausible singular forms for a plural word. English
// plural rules are ambiguous from spelling alone (e.g. "cookies" -> "cookie" but
// "berries" -> "berry", both consonant+"ies"), so rather than guess one form it
// emits every reasonable candidate and lets the resolver pick the one that
// exists. It returns nil for words that are not plurals. Suffixes handled are
// ASCII, so byte slicing always lands on a UTF-8 boundary.
func singularCandidates(s string) []string {
	if len(s) < 2 || !strings.HasSuffix(s, "s") || strings.HasSuffix(s, "ss") {
		return nil // "flour" is not plural; "glass"/"grass" end in -ss, also not plurals
	}
	// Each slice below is length-guarded so it can never yield the empty string
	// (e.g. "es" must not produce ""), which would be a meaningless candidate.
	//
	// Drop the trailing "s": eggs -> egg, cookies -> cookie, houses -> house.
	out := []string{s[:len(s)-1]}
	// "...es" frequently drops the whole "es": boxes -> box, dishes -> dish,
	// tomatoes -> tomato, glasses -> glass. Requires >2 chars so the stem survives.
	if strings.HasSuffix(s, "es") && len(s) > 2 {
		out = append(out, s[:len(s)-2])
	}
	// Consonant+"ies" -> "y": berries -> berry, cherries -> cherry. ("cookie"
	// is already covered by the plain trailing-"s" drop above.)
	if strings.HasSuffix(s, "ies") && len(s) > 3 {
		out = append(out, s[:len(s)-3]+"y")
	}
	return out
}

// contains reports whether xs already holds x.
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
