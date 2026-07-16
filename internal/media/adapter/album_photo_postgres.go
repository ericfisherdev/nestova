package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// AlbumPhotoRepository is the pgx-backed domain.AlbumPhotoRepository: the ordered
// membership of photos in an album.
type AlbumPhotoRepository struct {
	dbtx db.TX
}

var _ domain.AlbumPhotoRepository = (*AlbumPhotoRepository)(nil)

// NewAlbumPhotoRepository constructs the repository with an injected query executor.
func NewAlbumPhotoRepository(dbtx db.TX) *AlbumPhotoRepository {
	if dbtx == nil {
		panic("media/adapter: NewAlbumPhotoRepository requires a non-nil db.TX")
	}
	return &AlbumPhotoRepository{dbtx: dbtx}
}

// addMaxRetries bounds the optimistic retry when two concurrent Adds pick the
// same next position; each retry recomputes MAX(position)+1.
const addMaxRetries = 5

// Add appends the photo at the next position. household_id is derived from the
// album so the composite tenant FKs bind the photo to the album's household; a
// photo from another household raises the album_photo_photo_fk violation, mapped
// to domain.ErrPhotoNotFound. An unknown album returns domain.ErrAlbumNotFound.
// Adding a photo already in the album is a no-op. Concurrent Adds that race on
// the next position are retried (the loser recomputes a fresh position).
func (r *AlbumPhotoRepository) Add(ctx context.Context, albumID domain.AlbumID, photoID domain.PhotoID) error {
	const q = `
		INSERT INTO album_photo (household_id, album_id, photo_id, position)
		SELECT a.household_id, a.id, $2,
		       COALESCE((SELECT MAX(position) + 1 FROM album_photo WHERE album_id = a.id), 0)
		  FROM album a
		 WHERE a.id = $1
		ON CONFLICT (album_id, photo_id) DO NOTHING`
	for attempt := 0; attempt < addMaxRetries; attempt++ {
		tag, err := r.dbtx.Exec(ctx, q, albumID.String(), photoID.String())
		if err != nil {
			// Two concurrent Adds chose the same position; the unique constraint
			// rejected the loser. Recompute MAX(position)+1 and retry.
			if isUniqueViolation(err, albumPhotoPositionUniq) {
				continue
			}
			if mapped := mapFKViolation(err); mapped != nil {
				return mapped
			}
			return fmt.Errorf("add album photo: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Zero rows means either the album does not exist (the SELECT found
			// nothing) or the photo is already a member (ON CONFLICT). Disambiguate
			// so a missing album is reported rather than silently succeeding.
			exists, err := r.albumExists(ctx, albumID)
			if err != nil {
				return err
			}
			if !exists {
				return domain.ErrAlbumNotFound
			}
		}
		return nil
	}
	return fmt.Errorf("add album photo: position contention after %d attempts", addMaxRetries)
}

func (r *AlbumPhotoRepository) albumExists(ctx context.Context, albumID domain.AlbumID) (bool, error) {
	var exists bool
	if err := r.dbtx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM album WHERE id = $1)`, albumID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("album exists: %w", err)
	}
	return exists, nil
}

// Remove drops the photo from the album; removing a photo not in the album is a
// no-op (idempotent).
func (r *AlbumPhotoRepository) Remove(ctx context.Context, albumID domain.AlbumID, photoID domain.PhotoID) error {
	if _, err := r.dbtx.Exec(ctx,
		`DELETE FROM album_photo WHERE album_id = $1 AND photo_id = $2`,
		albumID.String(), photoID.String()); err != nil {
		return fmt.Errorf("remove album photo: %w", err)
	}
	return nil
}

// Reorder sets the album's complete order from order, atomically. Because
// UNIQUE (album_id, position) is checked per row, a direct permutation would
// collide mid-statement; instead it runs in a transaction that first shifts
// every position above the current maximum (out of the target range, staying
// non-negative for the position >= 0 CHECK) and then assigns the final 0-based
// positions. order must be exactly the album's current membership.
func (r *AlbumPhotoRepository) Reorder(ctx context.Context, albumID domain.AlbumID, order []domain.PhotoID) error {
	if len(order) == 0 {
		return nil
	}
	return r.inTx(ctx, "reorder album photos", func(tx pgx.Tx) error {
		// Shift all current positions above the album's max so the assignment
		// below cannot collide with a not-yet-updated row.
		if _, err := tx.Exec(ctx,
			`UPDATE album_photo
			    SET position = position + (SELECT COALESCE(MAX(position), 0) + 1 FROM album_photo WHERE album_id = $1)
			  WHERE album_id = $1`, albumID.String()); err != nil {
			return fmt.Errorf("shift positions: %w", err)
		}
		args := []any{albumID.String()}
		values := make([]string, 0, len(order))
		for i, photoID := range order {
			args = append(args, photoID.String())
			// The position is an int from the loop index — safe to inline.
			values = append(values, fmt.Sprintf("($%d::uuid, %d)", len(args), i))
		}
		q := `
			UPDATE album_photo AS ap
			   SET position = v.pos
			  FROM (VALUES ` + strings.Join(values, ", ") + `) AS v(photo_id, pos)
			 WHERE ap.album_id = $1 AND ap.photo_id = v.photo_id`
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("apply order: %w", err)
		}
		// Enforce the complete-membership contract: every album_photo row must have
		// received a final position in [0, len(order)). Any row still carrying a
		// shifted (out-of-range) position means order omitted a current member, so
		// the whole reorder is rolled back rather than left with a gap.
		var leftover int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM album_photo WHERE album_id = $1 AND position >= $2`,
			albumID.String(), len(order)).Scan(&leftover); err != nil {
			return fmt.Errorf("verify reorder coverage: %w", err)
		}
		if leftover > 0 {
			return fmt.Errorf("reorder: order must cover all album photos (%d left unordered)", leftover)
		}
		return nil
	})
}

// inTx runs fn inside a transaction, committing on success and rolling back on
// error (the deferred rollback is a no-op after a successful commit).
func (r *AlbumPhotoRepository) inTx(ctx context.Context, label string, fn func(pgx.Tx) error) error {
	beginner, ok := r.dbtx.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("media/adapter: %s: executor does not support transactions", label)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", label, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit: %w", label, err)
	}
	return nil
}

// ListByAlbumOrdered returns the album's photos in display (position) order, or
// an empty slice when the album has no photos (or does not exist).
func (r *AlbumPhotoRepository) ListByAlbumOrdered(ctx context.Context, albumID domain.AlbumID) ([]*domain.Photo, error) {
	const q = `
		SELECT p.id, p.household_id, p.storage_ref, p.content_sha256, p.size_bytes, p.content_type,
		       p.taken_at, p.caption, p.uploaded_by, p.created_at
		  FROM album_photo ap
		  JOIN photo p ON p.id = ap.photo_id
		 WHERE ap.album_id = $1
		 ORDER BY ap.position`
	rows, err := r.dbtx.Query(ctx, q, albumID.String())
	if err != nil {
		return nil, fmt.Errorf("list album photos: %w", err)
	}
	defer rows.Close()
	return scanPhotos(rows)
}
