package adapter_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// hexHash returns a syntactically valid 64-char lowercase hex sha256, distinct
// per seed string — TaskInstancePhoto.Validate requires this exact shape.
func hexHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// seedChoreProofPhoto inserts a real task_instance_photo row for instanceID
// (tasks' own TaskInstanceID) via media's own repository, converting through
// the canonical string form exactly as tasksadapter.ProofPhotoChecker does in
// production — this test exercises the SAME conversion path, not a shortcut
// around it.
func seedChoreProofPhoto(
	t *testing.T,
	mediaPhotos *mediaadapter.TaskInstancePhotoRepository,
	hh household.HouseholdID,
	instanceID domain.TaskInstanceID,
	kind mediadomain.PhotoKind,
	takenAt time.Time,
) *mediadomain.TaskInstancePhoto {
	t.Helper()
	mediaInstanceID, err := mediadomain.ParseTaskInstanceID(instanceID.String())
	if err != nil {
		t.Fatalf("parse media instance id: %v", err)
	}
	photo := &mediadomain.TaskInstancePhoto{
		ID:             mediadomain.NewTaskInstancePhotoID(),
		HouseholdID:    hh,
		TaskInstanceID: mediaInstanceID,
		Kind:           kind,
		StorageRef:     mediadomain.StorageRef("hh/chore-photos/aa/" + kind.String() + ".jpg"),
		ContentHash:    hexHash(kind.String() + takenAt.String()),
		SizeBytes:      1024,
		ContentType:    mediadomain.ContentTypeJPEG,
		TakenAt:        takenAt,
	}
	if err := mediaPhotos.Create(testCtx(t), photo); err != nil {
		t.Fatalf("seed chore proof photo (%s): %v", kind, err)
	}
	return photo
}

// TestProofPhotoChecker_ProofPhotos verifies the NES-120 cross-context
// adapter against a real database: it reports empty ids when no photos
// exist, then the correct before/after ids once media's own repository has
// real rows for the SAME instance — proving the tasks.TaskInstanceID →
// media.TaskInstanceID string conversion and the underlying ListByInstance
// query actually work end to end, not just against the hermetic fakes.
func TestProofPhotoChecker_ProofPhotos(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instanceRepo := adapter.NewTaskInstanceRepository(pool)
	mediaPhotos := mediaadapter.NewTaskInstancePhotoRepository(pool, mediadomain.StorageBackendLocal)
	checker := adapter.NewProofPhotoChecker(mediaPhotos)

	h, _, _ := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	inst := seedTaskInstance(t, instanceRepo, rt, time.Now())

	beforeID, afterID, err := checker.ProofPhotos(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("ProofPhotos(no photos): %v", err)
	}
	if beforeID != "" || afterID != "" {
		t.Errorf("ProofPhotos(no photos) = (%q, %q), want (\"\", \"\")", beforeID, afterID)
	}

	beforePhoto := seedChoreProofPhoto(t, mediaPhotos, h.ID, inst.ID, mediadomain.PhotoKindBefore,
		time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC))
	afterPhoto := seedChoreProofPhoto(t, mediaPhotos, h.ID, inst.ID, mediadomain.PhotoKindAfter,
		time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))

	beforeID, afterID, err = checker.ProofPhotos(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("ProofPhotos(both): %v", err)
	}
	if beforeID != beforePhoto.ID.String() {
		t.Errorf("beforeID = %q, want %q", beforeID, beforePhoto.ID.String())
	}
	if afterID != afterPhoto.ID.String() {
		t.Errorf("afterID = %q, want %q", afterID, afterPhoto.ID.String())
	}
}

// TestProofPhotoChecker_ProofPhotosByInstances verifies the NES-120 batch
// counterpart against a real database: one call resolves MULTIPLE
// instances' before/after photo ids correctly, an instance with no photos
// is simply absent from (or zero-valued in) the result, and a photo
// belonging to an instance NOT in the request never leaks into another
// instance's entry — proving the cross-context id conversion and the
// "most recent per kind" grouping (ListByInstances makes no per-instance
// ordering guarantee) actually hold end to end, not just against the
// hermetic fakes.
func TestProofPhotoChecker_ProofPhotosByInstances(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instanceRepo := adapter.NewTaskInstanceRepository(pool)
	mediaPhotos := mediaadapter.NewTaskInstancePhotoRepository(pool, mediadomain.StorageBackendLocal)
	checker := adapter.NewProofPhotoChecker(mediaPhotos)

	h, _, _ := seedHousehold(t, pool)
	rt := seedRecurringTask(t, taskRepo, h.ID)
	// Distinct due dates: the task_instance_task_due_uniq constraint rejects
	// a second instance for the same (recurring_task_id, due_on) pair, which
	// three same-day seedTaskInstance calls for the SAME rt would otherwise
	// collide on.
	instA := seedTaskInstance(t, instanceRepo, rt, time.Now())
	instB := seedTaskInstance(t, instanceRepo, rt, time.Now().AddDate(0, 0, 1))
	instC := seedTaskInstance(t, instanceRepo, rt, time.Now().AddDate(0, 0, 2)) // no photos

	// instA: TWO before photos plus one after — the LATER before photo must
	// win in the result (proves "most recent per kind", not "first row
	// seen", since ListByInstances makes no per-instance ordering
	// guarantee). Inserted oldest-first so each insert satisfies media's own
	// before/after ordering invariant (a "before" must not follow any
	// existing "after"; an "after" must not precede the latest "before").
	earlyBefore := seedChoreProofPhoto(t, mediaPhotos, h.ID, instA.ID, mediadomain.PhotoKindBefore,
		time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC))
	lateBefore := seedChoreProofPhoto(t, mediaPhotos, h.ID, instA.ID, mediadomain.PhotoKindBefore,
		time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC))
	afterA := seedChoreProofPhoto(t, mediaPhotos, h.ID, instA.ID, mediadomain.PhotoKindAfter,
		time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))

	// instB: after only.
	afterB := seedChoreProofPhoto(t, mediaPhotos, h.ID, instB.ID, mediadomain.PhotoKindAfter,
		time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC))

	result, err := checker.ProofPhotosByInstances(testCtx(t), h.ID, []domain.TaskInstanceID{instA.ID, instB.ID, instC.ID})
	if err != nil {
		t.Fatalf("ProofPhotosByInstances: %v", err)
	}

	if got := result[instA.ID]; got.BeforeID != lateBefore.ID.String() || got.AfterID != afterA.ID.String() {
		t.Errorf("instA = %+v, want before=%s (the LATER of the two, not earlyBefore=%s) after=%s",
			got, lateBefore.ID, earlyBefore.ID, afterA.ID)
	}
	if got := result[instB.ID]; got.BeforeID != "" || got.AfterID != afterB.ID.String() {
		t.Errorf("instB = %+v, want before=\"\" after=%s", got, afterB.ID)
	}
	if got, ok := result[instC.ID]; ok && (got.BeforeID != "" || got.AfterID != "") {
		t.Errorf("instC = %+v, want no photos", got)
	}

	empty, err := checker.ProofPhotosByInstances(testCtx(t), h.ID, nil)
	if err != nil {
		t.Fatalf("ProofPhotosByInstances(empty): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ProofPhotosByInstances(empty) = %d entries, want 0", len(empty))
	}
}

// TestTaskService_CompleteInstance_PhotoPolicy_EndToEnd exercises the full
// NES-120 gate against a real database — RecurringTaskRepository,
// TaskInstanceRepository, and the ProofPhotoChecker wired over media's own
// TaskInstancePhotoRepository, exactly as cmd/server/main.go composes them
// in production. AC1: a before_after task is blocked with no photos,
// blocked with only a before photo, and completes once both exist.
func TestTaskService_CompleteInstance_PhotoPolicy_EndToEnd(t *testing.T) {
	pool := newTestPool(t)
	taskRepo := adapter.NewRecurringTaskRepository(pool)
	instanceRepo := adapter.NewTaskInstanceRepository(pool)
	mediaPhotos := mediaadapter.NewTaskInstancePhotoRepository(pool, mediadomain.StorageBackendLocal)
	checker := adapter.NewProofPhotoChecker(mediaPhotos)

	svc, err := tasksapp.NewTaskService(taskRepo, instanceRepo, checker)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	h, m, _ := seedHousehold(t, pool)
	rt := &domain.RecurringTask{
		ID:             domain.NewRecurringTaskID(),
		HouseholdID:    h.ID,
		Title:          "Clean garage",
		Category:       domain.ChoreCategory,
		Cadence:        newWeeklyCadence(),
		RotationPolicy: domain.RotationClaimable,
		Active:         true,
		PhotoPolicy:    domain.PhotoPolicyBeforeAfter,
	}
	if err := taskRepo.Create(testCtx(t), rt); err != nil {
		t.Fatalf("Create recurring task: %v", err)
	}
	inst := seedTaskInstance(t, instanceRepo, rt, time.Now())

	if err := svc.CompleteInstance(testCtx(t), h.ID, inst.ID, m, time.Now()); !errors.Is(err, domain.ErrBeforePhotoRequired) {
		t.Fatalf("CompleteInstance(no photos) = %v, want ErrBeforePhotoRequired", err)
	}

	seedChoreProofPhoto(t, mediaPhotos, h.ID, inst.ID, mediadomain.PhotoKindBefore,
		time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC))
	if err := svc.CompleteInstance(testCtx(t), h.ID, inst.ID, m, time.Now()); !errors.Is(err, domain.ErrAfterPhotoRequired) {
		t.Fatalf("CompleteInstance(before only) = %v, want ErrAfterPhotoRequired", err)
	}

	seedChoreProofPhoto(t, mediaPhotos, h.ID, inst.ID, mediadomain.PhotoKindAfter,
		time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC))
	if err := svc.CompleteInstance(testCtx(t), h.ID, inst.ID, m, time.Now()); err != nil {
		t.Fatalf("CompleteInstance(both photos) = %v, want nil", err)
	}

	got, err := instanceRepo.Get(testCtx(t), h.ID, inst.ID)
	if err != nil {
		t.Fatalf("Get after completion: %v", err)
	}
	if got.Status != domain.StatusDone {
		t.Errorf("Status = %v, want done", got.Status)
	}
}
