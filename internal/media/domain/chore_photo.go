package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// PhotoKind distinguishes a chore-proof photo taken before work started from
// one taken after it finished (NES-119).
type PhotoKind int

// PhotoKind values.
const (
	// PhotoKindUnspecified is the zero value and is deliberately invalid: a
	// caller that forgets to choose a kind must be rejected rather than
	// silently defaulting to one.
	PhotoKindUnspecified PhotoKind = iota
	PhotoKindBefore
	PhotoKindAfter
)

// Valid reports whether k is a known PhotoKind.
func (k PhotoKind) Valid() bool {
	switch k {
	case PhotoKindBefore, PhotoKindAfter:
		return true
	default:
		return false
	}
}

// String returns kind's storage/form-field spelling ("before"/"after"), or
// "unspecified" for the zero value — never persisted, used in error messages
// and ParsePhotoKind's error text.
func (k PhotoKind) String() string {
	switch k {
	case PhotoKindBefore:
		return "before"
	case PhotoKindAfter:
		return "after"
	default:
		return "unspecified"
	}
}

// ParsePhotoKind parses the "before"/"after" form-field spelling into a
// PhotoKind, returning ErrInvalidTaskInstancePhoto for anything else.
func ParsePhotoKind(s string) (PhotoKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "before":
		return PhotoKindBefore, nil
	case "after":
		return PhotoKindAfter, nil
	default:
		return PhotoKindUnspecified, fmt.Errorf("%w: kind must be \"before\" or \"after\", got %q", ErrInvalidTaskInstancePhoto, s)
	}
}

// TaskInstancePhoto errors.
var (
	// ErrTaskInstancePhotoNotFound is returned when a chore-proof photo does
	// not exist (or belongs to another household).
	ErrTaskInstancePhotoNotFound = errors.New("media: task instance photo not found")
	// ErrInvalidTaskInstancePhoto is returned by TaskInstancePhoto.Validate
	// and ParsePhotoKind for a malformed photo/kind.
	ErrInvalidTaskInstancePhoto = errors.New("media: invalid task instance photo")
	// ErrTaskInstanceNotFound is media's OWN view of "the task instance this
	// photo documents does not exist, or belongs to another household" —
	// mapped by the adapter from the task_instance_photo_instance_fk
	// violation. It is deliberately a separate sentinel from
	// tasks/domain.ErrInstanceNotFound (not that error re-exported): media
	// does not import the tasks bounded context (see TaskInstanceID's doc),
	// so it cannot return the tasks context's own sentinel — this is media's
	// own ubiquitous-language equivalent, scoped to what the media context
	// actually knows ("no task_instance row this household_id/id pair
	// resolves to").
	ErrTaskInstanceNotFound = errors.New("media: task instance not found")
	// ErrPhotoMissingTimestamp is returned by ChoreProofPhotoService.Upload
	// when the upload carries no usable EXIF capture timestamp — a
	// screenshot, a re-saved/re-compressed image, or any file whose EXIF was
	// stripped before it reached Nestova. The web layer maps it to a
	// friendly "please take a new photo" message (see the adapter's
	// handleMutationError).
	ErrPhotoMissingTimestamp = errors.New("media: photo has no usable EXIF capture timestamp")
	// ErrPhotoStale is returned by ChoreProofPhotoService.Upload when the
	// EXIF capture time is further from the upload instant (in either
	// direction — see the service's doc) than the configured freshness
	// window, e.g. a photo pulled from the camera roll days later, or one
	// whose camera clock is set far in the future.
	//
	// Threat model, stated explicitly: the EXIF metadata this whole gate
	// (ErrPhotoMissingTimestamp, this error, and ErrAfterPrecedesBefore)
	// relies on is UPLOADER-CONTROLLED data, not a cryptographic attestation
	// of when or how a photo was actually captured — a household member with
	// the right tools could hand-craft an EXIF DateTimeOriginal tag on
	// arbitrary bytes and defeat all three checks. That is a known,
	// deliberately accepted limitation, not an oversight: Nestova's threat
	// model here is a family household coordinating chores among people who
	// already trust each other, and the freshness gate exists to stop
	// CASUAL reuse — re-submitting yesterday's photo, or a sibling's old
	// photo, as if it were fresh — not to defeat a household member
	// motivated enough to forge EXIF bytes by hand. Cryptographic
	// proof-of-capture (e.g. a signed capture attestation from a trusted
	// camera app) is explicitly out of scope for this ticket and would be a
	// different, much heavier feature if ever needed.
	ErrPhotoStale = errors.New("media: photo capture time is outside the freshness window")
	// ErrAfterPrecedesBefore is returned by TaskInstancePhotoRepository.Create
	// when an "after" photo's EXIF capture time is earlier than a "before"
	// photo's for the same task instance — the work could not have been
	// photographed as finished before it was photographed as started. This
	// single invariant is enforced from BOTH insertion directions: inserting
	// an "after" checks it against the instance's latest existing "before",
	// and inserting a "before" checks it against the instance's earliest
	// existing "after" (see Create's doc for why both directions matter —
	// without the second, a "before" that lands chronologically after an
	// already-recorded "after" would slip through uncaught). Enforced
	// ATOMICALLY inside Create's own transaction (see the port's doc) rather
	// than as a separate check-then-insert from the caller, so a concurrent
	// write for the same instance can never race this decision.
	ErrAfterPrecedesBefore = errors.New("media: after photo was taken before the before photo")
)

// AfterPrecedesBefore reports whether afterTaken (an "after" chore-proof
// photo's capture time, existing or about to be inserted) is invalid because
// it precedes beforeTaken (a "before" photo's capture time, existing or
// about to be inserted, for the SAME task instance) — the one invariant
// ErrAfterPrecedesBefore protects. It is deliberately symmetric in its two
// time.Time parameters: TaskInstancePhotoRepository.Create calls it BOTH
// ways (afterTaken = the new row's TakenAt when inserting an "after", or =
// an existing row's TakenAt when inserting a "before" — see Create's doc for
// which extreme each direction reads) rather than defining two
// differently-named predicates for what is the same underlying rule.
// TaskInstancePhotoRepository.Create is what APPLIES this rule atomically
// (holding a per-instance lock across the read and the insert, inside one
// transaction — see its doc) so the decision stays consistent with whatever
// is genuinely committed at decision time, regardless of a concurrent write
// to the same instance.
func AfterPrecedesBefore(afterTaken, beforeTaken time.Time) bool {
	return afterTaken.Before(beforeTaken)
}

// TaskInstancePhoto is a before/after proof photo attached to a task
// instance (NES-119): a household member documents a chore's starting or
// finished state, and the EXIF capture time embedded in the photo itself
// (never a client-supplied timestamp) is what ChoreProofPhotoService.Upload
// validates against — a usable timestamp, inside the freshness window, and
// (for an "after" photo) not earlier than the instance's most recent
// "before" photo.
//
// task_instance_photo is a table deliberately SEPARATE from photo (the
// rotating-album table): see the 00029 migration's doc for why this is a
// structural, not just logical, separation — an album/gallery query can
// never surface a chore photo, and vice versa, because there is no shared
// table to filter correctly or incorrectly.
//
// TakenAt is never the zero time: Upload rejects an upload with no usable
// EXIF timestamp (ErrPhotoMissingTimestamp) before a TaskInstancePhoto value
// is ever constructed, unlike Photo.TakenAt, which is a nullable *time.Time
// because an album photo may legitimately carry none.
type TaskInstancePhoto struct {
	ID             TaskInstancePhotoID
	HouseholdID    household.HouseholdID
	TaskInstanceID TaskInstanceID
	Kind           PhotoKind
	StorageRef     StorageRef
	ContentHash    string
	SizeBytes      int64
	ContentType    string
	TakenAt        time.Time
	// UploadedBy is nilled (not deleted) if that member is removed so the
	// photo survives, mirroring Photo.UploadedBy.
	UploadedBy *household.MemberID
	UploadedAt time.Time
}

// Validate reports whether the photo is well-formed, wrapping
// ErrInvalidTaskInstancePhoto. Every field checked here is one
// ChoreProofPhotoService.Upload always sets before constructing a
// TaskInstancePhoto — a violation signals a bug in Upload or a PhotoStore
// implementation, not a legitimate edge case (unlike Photo.Validate, this
// table has no pre-existing legacy rows to tolerate a blank value for).
func (p TaskInstancePhoto) Validate() error {
	if strings.TrimSpace(p.StorageRef.String()) == "" {
		return fmt.Errorf("%w: storage ref must not be blank", ErrInvalidTaskInstancePhoto)
	}
	if !contentHashPattern.MatchString(p.ContentHash) {
		return fmt.Errorf("%w: content hash must be a 64-character lowercase hex sha256, got %q", ErrInvalidTaskInstancePhoto, p.ContentHash)
	}
	if p.SizeBytes <= 0 {
		return fmt.Errorf("%w: size bytes must be positive, got %d", ErrInvalidTaskInstancePhoto, p.SizeBytes)
	}
	if _, ok := acceptedContentTypes[p.ContentType]; !ok {
		return fmt.Errorf("%w: content type %q is not accepted", ErrInvalidTaskInstancePhoto, p.ContentType)
	}
	if !p.Kind.Valid() {
		return fmt.Errorf("%w: kind must be before or after", ErrInvalidTaskInstancePhoto)
	}
	if p.TakenAt.IsZero() {
		return fmt.Errorf("%w: taken_at must be set", ErrInvalidTaskInstancePhoto)
	}
	return nil
}

// TaskInstancePhotoRepository persists chore-proof photo metadata (not the
// bytes, which live behind the PhotoStore — the same port the album path
// uses, under domain.PhotoClassChoreProof).
//
// Object lifecycle invariant (mirrors PhotoRepository/PhotoService's own,
// documented on Photo's package): stored objects are immutable and
// content-addressed, and this repository never deletes or rolls one back
// behind the caller's back. A failure that happens AFTER
// ChoreProofPhotoService.Upload has already called PhotoStore.Put — a
// Create failure, a Validate failure, anything — leaves the just-stored
// object in place rather than synchronously deleting it: the object is
// content-addressed, so a concurrent upload of byte-identical bytes could
// already be relying on that exact ref, and synchronously deleting it out
// from under that concurrent upload would destroy bytes it still needs (the
// same race PhotoService.Delete's doc explains for the album path). An
// orphaned object left behind this way is a reaper candidate for the
// planned NES-132/133 storage verify/reaper, never this repository's (or
// the service's) job to clean up inline.
//
// Error contracts:
//   - Create returns ErrTaskInstanceNotFound when taskInstanceID is unknown
//     or belongs to another household, household.ErrMemberNotFound when
//     UploadedBy is set but unknown, mapped from the composite tenant FK
//     violations (task_instance_photo_instance_fk /
//     task_instance_photo_uploader_fk). This is the authoritative,
//     race-proof backstop for an unknown/foreign instance;
//     ChoreProofPhotoService.Upload also runs a best-effort InstanceExists
//     preflight before ever buffering/scrubbing/storing an upload so the
//     COMMON case fails fast without wasted work, but Create's own FK check
//     is what actually closes the TOCTOU gap if the instance is removed
//     between that preflight and this call.
//   - Create returns ErrAfterPrecedesBefore when photo would violate the
//     before/after ordering invariant (see ErrAfterPrecedesBefore's own doc
//     for the symmetric, both-directions definition): for photo.Kind ==
//     PhotoKindAfter, TakenAt earlier than the instance's LATEST existing
//     PhotoKindBefore photo; for photo.Kind == PhotoKindBefore, TakenAt
//     later than the instance's EARLIEST existing PhotoKindAfter photo. This
//     check and the insert happen ATOMICALLY, inside one transaction, under
//     a per-task-instance advisory lock that EVERY Create for that instance
//     acquires (regardless of kind) before touching it — see the adapter's
//     implementation doc for why a "before" insert must also take the lock,
//     not just an "after" one: without it, a concurrent write of the OTHER
//     kind landing in the gap between this Create's own read and its insert
//     could commit a stale-relative-to-reality decision, in either
//     direction. The lock serializes every Create for the SAME instance
//     against every other; Creates for DIFFERENT instances never contend.
//   - InstanceExists reports whether taskInstanceID exists within
//     householdID, with no side effects and no lock — a lightweight,
//     best-effort preflight (see Create's contract above for why it is not
//     the sole enforcement).
//   - LatestTakenAt returns ok=false (not an error) when the instance has no
//     photo of the given kind yet — the expected "nothing to compare
//     against" outcome for a first "before" (or a first "after" with no
//     prior "before"), not an exceptional one. It is a plain, unlocked read
//     — a convenience for a future read-only view (NES-120), not part of
//     Create's own atomicity, which re-reads this same fact itself inside
//     its transaction rather than trusting a value read outside it.
//   - ListByInstance returns an empty slice (not an error) when the instance
//     has no chore-proof photos.
type TaskInstancePhotoRepository interface {
	// Create persists a new chore-proof photo, populating UploadedAt, and
	// atomically enforces the before/after ordering rule from whichever
	// direction photo.Kind inserts — see the type doc's error contracts for
	// the full, symmetric atomicity argument.
	Create(ctx context.Context, photo *TaskInstancePhoto) error

	// InstanceExists reports whether taskInstanceID exists within
	// householdID. See the type doc: this is a preflight convenience, not
	// the authoritative existence check (Create's own FK violation is).
	InstanceExists(ctx context.Context, householdID household.HouseholdID, taskInstanceID TaskInstanceID) (bool, error)

	// LatestTakenAt returns the most recent TakenAt among the instance's
	// photos of the given kind (household-scoped), and ok=true, or ok=false
	// when none exist. See the type doc: this is a read-only convenience,
	// not part of Create's own atomic ordering check.
	LatestTakenAt(ctx context.Context, householdID household.HouseholdID, taskInstanceID TaskInstanceID, kind PhotoKind) (takenAt time.Time, ok bool, err error)

	// ListByInstance returns every chore-proof photo for the instance,
	// ordered by taken_at ascending, for a future detail view (NES-120).
	// Returns an empty slice when none exist.
	ListByInstance(ctx context.Context, householdID household.HouseholdID, taskInstanceID TaskInstanceID) ([]*TaskInstancePhoto, error)
}

// ChoreProofExif extracts the EXIF facts the chore-proof upload path needs
// beyond ExifReader.TakenAt, and scrubs EXIF from an upload before its bytes
// ever reach PhotoStore.Put. It is deliberately a SEPARATE interface from
// ExifReader (ISP): the album upload path (PhotoService) never needs
// orientation or scrubbing, only capture time from a streamed
// RandomAccessReader — ChoreProofExif instead operates on an
// already-fully-buffered []byte, because scrubbing must happen BEFORE
// storage (see ChoreProofPhotoService.Upload's doc for why buffering the
// whole upload, capped at the configured size limit, is the deliberate
// tradeoff here rather than PhotoStore's usual never-buffer-the-whole-upload
// streaming discipline).
type ChoreProofExif interface {
	// TakenAtAndOrientation returns the EXIF capture time (UTC, nil when no
	// usable tag is present) and the Orientation tag (1-8; 0 when absent or
	// unreadable) from a JPEG's already-buffered bytes.
	//
	// The capture time comes from the DateTimeOriginal tag ONLY — the
	// generic DateTime (file-modified) and DateTimeDigitized tags are
	// deliberately NOT accepted as fallbacks here, unlike a generic EXIF
	// reader: DateTimeOriginal is the one tag a camera sets at the instant
	// the shutter fires and that editing/scanning software cannot forge
	// without deliberately copying it forward, so it is the only tag that
	// actually proves "this photo was taken just now" — the freshness gate
	// (ChoreProofPhotoService.Upload) depends on that provenance guarantee.
	// A re-saved, edited, or scanned image that carries a plain DateTime tag
	// but no DateTimeOriginal is treated the same as an image with no EXIF
	// at all (ErrPhotoMissingTimestamp).
	//
	// See the adapter's doc for why no EXIF OffsetTimeOriginal handling is
	// attempted and what timezone a naive (offset-less) EXIF timestamp is
	// interpreted in — including the single-household-appliance deployment
	// assumption that interpretation currently relies on.
	TakenAtAndOrientation(data []byte) (takenAt *time.Time, orientation int)

	// Scrub returns data with every EXIF tag removed — not just GPS, see the
	// adapter's doc for why a whole-segment strip is the deliberate,
	// pragmatic choice — baking orientation into the pixel data first via a
	// full JPEG re-encode when orientation is anything other than 1 or 0
	// (already upright, or unknown) so a stripped photo never displays
	// sideways. data must already be a well-formed JPEG; non-JPEG bytes (or
	// bytes with no SOI marker) are returned unchanged, nil error.
	Scrub(data []byte, orientation int) ([]byte, error)
}
