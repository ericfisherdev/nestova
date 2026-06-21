package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// Album errors.
var (
	// ErrAlbumNotFound is returned when an album does not exist (or belongs to
	// another household).
	ErrAlbumNotFound = errors.New("media: album not found")
	// ErrInvalidAlbum is returned by Album.Validate for a malformed album.
	ErrInvalidAlbum = errors.New("media: invalid album")
)

// RotationInterval is the time each photo is shown before the album advances. It
// is a value object so the positive-seconds invariant lives in one place;
// construct it with NewRotationInterval, which rejects non-positive durations.
type RotationInterval struct {
	seconds int
}

// NewRotationInterval returns a RotationInterval of the given seconds, or
// ErrInvalidAlbum when seconds is not strictly positive.
func NewRotationInterval(seconds int) (RotationInterval, error) {
	if seconds <= 0 {
		return RotationInterval{}, fmt.Errorf("%w: rotation seconds must be positive, got %d", ErrInvalidAlbum, seconds)
	}
	return RotationInterval{seconds: seconds}, nil
}

// Seconds returns the interval in whole seconds.
func (r RotationInterval) Seconds() int { return r.seconds }

// Duration returns the interval as a time.Duration.
func (r RotationInterval) Duration() time.Duration { return time.Duration(r.seconds) * time.Second }

// AlbumFilter narrows which of a household's photos an album shows. A zero-value
// filter (no member ids, no bounds) matches every household photo. It serializes
// to the album.filter jsonb column.
//   - MemberIDs: if non-empty, only photos uploaded by one of these members.
//   - Since/Until: if set, only photos whose EXIF TakenAt falls within the
//     inclusive bound; a photo with no TakenAt is excluded when either bound is set.
type AlbumFilter struct {
	MemberIDs []household.MemberID
	Since     *time.Time
	Until     *time.Time
}

// albumFilterJSON is the on-disk jsonb shape: member ids are stored as canonical
// UUID strings (the MemberID newtype does not marshal as a string on its own).
type albumFilterJSON struct {
	MemberIDs []string   `json:"member_ids,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
}

// MarshalJSON encodes the filter to its jsonb shape.
func (f AlbumFilter) MarshalJSON() ([]byte, error) {
	dto := albumFilterJSON{Since: f.Since, Until: f.Until}
	for _, id := range f.MemberIDs {
		dto.MemberIDs = append(dto.MemberIDs, id.String())
	}
	return json.Marshal(dto)
}

// UnmarshalJSON decodes the filter from its jsonb shape, rejecting malformed
// member ids.
func (f *AlbumFilter) UnmarshalJSON(b []byte) error {
	var dto albumFilterJSON
	if err := json.Unmarshal(b, &dto); err != nil {
		return err
	}
	out := AlbumFilter{Since: dto.Since, Until: dto.Until}
	for _, s := range dto.MemberIDs {
		id, err := household.ParseMemberID(s)
		if err != nil {
			return fmt.Errorf("album filter: %w", err)
		}
		out.MemberIDs = append(out.MemberIDs, id)
	}
	*f = out
	return nil
}

// Matches reports whether the photo passes the filter. It is the single source
// of truth for album membership selection, used when composing the playlist.
func (f AlbumFilter) Matches(p Photo) bool {
	if len(f.MemberIDs) > 0 {
		if p.UploadedBy == nil || !containsMember(f.MemberIDs, *p.UploadedBy) {
			return false
		}
	}
	if f.Since != nil || f.Until != nil {
		if p.TakenAt == nil {
			return false
		}
		if f.Since != nil && p.TakenAt.Before(*f.Since) {
			return false
		}
		if f.Until != nil && p.TakenAt.After(*f.Until) {
			return false
		}
	}
	return true
}

func containsMember(ids []household.MemberID, id household.MemberID) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// Album is a household-scoped slideshow: an ordered set of the household's
// photos (the order lives in album_photo), shown one at a time for Rotation,
// narrowed by Filter.
type Album struct {
	ID          AlbumID
	HouseholdID household.HouseholdID
	Name        string
	Rotation    RotationInterval
	Filter      AlbumFilter
	CreatedAt   time.Time
}

// Validate reports whether the album is well-formed, wrapping ErrInvalidAlbum.
func (a Album) Validate() error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("%w: name must not be blank", ErrInvalidAlbum)
	}
	// A zero-value RotationInterval (seconds == 0) is invalid; NewRotationInterval
	// is the only way to a positive one, so guard direct struct construction.
	if a.Rotation.Seconds() <= 0 {
		return fmt.Errorf("%w: rotation interval must be positive", ErrInvalidAlbum)
	}
	return nil
}

// AlbumRepository persists albums. Get and Update return ErrAlbumNotFound for an
// unknown id; a Create or Update with an unknown HouseholdID returns
// household.ErrHouseholdNotFound (mapped from the tenant FK violation by the
// adapter). ListByHousehold returns an empty slice (not an error) when none match.
type AlbumRepository interface {
	Create(ctx context.Context, album *Album) error
	Get(ctx context.Context, id AlbumID) (*Album, error)
	Update(ctx context.Context, album *Album) error
	ListByHousehold(ctx context.Context, householdID household.HouseholdID) ([]*Album, error)
	Delete(ctx context.Context, id AlbumID) error
}

// AlbumPhotoRepository manages the ordered membership of photos in an album.
// Implementations populate album_photo.household_id from the album so the
// composite tenant FKs reject linking a photo from another household; the
// service still verifies ownership up front for a clean domain error.
//   - Add appends the photo at the next position; adding a photo already in the
//     album is a no-op.
//   - Remove drops the photo from the album (closing the position gap is not
//     required; ListByAlbumOrdered orders by position regardless).
//   - Reorder sets the album's complete order from the given photo ids,
//     atomically; ids must be exactly the album's current membership.
//   - ListByAlbumOrdered returns the album's photos in display (position) order.
type AlbumPhotoRepository interface {
	Add(ctx context.Context, albumID AlbumID, photoID PhotoID) error
	Remove(ctx context.Context, albumID AlbumID, photoID PhotoID) error
	Reorder(ctx context.Context, albumID AlbumID, order []PhotoID) error
	ListByAlbumOrdered(ctx context.Context, albumID AlbumID) ([]*Photo, error)
}
