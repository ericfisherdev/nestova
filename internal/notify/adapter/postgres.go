// Package adapter contains the notify context's outbound adapters: the
// Postgres Outbox implementation and the in-app Sender.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// OutboxRepository is the pgx-backed implementation of domain.Outbox. UUIDs
// are passed and scanned as text, matching the household adapter convention (no
// pgx UUID codec registration required).
type OutboxRepository struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the adapter satisfies the port.
var _ domain.Outbox = (*OutboxRepository)(nil)

// NewOutboxRepository constructs the repository with an injected pgx pool.
func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	if pool == nil {
		panic("adapter: NewOutboxRepository requires a non-nil pool")
	}
	return &OutboxRepository{pool: pool}
}

// Enqueue inserts a new notification into the outbox table. The caller is
// responsible for setting a valid ID, HouseholdID, Channel, Title, Body, and
// ScheduledFor. MemberID, SourceType, and SourceID are optional (nil/empty).
// The row is always persisted with StatusPending — enqueueing an
// already-terminal notification is not meaningful — and the entity's Status is
// updated to match.
func (r *OutboxRepository) Enqueue(ctx context.Context, n *domain.Notification) error {
	if n == nil {
		return errors.New("adapter: enqueue: nil notification")
	}
	n.Status = domain.StatusPending
	const q = `
		INSERT INTO notification
		    (id, household_id, member_id, channel, title, body, scheduled_for, status, source_type, source_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING created_at`

	var memberIDStr *string
	if n.MemberID != nil {
		s := n.MemberID.String()
		memberIDStr = &s
	}

	var sourceIDStr *string
	if n.SourceID != nil {
		s := n.SourceID.String()
		sourceIDStr = &s
	}

	var sourceType *string
	if n.SourceType != "" {
		sourceType = &n.SourceType
	}

	err := r.pool.QueryRow(ctx, q,
		n.ID.String(),
		n.HouseholdID.String(),
		memberIDStr,
		n.Channel.String(),
		n.Title,
		n.Body,
		n.ScheduledFor,
		n.Status.String(),
		sourceType,
		sourceIDStr,
	).Scan(&n.CreatedAt)
	if err != nil {
		return fmt.Errorf("enqueue notification: %w", err)
	}
	return nil
}

// ClaimDue atomically claims up to limit pending notifications whose
// scheduled_for is <= now() and transitions them from StatusPending to
// StatusSent. It returns the claimed notifications so the Dispatcher can
// attempt delivery.
//
// Design trade-off (optimistic claim): ClaimDue transitions rows to StatusSent
// but leaves sent_at NULL — the actual delivery time is stamped later by
// MarkSent. A claimed row therefore has (status=sent, sent_at=NULL) until it is
// delivered, while a delivered row has (status=sent, sent_at set). If the
// process crashes after claiming but before delivery, the row stays in that
// (sent, NULL) limbo and is never retried by this skeleton — so delivery is
// effectively at-most-once. The (sent, sent_at IS NULL) shape is deliberately
// left detectable so a future recovery sweep (out of scope here) can find and
// re-dispatch crashed claims. The alternative — holding a transaction open
// across the (potentially slow) Send call — risks long-held row locks starving
// other dispatchers, so the claim lock is released immediately instead.
func (r *OutboxRepository) ClaimDue(ctx context.Context, limit int) ([]*domain.Notification, error) {
	// A CTE selects the rows to claim with SKIP LOCKED (so concurrent dispatchers
	// never claim the same row) and the UPDATE transitions them atomically.
	const q = `
		WITH claimed AS (
			SELECT id
			  FROM notification
			 WHERE status      = 'pending'
			   AND scheduled_for <= now()
			 ORDER BY scheduled_for
			 LIMIT $1
			   FOR UPDATE SKIP LOCKED
		)
		UPDATE notification n
		   SET status = 'sent'
		  FROM claimed
		 WHERE n.id = claimed.id
		RETURNING n.id, n.household_id, n.member_id, n.channel, n.title, n.body,
		          n.scheduled_for, n.status, n.sent_at, n.source_type, n.source_id,
		          n.created_at`

	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("claim due notifications: %w", err)
	}
	defer rows.Close()

	var notifications []*domain.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("claim due notifications: scan: %w", err)
		}
		notifications = append(notifications, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim due notifications: %w", err)
	}
	return notifications, nil
}

// MarkSent sets the notification's status to StatusSent and records sent_at.
// Returns domain.ErrNotificationNotFound when id is unknown.
func (r *OutboxRepository) MarkSent(ctx context.Context, id domain.NotificationID) error {
	const q = `UPDATE notification SET status = 'sent', sent_at = now() WHERE id = $1`
	tag, err := r.pool.Exec(ctx, q, id.String())
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotificationNotFound
	}
	return nil
}

// MarkFailed sets the notification's status to StatusFailed.
// Returns domain.ErrNotificationNotFound when id is unknown.
func (r *OutboxRepository) MarkFailed(ctx context.Context, id domain.NotificationID) error {
	const q = `UPDATE notification SET status = 'failed' WHERE id = $1`
	tag, err := r.pool.Exec(ctx, q, id.String())
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotificationNotFound
	}
	return nil
}

// row abstracts pgx.Row and pgx.Rows for the shared scan helper.
type row interface {
	Scan(dest ...any) error
}

func scanNotification(r row) (*domain.Notification, error) {
	var (
		n                                     domain.Notification
		idStr, hhIDStr, channelStr, statusStr string
		memberIDStr, sourceIDStr, sourceType  *string
		sentAt                                *time.Time
	)
	if err := r.Scan(
		&idStr, &hhIDStr, &memberIDStr, &channelStr,
		&n.Title, &n.Body, &n.ScheduledFor, &statusStr, &sentAt,
		&sourceType, &sourceIDStr, &n.CreatedAt,
	); err != nil {
		return nil, err
	}

	id, err := domain.ParseNotificationID(idStr)
	if err != nil {
		return nil, fmt.Errorf("scan notification: %w", err)
	}
	hhID, err := household.ParseHouseholdID(hhIDStr)
	if err != nil {
		return nil, fmt.Errorf("scan notification: %w", err)
	}
	channel, err := domain.ParseChannel(channelStr)
	if err != nil {
		return nil, fmt.Errorf("scan notification: %w", err)
	}
	status, err := domain.ParseStatus(statusStr)
	if err != nil {
		return nil, fmt.Errorf("scan notification: %w", err)
	}

	n.ID = id
	n.HouseholdID = hhID
	n.Channel = channel
	n.Status = status
	n.SentAt = sentAt

	if memberIDStr != nil {
		mid, err := household.ParseMemberID(*memberIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		n.MemberID = &mid
	}

	if sourceType != nil {
		n.SourceType = *sourceType
	}

	if sourceIDStr != nil {
		sid, err := uuid.Parse(*sourceIDStr)
		if err != nil {
			return nil, fmt.Errorf("scan notification: parse source id: %w", err)
		}
		n.SourceID = &sid
	}

	return &n, nil
}
