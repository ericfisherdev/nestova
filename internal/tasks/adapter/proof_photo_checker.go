package adapter

import (
	"context"
	"fmt"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ProofPhotoChecker adapts media's own domain.TaskInstancePhotoRepository
// (NES-119) to tasks/domain.ProofPhotoChecker's minimal, tasks-owned port
// (NES-120). It is the one place tasks/adapter depends on media/domain —
// deliberately: see [domain.ProofPhotoChecker]'s doc for why this mirrors
// the precedent TaskService already sets with notify/domain, rather than
// crossing the narrower tasks/adapter-vs-media/adapter web-routing boundary
// ChoreProofWebHandlers' doc describes.
type ProofPhotoChecker struct {
	photos mediadomain.TaskInstancePhotoRepository
}

// Compile-time assurance that ProofPhotoChecker satisfies the port.
var _ domain.ProofPhotoChecker = (*ProofPhotoChecker)(nil)

// NewProofPhotoChecker constructs a ProofPhotoChecker with an injected
// media.TaskInstancePhotoRepository. Panics on a nil dependency, matching
// this package's WebHandlers convention of catching a misconfigured
// composition root at startup.
func NewProofPhotoChecker(photos mediadomain.TaskInstancePhotoRepository) *ProofPhotoChecker {
	if photos == nil {
		panic("tasks/adapter: NewProofPhotoChecker requires a non-nil media TaskInstancePhotoRepository")
	}
	return &ProofPhotoChecker{photos: photos}
}

// ProofPhotos reports instanceID's most recent "before"/"after" chore-proof
// photo ids within householdID, or empty strings for a kind with no photo
// yet. It lists every photo for the instance (ordered by taken_at ascending
// — see TaskInstancePhotoRepository.ListByInstance's doc) rather than
// issuing two separate LatestTakenAt calls, both because one query is
// cheaper than two and because the caller needs the photo's id (to build a
// raw-bytes URL for the review UI), which LatestTakenAt does not return.
// Iterating in taken_at-ascending order means the last match of each kind
// overwrites any earlier one, so beforeID/afterID end up holding the MOST
// RECENT photo of each kind, matching LatestTakenAt's own "most recent"
// semantics exactly.
func (c *ProofPhotoChecker) ProofPhotos(
	ctx context.Context,
	householdID household.HouseholdID,
	instanceID domain.TaskInstanceID,
) (beforeID, afterID string, err error) {
	// media.TaskInstanceID and tasks.TaskInstanceID are structurally
	// identical but deliberately separate types — media does not import
	// tasks (see media/domain.TaskInstanceID's own doc) — so converting
	// between them always goes through the canonical string form, never an
	// unsafe cast.
	mediaInstanceID, err := mediadomain.ParseTaskInstanceID(instanceID.String())
	if err != nil {
		return "", "", fmt.Errorf("tasks/adapter: proof photos: parse instance id: %w", err)
	}
	photos, err := c.photos.ListByInstance(ctx, householdID, mediaInstanceID)
	if err != nil {
		return "", "", fmt.Errorf("tasks/adapter: proof photos: list by instance: %w", err)
	}
	for _, p := range photos {
		switch p.Kind {
		case mediadomain.PhotoKindBefore:
			beforeID = p.ID.String()
		case mediadomain.PhotoKindAfter:
			afterID = p.ID.String()
		}
	}
	return beforeID, afterID, nil
}

// ProofPhotosByInstances is ProofPhotos' batch counterpart (NES-120): one
// media.TaskInstancePhotoRepository.ListByInstances call for every id in
// instanceIDs, instead of one ListByInstance call per id — the N+1
// avoidance the /tasks list builder needs (see the port doc). Unlike
// ListByInstance's own taken_at-ascending guarantee, ListByInstances'
// ordering is unspecified (it is a flat, multi-instance result set), so
// "most recent per kind" is derived explicitly here by comparing TakenAt,
// not by trusting row order.
func (c *ProofPhotoChecker) ProofPhotosByInstances(
	ctx context.Context,
	householdID household.HouseholdID,
	instanceIDs []domain.TaskInstanceID,
) (map[domain.TaskInstanceID]domain.ProofPhotoIDs, error) {
	result := make(map[domain.TaskInstanceID]domain.ProofPhotoIDs, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return result, nil
	}

	mediaIDs := make([]mediadomain.TaskInstanceID, len(instanceIDs))
	// mediaToTasks reverses the media.TaskInstanceID → tasks.TaskInstanceID
	// mapping so the flat result set below can be regrouped by the CALLER's
	// own id type, which media's TaskInstancePhoto never carries.
	mediaToTasks := make(map[mediadomain.TaskInstanceID]domain.TaskInstanceID, len(instanceIDs))
	for i, id := range instanceIDs {
		mediaID, err := mediadomain.ParseTaskInstanceID(id.String())
		if err != nil {
			return nil, fmt.Errorf("tasks/adapter: proof photos by instances: parse instance id: %w", err)
		}
		mediaIDs[i] = mediaID
		mediaToTasks[mediaID] = id
	}

	photos, err := c.photos.ListByInstances(ctx, householdID, mediaIDs)
	if err != nil {
		return nil, fmt.Errorf("tasks/adapter: proof photos by instances: list by instances: %w", err)
	}

	// latest tracks the most recent TakenAt seen so far per (instance, kind)
	// pair, so a later row for the same pair can be compared against it
	// before overwriting result — required because ListByInstances makes no
	// per-instance ordering guarantee (unlike ProofPhotos' single-instance
	// ListByInstance call, which relies on ORDER BY taken_at).
	type key struct {
		instance domain.TaskInstanceID
		kind     mediadomain.PhotoKind
	}
	type latestPhoto struct {
		takenAt time.Time
		id      string
	}
	latest := make(map[key]latestPhoto, len(photos))
	for _, p := range photos {
		instanceID, ok := mediaToTasks[p.TaskInstanceID]
		if !ok {
			// A photo for an instance this caller never asked about — should
			// not happen given the `= ANY($2)` filter, but tolerated rather
			// than panicking on an adversarial/legacy row.
			continue
		}
		k := key{instance: instanceID, kind: p.Kind}
		if existing, ok := latest[k]; !ok || p.TakenAt.After(existing.takenAt) {
			latest[k] = latestPhoto{takenAt: p.TakenAt, id: p.ID.String()}
		}
	}

	for k, v := range latest {
		ids := result[k.instance]
		switch k.kind {
		case mediadomain.PhotoKindBefore:
			ids.BeforeID = v.id
		case mediadomain.PhotoKindAfter:
			ids.AfterID = v.id
		}
		result[k.instance] = ids
	}
	return result, nil
}
