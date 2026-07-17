package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// jpegSOIPrefix is the 2-byte JPEG Start-Of-Image marker every real JPEG
// begins with — sniffed here (not via http.DetectContentType, which
// classifies far more than JPEG) purely to decide whether EXIF
// extraction/scrubbing applies at all: only a JPEG can carry the EXIF this
// service validates against, so a non-JPEG upload skips straight to
// PhotoStore.Put, which independently sniffs and validates the true content
// type from the bytes themselves and rejects anything unsupported.
var jpegSOIPrefix = []byte{0xFF, 0xD8}

// ChoreProofPhotoService handles uploading a before/after chore-proof photo
// (NES-119): it runs a best-effort household-scoped preflight to confirm
// the task instance exists, buffers the upload (bounded by maxUploadBytes),
// extracts and validates its EXIF capture time, scrubs EXIF (including any
// GPS tags — see ChoreProofExif.Scrub's doc) from a JPEG upload before the
// bytes ever reach storage, streams the result behind the PhotoStore under
// domain.PhotoClassChoreProof, and persists the photo metadata — the
// before/after ordering rule (domain.ErrAfterPrecedesBefore) is enforced
// ATOMICALLY by TaskInstancePhotoRepository.Create itself, not by this
// service; see that port's doc for the full argument.
//
// Buffering the whole upload here (rather than PhotoStore.Put's usual
// never-buffer-the-whole-upload streaming discipline) is a deliberate,
// documented tradeoff: EXIF (and any GPS coordinates it carries) must be
// captured and then scrubbed BEFORE bytes are ever written to disk — the
// whole point of scrubbing is that an original, GPS-bearing copy must never
// exist on disk even momentarily, so there is no way to reuse PhotoStore.Put
// unmodified for the raw upload first and clean up after. A chore-proof
// upload is a single photo capped at maxUploadBytes (the same operator
// limit LocalPhotoStore enforces), not a bulk album batch of dozens of
// files, so this bounded, one-shot buffering is an acceptable cost — unlike
// the albums' bulk-upload path (NES-124), which streams specifically to
// avoid holding many large files in memory at once.
//
// Object lifecycle invariant (mirrors PhotoService.Upload's own, and
// TaskInstancePhotoRepository's — see that port's doc): a failure AFTER
// PhotoStore.Put has already stored bytes — a Validate failure, a Create
// failure, anything — is never rolled back by deleting the just-stored
// object. It is content-addressed and may already be relied on by a
// concurrent upload of identical bytes; synchronously deleting it here
// could destroy bytes that upload still needs. An orphaned object left
// behind this way is a reaper candidate for the planned NES-132/133 storage
// verify/reaper, not something this service cleans up inline. The
// InstanceExists preflight below exists specifically so the COMMON failure
// case (an unknown or cross-household task instance) is caught BEFORE any
// bytes are ever buffered, scrubbed, or stored — not as a substitute for
// this invariant, which still governs every failure that preflight cannot
// catch (e.g. the instance is removed in the narrow window between the
// preflight and Create, a race Create's own FK violation is the true
// backstop for).
type ChoreProofPhotoService struct {
	store           domain.PhotoStore
	exif            domain.ChoreProofExif
	photos          domain.TaskInstancePhotoRepository
	maxUploadBytes  int64
	freshnessWindow time.Duration
}

// NewChoreProofPhotoService constructs the service with injected
// dependencies, panicking-free — it returns an error instead — on a nil
// dependency or a non-positive maxUploadBytes/freshnessWindow.
//
// maxUploadBytes must also leave room for readBounded's own +1
// overflow-detection byte (see its doc): a maxUploadBytes within 1 of
// math.MaxInt64 would wrap readBounded's `maxBytes+1` computation negative,
// corrupting the size-cap check it exists to perform. No real deployment
// configures anything remotely close to that (MEDIA_MAX_UPLOAD_BYTES
// defaults to 25 MiB), so this only ever rejects a pathological
// misconfiguration, not a legitimate large limit.
func NewChoreProofPhotoService(
	store domain.PhotoStore,
	exif domain.ChoreProofExif,
	photos domain.TaskInstancePhotoRepository,
	maxUploadBytes int64,
	freshnessWindow time.Duration,
) (*ChoreProofPhotoService, error) {
	switch {
	case store == nil:
		return nil, errors.New("media/app: NewChoreProofPhotoService requires a non-nil PhotoStore")
	case exif == nil:
		return nil, errors.New("media/app: NewChoreProofPhotoService requires a non-nil ChoreProofExif")
	case photos == nil:
		return nil, errors.New("media/app: NewChoreProofPhotoService requires a non-nil TaskInstancePhotoRepository")
	case maxUploadBytes <= 0:
		return nil, fmt.Errorf("media/app: NewChoreProofPhotoService requires a positive maxUploadBytes, got %d", maxUploadBytes)
	case maxUploadBytes > math.MaxInt64-1:
		return nil, fmt.Errorf("media/app: NewChoreProofPhotoService requires maxUploadBytes to leave room for the +1 overflow-detection byte, got %d", maxUploadBytes)
	case freshnessWindow <= 0:
		return nil, fmt.Errorf("media/app: NewChoreProofPhotoService requires a positive freshnessWindow, got %v", freshnessWindow)
	}
	return &ChoreProofPhotoService{
		store: store, exif: exif, photos: photos,
		maxUploadBytes: maxUploadBytes, freshnessWindow: freshnessWindow,
	}, nil
}

// Upload runs a household-scoped preflight to confirm taskInstanceID exists
// (see the type doc's object lifecycle invariant for why this comes before
// anything else), buffers r (bounded by maxUploadBytes), validates kind,
// extracts and validates the EXIF capture time, scrubs EXIF from a JPEG
// upload, stores the result under domain.PhotoClassChoreProof, and persists
// the photo attributed to uploaderID. now is the upload instant the
// freshness window and EXIF capture time are compared against
// (caller-supplied, mirroring TaskInstanceRepository.Complete's own `at
// time.Time` parameter, so tests can pin it).
//
// The freshness window comparison below relies on the same
// server-local-timezone-is-household-timezone deployment assumption
// ChoreProofExif.TakenAtAndOrientation's doc explains in detail (both now
// and the EXIF capture time it's compared against are interpreted in that
// same assumed timezone) — see that doc for what breaks the assumption and
// what the fix would be.
//
// Validation order (each returns its own sentinel, wrapped where noted):
//   - taskInstanceID must exist within householdID, else
//     domain.ErrTaskInstanceNotFound (preflight; see the type doc — this is
//     a best-effort fast-fail, not the authoritative check, which is
//     TaskInstancePhotoRepository.Create's own FK violation).
//   - kind must be domain.PhotoKindBefore or domain.PhotoKindAfter, else
//     domain.ErrInvalidTaskInstancePhoto.
//   - the upload must not exceed maxUploadBytes, else domain.ErrPhotoTooLarge.
//   - the upload must carry a usable EXIF DateTimeOriginal capture time (see
//     ChoreProofExif.TakenAtAndOrientation — DateTime/DateTimeDigitized are
//     deliberately NOT accepted fallbacks for a chore-proof photo), else
//     domain.ErrPhotoMissingTimestamp. A non-JPEG upload never has one
//     extracted (see jpegSOIPrefix's doc) and always fails this check.
//   - the capture time must be within freshnessWindow of now, in EITHER
//     direction — a stale cached photo and a camera with a badly-set clock
//     are both rejected the same way — else domain.ErrPhotoStale.
//   - PhotoStore.Put's own validation errors (ErrUnsupportedMediaType,
//     ErrInvalidPhoto) propagate unchanged.
//   - for kind == domain.PhotoKindAfter, domain.ErrAfterPrecedesBefore when
//     the capture time is earlier than the instance's most recent
//     domain.PhotoKindBefore photo — enforced ATOMICALLY by
//     TaskInstancePhotoRepository.Create itself (see its doc), not checked
//     here, so it is evaluated AFTER Put succeeds, at Create time.
func (s *ChoreProofPhotoService) Upload(
	ctx context.Context,
	householdID household.HouseholdID,
	uploaderID household.MemberID,
	taskInstanceID domain.TaskInstanceID,
	kind domain.PhotoKind,
	r io.Reader,
	now time.Time,
) (*domain.TaskInstancePhoto, error) {
	exists, err := s.photos.InstanceExists(ctx, householdID, taskInstanceID)
	if err != nil {
		return nil, fmt.Errorf("media/app: check task instance: %w", err)
	}
	if !exists {
		return nil, domain.ErrTaskInstanceNotFound
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: kind must be before or after", domain.ErrInvalidTaskInstancePhoto)
	}

	raw, err := readBounded(r, s.maxUploadBytes)
	if err != nil {
		return nil, err
	}

	finalBytes, taken, err := s.captureAndScrub(raw)
	if err != nil {
		return nil, err
	}
	if taken == nil {
		return nil, domain.ErrPhotoMissingTimestamp
	}
	if delta := now.Sub(*taken); delta > s.freshnessWindow || delta < -s.freshnessWindow {
		return nil, domain.ErrPhotoStale
	}

	stored, err := s.store.Put(ctx, householdID, domain.PhotoClassChoreProof, bytes.NewReader(finalBytes))
	if err != nil {
		return nil, err
	}

	uploader := uploaderID
	photo := &domain.TaskInstancePhoto{
		ID:             domain.NewTaskInstancePhotoID(),
		HouseholdID:    householdID,
		TaskInstanceID: taskInstanceID,
		Kind:           kind,
		StorageRef:     stored.Ref,
		ContentHash:    stored.ContentHash,
		SizeBytes:      stored.SizeBytes,
		ContentType:    stored.ContentType,
		TakenAt:        *taken,
		UploadedBy:     &uploader,
	}
	if err := photo.Validate(); err != nil {
		return nil, err
	}
	if err := s.photos.Create(ctx, photo); err != nil {
		return nil, err
	}
	return photo, nil
}

// captureAndScrub extracts the EXIF capture time from raw and, for a JPEG
// upload, returns the EXIF-scrubbed bytes to store instead of raw (see the
// type doc for why scrubbing must happen before PhotoStore.Put ever sees the
// bytes). A non-JPEG upload is returned unchanged with a nil capture time —
// it will fail the caller's ErrPhotoMissingTimestamp check, since no
// non-JPEG upload can carry an extractable EXIF timestamp today (see
// jpegSOIPrefix's doc).
func (s *ChoreProofPhotoService) captureAndScrub(raw []byte) (finalBytes []byte, taken *time.Time, err error) {
	if !bytes.HasPrefix(raw, jpegSOIPrefix) {
		return raw, nil, nil
	}
	var orientation int
	taken, orientation = s.exif.TakenAtAndOrientation(raw)
	scrubbed, err := s.exif.Scrub(raw, orientation)
	if err != nil {
		return nil, nil, fmt.Errorf("media/app: scrub exif: %w", err)
	}
	return scrubbed, taken, nil
}

// readBounded reads r fully, rejecting anything beyond maxBytes with
// domain.ErrPhotoTooLarge — mirrors LocalPhotoStore.Put's own size-cap
// convention (read up to maxBytes+1 so the cap is detected without ever
// buffering more than one byte past it). maxBytes+1 would overflow for a
// maxBytes within 1 of math.MaxInt64, but NewChoreProofPhotoService already
// rejects that at construction time, so every maxBytes reaching here is
// safe to add 1 to.
func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(r, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("media/app: read upload: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: exceeds the %d-byte limit", domain.ErrPhotoTooLarge, maxBytes)
	}
	return data, nil
}
