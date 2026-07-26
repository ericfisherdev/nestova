// Package adapter is the federation bounded context's infrastructure layer:
// the Postgres instance-link repository, the Nestorage HTTP client, and the
// /settings page's federation section handlers (NSTR-106).
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// InstanceLinkRepository is the pgx-backed domain.InstanceLinkRepository.
// UUIDs are passed and scanned as text, matching the other adapters.
type InstanceLinkRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.InstanceLinkRepository = (*InstanceLinkRepository)(nil)

// NewInstanceLinkRepository constructs the repository with an injected
// query executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewInstanceLinkRepository(dbtx db.TX) *InstanceLinkRepository {
	if dbtx == nil {
		panic("federation/adapter: NewInstanceLinkRepository requires a non-nil db.TX")
	}
	return &InstanceLinkRepository{dbtx: dbtx}
}

const instanceLinkColumns = `SELECT id, household_id, base_url, api_key_enc, attached_by, attached_at FROM federation_instance_link`

// Create persists link and populates its AttachedAt. Returns
// household.ErrHouseholdNotFound when the household is unknown,
// domain.ErrHouseholdAlreadyBound when it already has a link, or
// domain.ErrInstanceAlreadyBound when the base url is already linked to a
// different household.
func (r *InstanceLinkRepository) Create(ctx context.Context, link *domain.InstanceLink) error {
	if link == nil {
		return errors.New("federation/adapter: create instance link: nil link")
	}
	var attachedBy *string
	if link.AttachedBy != nil {
		s := link.AttachedBy.String()
		attachedBy = &s
	}
	const q = `
		INSERT INTO federation_instance_link (id, household_id, base_url, api_key_enc, attached_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING attached_at`
	err := r.dbtx.QueryRow(ctx, q,
		link.ID.String(), link.HouseholdID.String(), link.BaseURL, link.APIKeyEnc, attachedBy,
	).Scan(&link.AttachedAt)
	if err != nil {
		if mapped := mapCreateError(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create instance link: %w", err)
	}
	return nil
}

// GetByHousehold returns householdID's link, or domain.ErrLinkNotFound.
func (r *InstanceLinkRepository) GetByHousehold(ctx context.Context, householdID household.HouseholdID) (*domain.InstanceLink, error) {
	link, err := scanInstanceLink(r.dbtx.QueryRow(ctx, instanceLinkColumns+` WHERE household_id = $1`, householdID.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrLinkNotFound
		}
		return nil, fmt.Errorf("get instance link by household: %w", err)
	}
	return link, nil
}

// GetByBaseURL returns the link bound to the already-normalized baseURL, or
// domain.ErrLinkNotFound. The lookup itself still normalizes via
// lower(base_url) so it agrees with the storage-level unique index
// regardless of case.
func (r *InstanceLinkRepository) GetByBaseURL(ctx context.Context, baseURL string) (*domain.InstanceLink, error) {
	const q = instanceLinkColumns + ` WHERE lower(base_url) = lower($1)`
	link, err := scanInstanceLink(r.dbtx.QueryRow(ctx, q, baseURL))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrLinkNotFound
		}
		return nil, fmt.Errorf("get instance link by base url: %w", err)
	}
	return link, nil
}

// DeleteByHousehold removes householdID's link. It does not error when no
// link exists — Detach is idempotent.
func (r *InstanceLinkRepository) DeleteByHousehold(ctx context.Context, householdID household.HouseholdID) error {
	const q = `DELETE FROM federation_instance_link WHERE household_id = $1`
	if _, err := r.dbtx.Exec(ctx, q, householdID.String()); err != nil {
		return fmt.Errorf("delete instance link: %w", err)
	}
	return nil
}

// row is the read surface shared by pgx.Row and pgx.Rows for scan helpers,
// mirroring kiosk/adapter's identically named interface.
type row interface {
	Scan(dest ...any) error
}

func scanInstanceLink(r row) (*domain.InstanceLink, error) {
	var (
		link         domain.InstanceLink
		idStr, hhStr string
		attachedBy   *string
	)
	if err := r.Scan(&idStr, &hhStr, &link.BaseURL, &link.APIKeyEnc, &attachedBy, &link.AttachedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseInstanceLinkID(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse instance link id: %w", err)
	}
	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("parse household id: %w", err)
	}
	link.ID = id
	link.HouseholdID = hh
	if attachedBy != nil {
		memberID, err := household.ParseMemberID(*attachedBy)
		if err != nil {
			return nil, fmt.Errorf("parse attached-by member id: %w", err)
		}
		link.AttachedBy = &memberID
	}
	return &link, nil
}
