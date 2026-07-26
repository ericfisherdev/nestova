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

// memberLinkColumns is nestorage_member_link's own SELECT projection,
// shared by ListByHousehold and Put's own idempotent-retry lookup.
const memberLinkColumns = `SELECT member_id, household_id, remote_user_id, linked_via, linked_at FROM nestorage_member_link`

// MemberLinkRepository is the pgx-backed domain.MemberLinkRepository
// (NSTR-107).
type MemberLinkRepository struct {
	dbtx db.TX
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.MemberLinkRepository = (*MemberLinkRepository)(nil)

// NewMemberLinkRepository constructs the repository with an injected query
// executor (a db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewMemberLinkRepository(dbtx db.TX) *MemberLinkRepository {
	if dbtx == nil {
		panic("federation/adapter: NewMemberLinkRepository requires a non-nil db.TX")
	}
	return &MemberLinkRepository{dbtx: dbtx}
}

// ListByHousehold returns every existing link for householdID, in no
// particular order.
func (r *MemberLinkRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]domain.MemberLink, error) {
	rows, err := r.dbtx.Query(ctx, memberLinkColumns+` WHERE household_id = $1`, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list member links: %w", err)
	}
	defer rows.Close()

	var links []domain.MemberLink
	for rows.Next() {
		link, err := scanMemberLink(rows)
		if err != nil {
			return nil, fmt.Errorf("list member links: %w", err)
		}
		links = append(links, *link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list member links: %w", err)
	}
	return links, nil
}

// Put idempotently records link: ON CONFLICT (member_id) DO NOTHING makes
// a second call for a member who already has a link a no-op at the SQL
// level rather than a primary-key violation; the follow-up read below is
// what tells an idempotent replay (the existing row already names the SAME
// remote_user_id — success, unchanged) apart from a genuine conflict (the
// existing row names a DIFFERENT remote_user_id — domain.ErrLinkConflict;
// in practice Confirm's own caller already guarantees this path is never
// reached for an already-linked member, but Put itself does not trust
// that). A violation of nestorage_member_link_remote_user_id_uniq — a
// DIFFERENT member already claims link.RemoteUserID — is the other case
// Put rejects, also as domain.ErrLinkConflict.
func (r *MemberLinkRepository) Put(ctx context.Context, link domain.MemberLink) error {
	const q = `
		INSERT INTO nestorage_member_link (member_id, household_id, remote_user_id, linked_via)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (member_id) DO NOTHING`
	tag, err := r.dbtx.Exec(ctx, q, link.MemberID.String(), link.HouseholdID.String(), link.RemoteUserID, link.LinkedVia.String())
	if err != nil {
		if mapped := mapMemberLinkPutError(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("put member link: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	existing, err := scanMemberLink(r.dbtx.QueryRow(ctx, memberLinkColumns+` WHERE member_id = $1`, link.MemberID.String()))
	if err != nil {
		return fmt.Errorf("put member link: read existing row: %w", err)
	}
	if existing.RemoteUserID != link.RemoteUserID {
		return domain.ErrLinkConflict
	}
	return nil
}

func scanMemberLink(r scanner) (*domain.MemberLink, error) {
	var (
		link                        domain.MemberLink
		memberIDStr, householdIDStr string
		viaStr                      string
	)
	if err := r.Scan(&memberIDStr, &householdIDStr, &link.RemoteUserID, &viaStr, &link.LinkedAt); err != nil {
		return nil, err
	}
	memberID, err := household.ParseMemberID(memberIDStr)
	if err != nil {
		return nil, fmt.Errorf("parse member id: %w", err)
	}
	householdID, err := household.ParseHouseholdID(householdIDStr)
	if err != nil {
		return nil, fmt.Errorf("parse household id: %w", err)
	}
	via, err := domain.ParseLinkOrigin(viaStr)
	if err != nil {
		return nil, fmt.Errorf("parse linked_via: %w", err)
	}
	link.MemberID = memberID
	link.HouseholdID = householdID
	link.LinkedVia = via
	return &link, nil
}

// MemberReader is the pgx-backed narrow read this context needs onto
// household members — id, display name, role, and login email — mirroring
// notify's own PostgresContactDirectory precedent of reading the exact
// member columns a context needs directly, rather than depending on
// household.Repository's much wider surface (ISP).
type MemberReader struct {
	dbtx db.TX
}

// NewMemberReader constructs the reader with an injected query executor (a
// db.TX, satisfied by both *pgxpool.Pool and pgx.Tx).
func NewMemberReader(dbtx db.TX) *MemberReader {
	if dbtx == nil {
		panic("federation/adapter: NewMemberReader requires a non-nil db.TX")
	}
	return &MemberReader{dbtx: dbtx}
}

// MembersByHousehold returns householdID's members ordered by creation
// (mirroring household's own PostgresRepository.ListMembers ordering),
// each carrying its login email when one is on file (member.email, citext,
// nullable — nil here means the member has no login credential yet, which
// domain.ProposeMatches always treats as unmatched).
func (r *MemberReader) MembersByHousehold(ctx context.Context, householdID household.HouseholdID) ([]domain.MemberCandidate, error) {
	const q = `SELECT id, display_name, role, email FROM member WHERE household_id = $1 ORDER BY created_at, id`
	rows, err := r.dbtx.Query(ctx, q, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list members for reconciliation: %w", err)
	}
	defer rows.Close()

	var members []domain.MemberCandidate
	for rows.Next() {
		var (
			idStr, displayName, roleStr string
			email                       *string
		)
		if err := rows.Scan(&idStr, &displayName, &roleStr, &email); err != nil {
			return nil, fmt.Errorf("list members for reconciliation: %w", err)
		}
		id, err := household.ParseMemberID(idStr)
		if err != nil {
			return nil, fmt.Errorf("list members for reconciliation: parse member id: %w", err)
		}
		role, err := household.ParseRole(roleStr)
		if err != nil {
			return nil, fmt.Errorf("list members for reconciliation: parse role: %w", err)
		}
		members = append(members, domain.MemberCandidate{MemberID: id, DisplayName: displayName, Role: role, Email: email})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members for reconciliation: %w", err)
	}
	return members, nil
}

// scanner is the read surface shared by pgx.Row and pgx.Rows for scan
// helpers — named for its single Scan method per the codebase's
// single-method-interface convention (e.g. media/adapter's queryRower,
// txBeginner), rather than kiosk/adapter's identically-shaped but
// unrenamed row.
type scanner interface {
	Scan(dest ...any) error
}

func scanInstanceLink(r scanner) (*domain.InstanceLink, error) {
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
