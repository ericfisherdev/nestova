package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// AlbumRepository is the pgx-backed domain.AlbumRepository. UUIDs are passed and
// scanned as text, matching the other adapters.
type AlbumRepository struct {
	dbtx db.TX
}

var _ domain.AlbumRepository = (*AlbumRepository)(nil)

// NewAlbumRepository constructs the repository with an injected query executor.
func NewAlbumRepository(dbtx db.TX) *AlbumRepository {
	if dbtx == nil {
		panic("media/adapter: NewAlbumRepository requires a non-nil db.TX")
	}
	return &AlbumRepository{dbtx: dbtx}
}

const albumColumns = `SELECT id, household_id, name, rotation_seconds, filter, created_at FROM album`

// Create inserts an album and populates its created_at, mapping an unknown
// household to household.ErrHouseholdNotFound.
func (r *AlbumRepository) Create(ctx context.Context, album *domain.Album) error {
	if album == nil {
		return errors.New("media/adapter: create album: nil album")
	}
	filter, err := json.Marshal(album.Filter)
	if err != nil {
		return fmt.Errorf("create album: marshal filter: %w", err)
	}
	const q = `
		INSERT INTO album (id, household_id, name, rotation_seconds, filter)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`
	err = r.dbtx.QueryRow(ctx, q,
		album.ID.String(), album.HouseholdID.String(), album.Name,
		album.Rotation.Seconds(), filter,
	).Scan(&album.CreatedAt)
	if err != nil {
		if mapped := mapFKViolation(err); mapped != nil {
			return mapped
		}
		return fmt.Errorf("create album: %w", err)
	}
	return nil
}

// Get returns the album, or domain.ErrAlbumNotFound.
func (r *AlbumRepository) Get(ctx context.Context, id domain.AlbumID) (*domain.Album, error) {
	album, err := scanAlbum(r.dbtx.QueryRow(ctx, albumColumns+` WHERE id = $1`, id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrAlbumNotFound
		}
		return nil, fmt.Errorf("get album: %w", err)
	}
	return album, nil
}

// Update rewrites the album's mutable fields (name, rotation, filter);
// household_id is immutable. It returns domain.ErrAlbumNotFound for an unknown id.
func (r *AlbumRepository) Update(ctx context.Context, album *domain.Album) error {
	if album == nil {
		return errors.New("media/adapter: update album: nil album")
	}
	filter, err := json.Marshal(album.Filter)
	if err != nil {
		return fmt.Errorf("update album: marshal filter: %w", err)
	}
	const q = `
		UPDATE album SET name = $2, rotation_seconds = $3, filter = $4
		 WHERE id = $1
		RETURNING id`
	var scanned string
	if err := r.dbtx.QueryRow(ctx, q, album.ID.String(), album.Name, album.Rotation.Seconds(), filter).Scan(&scanned); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAlbumNotFound
		}
		return fmt.Errorf("update album: %w", err)
	}
	return nil
}

// ListByHousehold returns the household's albums ordered by creation time then
// name, or an empty slice when none exist.
func (r *AlbumRepository) ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*domain.Album, error) {
	rows, err := r.dbtx.Query(ctx, albumColumns+` WHERE household_id = $1 ORDER BY created_at, name`, householdID.String())
	if err != nil {
		return nil, fmt.Errorf("list albums: %w", err)
	}
	defer rows.Close()

	albums := make([]*domain.Album, 0)
	for rows.Next() {
		album, err := scanAlbum(rows)
		if err != nil {
			return nil, fmt.Errorf("list albums: scan: %w", err)
		}
		albums = append(albums, album)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list albums: %w", err)
	}
	return albums, nil
}

// Delete removes the album (cascading its memberships), returning
// domain.ErrAlbumNotFound when the id is unknown.
func (r *AlbumRepository) Delete(ctx context.Context, id domain.AlbumID) error {
	tag, err := r.dbtx.Exec(ctx, `DELETE FROM album WHERE id = $1`, id.String())
	if err != nil {
		return fmt.Errorf("delete album: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrAlbumNotFound
	}
	return nil
}

func scanAlbum(r row) (*domain.Album, error) {
	var (
		album          domain.Album
		idStr, hhStr   string
		rotationSecond int
		filterJSON     []byte
	)
	if err := r.Scan(&idStr, &hhStr, &album.Name, &rotationSecond, &filterJSON, &album.CreatedAt); err != nil {
		return nil, err
	}
	id, err := domain.ParseAlbumID(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse album id: %w", err)
	}
	hh, err := household.ParseHouseholdID(hhStr)
	if err != nil {
		return nil, fmt.Errorf("parse household id: %w", err)
	}
	rotation, err := domain.NewRotationInterval(rotationSecond)
	if err != nil {
		return nil, fmt.Errorf("album rotation: %w", err)
	}
	if err := json.Unmarshal(filterJSON, &album.Filter); err != nil {
		return nil, fmt.Errorf("unmarshal album filter: %w", err)
	}
	album.ID = id
	album.HouseholdID = hh
	album.Rotation = rotation
	return &album, nil
}
