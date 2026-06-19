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

// Compile-time assurance the repository satisfies the catalogue ports.
var (
	_ domain.IngredientResolver = (*IngredientRepository)(nil)
	_ domain.IngredientEnsurer  = (*IngredientRepository)(nil)
	_ domain.IngredientNamer    = (*IngredientRepository)(nil)
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
	// Ordering, most-preferred first:
	//  1. a canonical-name match beats an alias-only match, and
	//  2. among canonical matches, the earliest candidate wins — candidates are
	//     ordered [normalized input, singular forms...], so the exact input form
	//     is preferred over a singularized guess (e.g. an explicit "tomatoes"
	//     ingredient is chosen over a separate "tomato" one).
	// array_position is NULL for alias-only matches, so COALESCE pushes them last.
	const q = `
		SELECT id, canonical_name, aliases
		  FROM ingredient
		 WHERE canonical_name = ANY($1::text[]) OR aliases && $1::text[]
		 ORDER BY (canonical_name = ANY($1::text[])) DESC,
		          COALESCE(array_position($1::text[], canonical_name), 2147483647) ASC
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

// NamesByIDs batch-maps ingredient ids to their canonical names for display.
// Unknown ids are omitted from the result map; an empty input yields an empty
// map without touching the database. Ids are passed as text, matching the rest of
// this adapter (no pgx UUID codec registration required).
func (r *IngredientRepository) NamesByIDs(ctx context.Context, ids []domain.IngredientID) (map[domain.IngredientID]string, error) {
	names := make(map[domain.IngredientID]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	idStrings := make([]string, len(ids))
	for i, id := range ids {
		idStrings[i] = id.String()
	}
	const q = `SELECT id, canonical_name FROM ingredient WHERE id = ANY($1::uuid[])`
	rows, err := r.dbtx.Query(ctx, q, idStrings)
	if err != nil {
		return nil, fmt.Errorf("ingredient names by ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idStr, name string
		if err := rows.Scan(&idStr, &name); err != nil {
			return nil, fmt.Errorf("ingredient names by ids: scan: %w", err)
		}
		id, err := domain.ParseIngredientID(idStr)
		if err != nil {
			return nil, fmt.Errorf("ingredient names by ids: %w", err)
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ingredient names by ids: %w", err)
	}
	return names, nil
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
