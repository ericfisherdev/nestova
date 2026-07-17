package domain

import "fmt"

// PhotoClass partitions the PhotoStore's key namespace so bytes uploaded for
// one purpose (e.g. a chore-completion proof photo) can never collide with,
// or be reinterpreted as, another purpose's bytes (e.g. a household album
// photo) — every PhotoStore.Put call takes a PhotoClass, and the resulting
// StorageRef is namespaced by it (see LocalPhotoStore's key layout).
//
// PhotoClass is owned by the calling context, not the store: PhotoService
// (this package's album/gallery use case) always passes PhotoClassAlbum. A
// future chore-proof upload path (NES-119, which introduces its own
// task_instance_photo table rather than reusing the photo table) will pass
// PhotoClassChoreProof. PhotoClassRewardImage only reserves the namespace —
// reward.image_ref (NES-126) is a plain optional text/emoji field today, not
// a stored photo, so no reward-image upload path exists yet.
type PhotoClass int

// PhotoClass values. Stored nowhere as text (nothing serializes a PhotoClass
// across a boundary today), so there is deliberately no ParsePhotoClass —
// only Valid, for the exhaustive-switch guard in the adapters that map a
// class to its storage key prefix.
const (
	// PhotoClassUnspecified is the zero value and is deliberately invalid:
	// a caller that forgets to choose a class must be rejected rather than
	// silently storing bytes in the album namespace.
	PhotoClassUnspecified PhotoClass = iota
	// PhotoClassAlbum is a household rotating-album photo.
	PhotoClassAlbum
	// PhotoClassChoreProof is a chore/task-completion proof photo.
	PhotoClassChoreProof
	// PhotoClassRewardImage reserves the key namespace for a future reward
	// image upload; see the type doc for why none exists yet.
	PhotoClassRewardImage
)

// Valid reports whether c is a known PhotoClass.
func (c PhotoClass) Valid() bool {
	switch c {
	case PhotoClassAlbum, PhotoClassChoreProof, PhotoClassRewardImage:
		return true
	default:
		return false
	}
}

// String returns a human-readable name for c, or "unknown photo class" for an
// invalid value — used in error messages, never persisted.
func (c PhotoClass) String() string {
	switch c {
	case PhotoClassAlbum:
		return "album"
	case PhotoClassChoreProof:
		return "chore_proof"
	case PhotoClassRewardImage:
		return "reward_image"
	default:
		return fmt.Sprintf("unknown photo class %d", int(c))
	}
}
