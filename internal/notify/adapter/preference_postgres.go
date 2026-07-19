package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// PostgresPreferenceRepository is the pgx-backed implementation of
// domain.PreferenceRepository (NES-139), against the
// member_notification_pref table.
type PostgresPreferenceRepository struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.PreferenceRepository = (*PostgresPreferenceRepository)(nil)

// NewPostgresPreferenceRepository constructs the repository with an
// injected pgx pool.
func NewPostgresPreferenceRepository(pool *pgxpool.Pool) *PostgresPreferenceRepository {
	if pool == nil {
		panic("adapter: NewPostgresPreferenceRepository requires a non-nil pool")
	}
	return &PostgresPreferenceRepository{pool: pool}
}

// Get returns memberID's explicit channel preference for eventType, or
// domain.ErrPreferenceNotFound when no row exists.
func (r *PostgresPreferenceRepository) Get(ctx context.Context, memberID household.MemberID, eventType domain.EventType) (domain.Channel, error) {
	const q = `SELECT channel FROM member_notification_pref WHERE member_id = $1 AND event_type = $2`
	var channelStr string
	if err := r.pool.QueryRow(ctx, q, memberID.String(), eventType.String()).Scan(&channelStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrPreferenceNotFound
		}
		return "", fmt.Errorf("get member preference: %w", err)
	}
	channel, err := domain.ParseChannel(channelStr)
	if err != nil {
		return "", fmt.Errorf("get member preference: %w", err)
	}
	return channel, nil
}

// Set upserts pref, replacing any existing preference for the same
// (member_id, event_type) pair.
func (r *PostgresPreferenceRepository) Set(ctx context.Context, pref domain.MemberPreference) error {
	const q = `
		INSERT INTO member_notification_pref (member_id, household_id, event_type, channel)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (member_id, event_type)
		DO UPDATE SET channel = EXCLUDED.channel, updated_at = now()`
	_, err := r.pool.Exec(ctx, q,
		pref.MemberID.String(), pref.HouseholdID.String(), pref.EventType.String(), pref.Channel.String())
	if err != nil {
		return fmt.Errorf("set member preference: %w", err)
	}
	return nil
}

// ListForMember returns every explicit preference memberID has set.
func (r *PostgresPreferenceRepository) ListForMember(ctx context.Context, memberID household.MemberID) ([]domain.MemberPreference, error) {
	const q = `SELECT household_id, event_type, channel FROM member_notification_pref WHERE member_id = $1`
	rows, err := r.pool.Query(ctx, q, memberID.String())
	if err != nil {
		return nil, fmt.Errorf("list member preferences: %w", err)
	}
	defer rows.Close()

	prefs := make([]domain.MemberPreference, 0)
	for rows.Next() {
		var hhIDStr, eventTypeStr, channelStr string
		if err := rows.Scan(&hhIDStr, &eventTypeStr, &channelStr); err != nil {
			return nil, fmt.Errorf("list member preferences: scan: %w", err)
		}
		hhID, err := household.ParseHouseholdID(hhIDStr)
		if err != nil {
			return nil, fmt.Errorf("list member preferences: %w", err)
		}
		eventType, err := domain.ParseEventType(eventTypeStr)
		if err != nil {
			return nil, fmt.Errorf("list member preferences: %w", err)
		}
		channel, err := domain.ParseChannel(channelStr)
		if err != nil {
			return nil, fmt.Errorf("list member preferences: %w", err)
		}
		prefs = append(prefs, domain.MemberPreference{
			HouseholdID: hhID, MemberID: memberID, EventType: eventType, Channel: channel,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list member preferences: %w", err)
	}
	return prefs, nil
}

// DowngradeChannel replaces every one of memberID's preference rows
// currently set to from with to. Matching zero rows is not an error — a
// member with no explicit from-channel preference at all is a normal
// outcome (see the port's own doc).
func (r *PostgresPreferenceRepository) DowngradeChannel(ctx context.Context, memberID household.MemberID, from, to domain.Channel) error {
	const q = `
		UPDATE member_notification_pref
		   SET channel = $1, updated_at = now()
		 WHERE member_id = $2 AND channel = $3`
	if _, err := r.pool.Exec(ctx, q, to.String(), memberID.String(), from.String()); err != nil {
		return fmt.Errorf("downgrade member preference channel: %w", err)
	}
	return nil
}
