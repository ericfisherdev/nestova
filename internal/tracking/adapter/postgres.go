package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ericfisherdev/nestova/internal/platform/db"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// IngredientRepository is the pgx-backed implementation of the ingredient
// catalogue ports. UUIDs are passed and scanned as text, matching the household
// and notify adapters (no pgx UUID codec registration required).
type IngredientRepository struct {
	dbtx db.TX
}

// Compile-time assurance the repository satisfies both catalogue ports.
var (
	_ domain.IngredientResolver = (*IngredientRepository)(nil)
	_ domain.IngredientEnsurer  = (*IngredientRepository)(nil)
)

// NewIngredientRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewIngredientRepository(dbtx db.TX) *IngredientRepository {
	if dbtx == nil {
		panic("adapter: NewIngredientRepository requires a non-nil db.TX")
	}
	return &IngredientRepository{dbtx: dbtx}
}

// Resolve maps a free-text name to an existing ingredient by matching the
// normalized name (and a best-effort singular form) against canonical_name or
// aliases. An exact canonical match is preferred over an alias/plural match.
// Returns domain.ErrInvalidIngredient for empty input and
// domain.ErrIngredientNotFound when nothing matches.
func (r *IngredientRepository) Resolve(ctx context.Context, name string) (*domain.Ingredient, error) {
	candidates := domain.ResolutionCandidates(name)
	if len(candidates) == 0 {
		return nil, domain.ErrInvalidIngredient
	}
	// canonical_name = ANY orders ahead of an aliases-only hit so an exact
	// primary-name match wins when both a canonical row and an alias row match.
	const q = `
		SELECT id, canonical_name, aliases
		  FROM ingredient
		 WHERE canonical_name = ANY($1::text[]) OR aliases && $1::text[]
		 ORDER BY (canonical_name = ANY($1::text[])) DESC
		 LIMIT 1`
	ing, err := scanIngredient(r.dbtx.QueryRow(ctx, q, candidates))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrIngredientNotFound
		}
		return nil, fmt.Errorf("resolve ingredient: %w", err)
	}
	return ing, nil
}

// EnsureIngredient idempotently creates the canonical ingredient for name and
// returns it. It is race-safe: INSERT ... ON CONFLICT DO NOTHING leaves any
// concurrently-inserted row intact, and the subsequent re-select returns the
// surviving row whether this call or another created it. Returns
// domain.ErrInvalidIngredient for input that normalizes to empty.
func (r *IngredientRepository) EnsureIngredient(ctx context.Context, name string) (*domain.Ingredient, error) {
	canonical := domain.NormalizeName(name)
	if canonical == "" {
		return nil, domain.ErrInvalidIngredient
	}

	const insert = `
		INSERT INTO ingredient (id, canonical_name)
		VALUES ($1, $2)
		ON CONFLICT (canonical_name) DO NOTHING`
	if _, err := r.dbtx.Exec(ctx, insert, domain.NewIngredientID().String(), canonical); err != nil {
		return nil, fmt.Errorf("ensure ingredient: insert: %w", err)
	}

	const sel = `SELECT id, canonical_name, aliases FROM ingredient WHERE canonical_name = $1`
	ing, err := scanIngredient(r.dbtx.QueryRow(ctx, sel, canonical))
	if err != nil {
		return nil, fmt.Errorf("ensure ingredient: reselect: %w", err)
	}
	return ing, nil
}

// row abstracts pgx.Row and pgx.Rows for the shared scan helper.
type row interface {
	Scan(dest ...any) error
}

func scanIngredient(r row) (*domain.Ingredient, error) {
	var (
		ing   domain.Ingredient
		idStr string
	)
	if err := r.Scan(&idStr, &ing.CanonicalName, &ing.Aliases); err != nil {
		return nil, err
	}
	id, err := domain.ParseIngredientID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan ingredient: %w", err)
	}
	ing.ID = id
	return &ing, nil
}
