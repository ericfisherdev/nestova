package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

// This file's fakes are cmd/storage's own test doubles — a separate binary
// (package main) from cmd/server, so cmd/server's identically-shaped fakes
// (fakeMediaStore, fakeMediaPhotoRepo, ...) are not importable here; these
// mirror that same pattern purely for cmd/storage's own migrator/verifier
// tests.

// fakeObject is one stored object/file, as either fakeLocalStore or
// fakeTargetStore holds it.
type fakeObject struct {
	data        []byte
	contentType string
	modified    time.Time
}

// fakeReader adapts a *bytes.Reader (already Read+ReadAt+Seek) into a
// domain.PhotoReader with a no-op Close.
type fakeReader struct{ *bytes.Reader }

func (fakeReader) Close() error { return nil }

// --- fakeLocalStore: the LOCAL backend fake ---

// fakeLocalStore fakes the local domain.PhotoStore: an in-memory,
// ref-keyed map, so migrator/verifier tests never touch real disk.
type fakeLocalStore struct {
	objects   map[mediadomain.StorageRef]fakeObject
	deleted   []mediadomain.StorageRef
	deleteErr error
}

func newFakeLocalStore() *fakeLocalStore {
	return &fakeLocalStore{objects: map[mediadomain.StorageRef]fakeObject{}}
}

func (f *fakeLocalStore) seed(ref mediadomain.StorageRef, data []byte, contentType string) {
	f.objects[ref] = fakeObject{data: data, contentType: contentType}
}

func (f *fakeLocalStore) Put(context.Context, household.HouseholdID, mediadomain.PhotoClass, io.Reader) (mediadomain.PutResult, error) {
	return mediadomain.PutResult{}, errors.New("fakeLocalStore: Put unused by migrate/verify tests")
}

func (f *fakeLocalStore) Open(_ context.Context, ref mediadomain.StorageRef) (mediadomain.PhotoReader, error) {
	obj, ok := f.objects[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", mediadomain.ErrPhotoNotFound, ref)
	}
	return fakeReader{bytes.NewReader(obj.data)}, nil
}

func (f *fakeLocalStore) Delete(_ context.Context, ref mediadomain.StorageRef) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.objects, ref)
	f.deleted = append(f.deleted, ref)
	return nil
}

func (f *fakeLocalStore) URL(context.Context, mediadomain.StorageRef, time.Duration) (string, error) {
	return "", errors.New("fakeLocalStore: URL unused")
}

func (f *fakeLocalStore) SupportsDirectURL() bool { return false }

// --- fakeTargetStore: the object-store (S3-like) backend fake ---

// fakeTargetStore fakes S3PhotoStore's port surface: domain.PhotoStore plus
// ObjectLister, ObjectExister, and RawObjectWriter — everything
// photoMigrator/verifier need from the target backend.
type fakeTargetStore struct {
	objects      map[mediadomain.StorageRef]fakeObject
	putAtErr     error
	putAtCalls   int
	existsErr    error
	existsCalls  []mediadomain.StorageRef
	listErr      error
	classOfKeyFn func(string) (mediadomain.PhotoClass, bool)
}

func newFakeTargetStore() *fakeTargetStore {
	return &fakeTargetStore{objects: map[mediadomain.StorageRef]fakeObject{}}
}

func (f *fakeTargetStore) seed(ref mediadomain.StorageRef, data []byte, contentType string, modified time.Time) {
	f.objects[ref] = fakeObject{data: data, contentType: contentType, modified: modified}
}

func (f *fakeTargetStore) Put(context.Context, household.HouseholdID, mediadomain.PhotoClass, io.Reader) (mediadomain.PutResult, error) {
	return mediadomain.PutResult{}, errors.New("fakeTargetStore: Put unused")
}

func (f *fakeTargetStore) Open(_ context.Context, ref mediadomain.StorageRef) (mediadomain.PhotoReader, error) {
	obj, ok := f.objects[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", mediadomain.ErrPhotoNotFound, ref)
	}
	return fakeReader{bytes.NewReader(obj.data)}, nil
}

func (f *fakeTargetStore) Delete(_ context.Context, ref mediadomain.StorageRef) error {
	delete(f.objects, ref)
	return nil
}

func (f *fakeTargetStore) URL(context.Context, mediadomain.StorageRef, time.Duration) (string, error) {
	return "", errors.New("fakeTargetStore: URL unused")
}

func (f *fakeTargetStore) SupportsDirectURL() bool { return true }

// ListObjects filters f.objects to class via classOfKeyFn (the real
// mediaadapter.ClassOfKey, wired in by each test's newFakeTargetStore
// caller) — every ref this fake ever holds was either seeded directly by a
// test (in the canonical households/.../<class-prefix>/... shape) or
// written by PutAt via photoMigrator.migrateBytes's real
// mediaadapter.BuildStorageKey call, so ClassOfKey classifies it exactly
// like the real S3PhotoStore.ListObjects would.
func (f *fakeTargetStore) ListObjects(_ context.Context, class mediadomain.PhotoClass) ([]mediadomain.ObjectInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []mediadomain.ObjectInfo
	for ref, obj := range f.objects {
		if c, ok := f.classOfKeyFn(ref.String()); ok && c == class {
			out = append(out, mediadomain.ObjectInfo{Key: ref, LastModified: obj.modified})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fakeTargetStore) ObjectExists(_ context.Context, ref mediadomain.StorageRef) (bool, error) {
	f.existsCalls = append(f.existsCalls, ref)
	if f.existsErr != nil {
		return false, f.existsErr
	}
	_, ok := f.objects[ref]
	return ok, nil
}

func (f *fakeTargetStore) PutAt(_ context.Context, ref mediadomain.StorageRef, contentType string, r io.Reader) error {
	f.putAtCalls++
	if f.putAtErr != nil {
		return f.putAtErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[ref] = fakeObject{data: data, contentType: contentType, modified: time.Now()}
	return nil
}

// --- fakePhotoRepo / fakeTaskInstancePhotoRepo: full domain.PhotoRepository /
// domain.TaskInstancePhotoRepository fakes, including NES-133's additions ---

type fakePhotoRepo struct {
	store map[mediadomain.PhotoID]*mediadomain.Photo
}

func newFakePhotoRepo() *fakePhotoRepo {
	return &fakePhotoRepo{store: map[mediadomain.PhotoID]*mediadomain.Photo{}}
}

func (f *fakePhotoRepo) seed(p *mediadomain.Photo) { f.store[p.ID] = p }

func (f *fakePhotoRepo) Create(_ context.Context, p *mediadomain.Photo) error {
	f.store[p.ID] = p
	return nil
}

func (f *fakePhotoRepo) Get(_ context.Context, id mediadomain.PhotoID) (*mediadomain.Photo, error) {
	if p, ok := f.store[id]; ok {
		return p, nil
	}
	return nil, mediadomain.ErrPhotoNotFound
}

func (f *fakePhotoRepo) FindByContentHash(context.Context, household.HouseholdID, string) (*mediadomain.Photo, error) {
	return nil, mediadomain.ErrPhotoNotFound
}

func (f *fakePhotoRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*mediadomain.Photo, error) {
	return nil, nil
}

func (f *fakePhotoRepo) Delete(_ context.Context, id mediadomain.PhotoID) error {
	delete(f.store, id)
	return nil
}

func (f *fakePhotoRepo) ListAllStorageRefs(_ context.Context, backend mediadomain.StorageBackend) ([]mediadomain.StorageRef, error) {
	refs := make([]mediadomain.StorageRef, 0)
	for _, p := range f.store {
		if p.StorageBackend == backend {
			refs = append(refs, p.StorageRef)
		}
	}
	return refs, nil
}

func (f *fakePhotoRepo) ExistsByStorageRef(_ context.Context, ref mediadomain.StorageRef, backend mediadomain.StorageBackend) (bool, error) {
	for _, p := range f.store {
		if p.StorageRef == ref && p.StorageBackend == backend {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakePhotoRepo) ListByBackend(_ context.Context, backend mediadomain.StorageBackend, afterID mediadomain.PhotoID, limit int) ([]*mediadomain.Photo, error) {
	matches := make([]*mediadomain.Photo, 0, len(f.store))
	for _, p := range f.store {
		if p.StorageBackend == backend && p.ID.String() > afterID.String() {
			matches = append(matches, p)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID.String() < matches[j].ID.String() })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func (f *fakePhotoRepo) MigrateStorageBackend(_ context.Context, id mediadomain.PhotoID, newRef mediadomain.StorageRef, newBackend mediadomain.StorageBackend, contentHash string) (bool, error) {
	p, ok := f.store[id]
	if !ok || p.StorageBackend != mediadomain.StorageBackendLocal {
		return false, nil
	}
	p.StorageRef = newRef
	p.StorageBackend = newBackend
	if p.ContentHash == "" {
		p.ContentHash = contentHash
	}
	return true, nil
}

type fakeTaskInstancePhotoRepo struct {
	store map[mediadomain.TaskInstancePhotoID]*mediadomain.TaskInstancePhoto
}

func newFakeTaskInstancePhotoRepo() *fakeTaskInstancePhotoRepo {
	return &fakeTaskInstancePhotoRepo{store: map[mediadomain.TaskInstancePhotoID]*mediadomain.TaskInstancePhoto{}}
}

func (f *fakeTaskInstancePhotoRepo) seed(p *mediadomain.TaskInstancePhoto) { f.store[p.ID] = p }

func (f *fakeTaskInstancePhotoRepo) Create(_ context.Context, p *mediadomain.TaskInstancePhoto) error {
	f.store[p.ID] = p
	return nil
}

func (f *fakeTaskInstancePhotoRepo) Get(_ context.Context, id mediadomain.TaskInstancePhotoID) (*mediadomain.TaskInstancePhoto, error) {
	if p, ok := f.store[id]; ok {
		return p, nil
	}
	return nil, mediadomain.ErrTaskInstancePhotoNotFound
}

func (f *fakeTaskInstancePhotoRepo) InstanceExists(context.Context, household.HouseholdID, mediadomain.TaskInstanceID) (bool, error) {
	return true, nil
}

func (f *fakeTaskInstancePhotoRepo) LatestTakenAt(context.Context, household.HouseholdID, mediadomain.TaskInstanceID, mediadomain.PhotoKind) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeTaskInstancePhotoRepo) ListByInstance(context.Context, household.HouseholdID, mediadomain.TaskInstanceID) ([]*mediadomain.TaskInstancePhoto, error) {
	return nil, nil
}

func (f *fakeTaskInstancePhotoRepo) ListByInstances(context.Context, household.HouseholdID, []mediadomain.TaskInstanceID) ([]*mediadomain.TaskInstancePhoto, error) {
	return nil, nil
}

func (f *fakeTaskInstancePhotoRepo) ListAllStorageRefs(_ context.Context, backend mediadomain.StorageBackend) ([]mediadomain.StorageRef, error) {
	refs := make([]mediadomain.StorageRef, 0)
	for _, p := range f.store {
		if p.StorageBackend == backend {
			refs = append(refs, p.StorageRef)
		}
	}
	return refs, nil
}

func (f *fakeTaskInstancePhotoRepo) DeleteUploadedBefore(context.Context, mediadomain.StorageBackend, time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeTaskInstancePhotoRepo) ListStorageRefsUploadedBefore(context.Context, mediadomain.StorageBackend, time.Time) ([]mediadomain.StorageRef, error) {
	return nil, nil
}

func (f *fakeTaskInstancePhotoRepo) ExistsByStorageRef(_ context.Context, ref mediadomain.StorageRef, backend mediadomain.StorageBackend) (bool, error) {
	for _, p := range f.store {
		if p.StorageRef == ref && p.StorageBackend == backend {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeTaskInstancePhotoRepo) ListByBackend(_ context.Context, backend mediadomain.StorageBackend, afterID mediadomain.TaskInstancePhotoID, limit int) ([]*mediadomain.TaskInstancePhoto, error) {
	matches := make([]*mediadomain.TaskInstancePhoto, 0, len(f.store))
	for _, p := range f.store {
		if p.StorageBackend == backend && p.ID.String() > afterID.String() {
			matches = append(matches, p)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID.String() < matches[j].ID.String() })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

func (f *fakeTaskInstancePhotoRepo) MigrateStorageBackend(_ context.Context, id mediadomain.TaskInstancePhotoID, newRef mediadomain.StorageRef, newBackend mediadomain.StorageBackend) (bool, error) {
	p, ok := f.store[id]
	if !ok || p.StorageBackend != mediadomain.StorageBackendLocal {
		return false, nil
	}
	p.StorageRef = newRef
	p.StorageBackend = newBackend
	return true, nil
}
