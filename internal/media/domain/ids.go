package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// AlbumID uniquely identifies a photo album.
type AlbumID uuid.UUID

// NewAlbumID returns a new time-ordered (UUIDv7) album id, which gives better
// B-tree index locality than random v4 ids. uuid.NewV7 only errors if the crypto
// random source is unavailable, the same failure under which uuid.New panics, so
// Must is appropriate here.
func NewAlbumID() AlbumID { return AlbumID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id AlbumID) String() string { return uuid.UUID(id).String() }

// ParseAlbumID parses a canonical UUID string into an AlbumID.
func ParseAlbumID(s string) (AlbumID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return AlbumID{}, fmt.Errorf("parse album id: %w", err)
	}
	return AlbumID(u), nil
}

// PhotoID uniquely identifies a photo.
type PhotoID uuid.UUID

// NewPhotoID returns a new time-ordered (UUIDv7) photo id.
func NewPhotoID() PhotoID { return PhotoID(uuid.Must(uuid.NewV7())) }

// String returns the canonical UUID string.
func (id PhotoID) String() string { return uuid.UUID(id).String() }

// ParsePhotoID parses a canonical UUID string into a PhotoID.
func ParsePhotoID(s string) (PhotoID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return PhotoID{}, fmt.Errorf("parse photo id: %w", err)
	}
	return PhotoID(u), nil
}

// TaskInstancePhotoID uniquely identifies a chore-proof photo (NES-119).
type TaskInstancePhotoID uuid.UUID

// NewTaskInstancePhotoID returns a new time-ordered (UUIDv7) id.
func NewTaskInstancePhotoID() TaskInstancePhotoID {
	return TaskInstancePhotoID(uuid.Must(uuid.NewV7()))
}

// String returns the canonical UUID string.
func (id TaskInstancePhotoID) String() string { return uuid.UUID(id).String() }

// ParseTaskInstancePhotoID parses a canonical UUID string into a TaskInstancePhotoID.
func ParseTaskInstancePhotoID(s string) (TaskInstancePhotoID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return TaskInstancePhotoID{}, fmt.Errorf("parse task instance photo id: %w", err)
	}
	return TaskInstancePhotoID(u), nil
}

// TaskInstanceID identifies the task instance a chore-proof photo documents
// (NES-119). It is media's OWN reference type — a raw UUID, structurally
// identical to (and always constructed from the same string as) the tasks
// bounded context's own tasks/domain.TaskInstanceID — not an import of it:
// media does not depend on the tasks context (see the import-graph note on
// ChoreProofExif in chore_photo.go), so a chore-proof photo references "the
// task instance with this id" as a plain value, the same way photo.go
// already references "the household with this id" would if media did not
// already share household as the one, deliberately foundational, shared
// kernel every bounded context depends on. Parsing an invalid/foreign id is
// impossible to distinguish here from a valid-but-nonexistent one; both are
// caught downstream by the task_instance_photo_instance_fk composite FK,
// mapped by the adapter to ErrTaskInstanceNotFound.
type TaskInstanceID uuid.UUID

// String returns the canonical UUID string.
func (id TaskInstanceID) String() string { return uuid.UUID(id).String() }

// ParseTaskInstanceID parses a canonical UUID string into a TaskInstanceID.
func ParseTaskInstanceID(s string) (TaskInstanceID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return TaskInstanceID{}, fmt.Errorf("parse task instance id: %w", err)
	}
	return TaskInstanceID(u), nil
}
