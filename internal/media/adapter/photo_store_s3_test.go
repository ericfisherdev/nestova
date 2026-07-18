package adapter_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/media/app"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// s3TestEndpointEnv gates every test in this file: unset means "skip
// hermetically" (the ticket's requirement), so `go test ./...` never
// depends on a running MinIO instance. Point it at a disposable MinIO
// container, e.g.:
//
//	docker run -d --name nes132-minio -e MINIO_ROOT_USER=test \
//	  -e MINIO_ROOT_PASSWORD=testtest123 -p 127.0.0.1:59000:9000 minio/minio server /data
//	NESTOVA_TEST_S3_ENDPOINT=http://127.0.0.1:59000 go test ./internal/media/adapter/...
const s3TestEndpointEnv = "NESTOVA_TEST_S3_ENDPOINT"

// s3TestAccessKey / s3TestSecretKey match the disposable MinIO container's
// MINIO_ROOT_USER/MINIO_ROOT_PASSWORD documented above.
const (
	s3TestAccessKey = "test"
	s3TestSecretKey = "testtest123"
	s3TestRegion    = "us-east-1"
)

// newTestS3Store skips the test unless NESTOVA_TEST_S3_ENDPOINT is set,
// otherwise creates a fresh, uniquely-named bucket (so parallel/sequential
// test runs never collide) and returns an adapter.S3PhotoStore against it,
// registering cleanup to empty and delete the bucket afterward.
func newTestS3Store(t *testing.T, maxUploadBytes int64) *adapter.S3PhotoStore {
	t.Helper()
	endpoint := os.Getenv(s3TestEndpointEnv)
	if endpoint == "" {
		t.Skipf("set %s to run the S3 photo store tests against MinIO", s3TestEndpointEnv)
	}

	bucket := fmt.Sprintf("nes132-test-%d", time.Now().UnixNano())
	ctx := context.Background()
	client := rawTestS3Client(ctx, t, endpoint)
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create test bucket %q: %v", bucket, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		emptyTestBucket(cleanupCtx, t, client, bucket)
		if _, err := client.DeleteBucket(cleanupCtx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Logf("cleanup: delete test bucket %q: %v", bucket, err)
		}
	})

	if maxUploadBytes <= 0 {
		maxUploadBytes = 10 << 20
	}
	store, err := adapter.NewS3PhotoStore(ctx, adapter.S3Params{
		Endpoint: endpoint, Region: s3TestRegion, Bucket: bucket,
		AccessKeyID: s3TestAccessKey, SecretAccessKey: s3TestSecretKey,
		UsePathStyle: true, PresignTTL: 5 * time.Minute, MaxUploadBytes: maxUploadBytes,
	})
	if err != nil {
		t.Fatalf("NewS3PhotoStore: %v", err)
	}
	return store
}

// rawTestS3Client builds a plain *s3.Client against endpoint — used only for
// test-fixture bucket setup/teardown, which NewS3PhotoStore does not expose
// (it manages a bucket that already exists, not bucket lifecycle).
func rawTestS3Client(ctx context.Context, t *testing.T, endpoint string) *s3.Client {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(s3TestRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s3TestAccessKey, s3TestSecretKey, "")),
	)
	if err != nil {
		t.Fatalf("load AWS config for raw test client: %v", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

func emptyTestBucket(ctx context.Context, t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			t.Logf("cleanup: list objects in %q: %v", bucket, err)
			return
		}
		for _, obj := range page.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: obj.Key}); err != nil {
				t.Logf("cleanup: delete object %q: %v", aws.ToString(obj.Key), err)
			}
		}
	}
}

// TestS3PhotoStorePutOpenURLDelete covers the S3PhotoStore basics against a
// real MinIO endpoint: Put stores validated bytes, Open reads them back
// unchanged, URL returns a presigned GET that a plain HTTP client can
// actually fetch the same bytes from (proving the redirect target genuinely
// works, not just that a string was returned), and Delete removes the
// object — after which Open/URL both report ErrPhotoNotFound.
func TestS3PhotoStorePutOpenURLDelete(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	want := jpegBytes(t)

	result, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if result.Ref == "" || result.ContentHash == "" || result.SizeBytes != int64(len(want)) || result.ContentType != "image/jpeg" {
		t.Fatalf("Put result = %+v", result)
	}

	rc, err := store.Open(context.Background(), result.Ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read opened photo: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Open returned %d bytes, want %d matching bytes", len(got), len(want))
	}

	if !store.SupportsDirectURL() {
		t.Fatal("SupportsDirectURL() = false, want true for S3PhotoStore")
	}
	presigned, err := store.URL(context.Background(), result.Ref, 5*time.Minute)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if !strings.Contains(presigned, "X-Amz-Signature") {
		t.Fatalf("URL = %q, want a presigned URL with a signature query parameter", presigned)
	}
	resp, err := http.Get(presigned) //nolint:noctx // test-only fetch of a presigned URL
	if err != nil {
		t.Fatalf("fetch presigned URL: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET presigned URL: status=%d, want 200", resp.StatusCode)
	}
	// The presigned GET's own response must carry the app's Cache-Control —
	// baked in via PutObjectInput.CacheControl at upload time and reasserted
	// via ResponseCacheControl on this specific presign, so it never falls
	// back to whatever permissive default MinIO/S3 would otherwise apply.
	if cc := resp.Header.Get("Cache-Control"); cc != "private, max-age=3600" {
		t.Fatalf("presigned GET response Cache-Control = %q, want %q", cc, "private, max-age=3600")
	}
	fetched, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned URL response: %v", err)
	}
	if !bytes.Equal(fetched, want) {
		t.Fatalf("presigned URL served %d bytes, want %d matching bytes", len(fetched), len(want))
	}

	if err := store.Delete(context.Background(), result.Ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Idempotent, mirroring LocalPhotoStore.Delete.
	if err := store.Delete(context.Background(), result.Ref); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := store.Open(context.Background(), result.Ref); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Open after delete = %v, want ErrPhotoNotFound", err)
	}
	if _, err := store.URL(context.Background(), result.Ref, 5*time.Minute); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("URL after delete = %v, want ErrPhotoNotFound", err)
	}
}

// TestS3PhotoStoreKeysByClass covers AC1: album uploads land under
// photos/, chore-proof uploads under chore-photos/, and ListObjects (the
// domain.ObjectLister the reaper drives) reports each object under its own
// class only, asserting the ACTUAL object keys MinIO stored them under —
// not just the ref string Put returned.
func TestS3PhotoStoreKeysByClass(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	data := jpegBytes(t)

	album, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(album): %v", err)
	}
	choreProof, err := store.Put(context.Background(), hh, domain.PhotoClassChoreProof, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(choreProof): %v", err)
	}

	wantAlbumPrefix := "households/" + hh.String() + "/photos/"
	wantChorePrefix := "households/" + hh.String() + "/chore-photos/"
	if !strings.HasPrefix(album.Ref.String(), wantAlbumPrefix) {
		t.Fatalf("album ref %q does not start with %q", album.Ref, wantAlbumPrefix)
	}
	if !strings.HasPrefix(choreProof.Ref.String(), wantChorePrefix) {
		t.Fatalf("chore-proof ref %q does not start with %q", choreProof.Ref, wantChorePrefix)
	}

	albumObjects, err := store.ListObjects(context.Background(), domain.PhotoClassAlbum)
	if err != nil {
		t.Fatalf("ListObjects(album): %v", err)
	}
	if len(albumObjects) != 1 || albumObjects[0].Key != album.Ref {
		t.Fatalf("ListObjects(album) = %v, want exactly the album ref", albumObjects)
	}
	choreObjects, err := store.ListObjects(context.Background(), domain.PhotoClassChoreProof)
	if err != nil {
		t.Fatalf("ListObjects(choreProof): %v", err)
	}
	if len(choreObjects) != 1 || choreObjects[0].Key != choreProof.Ref {
		t.Fatalf("ListObjects(choreProof) = %v, want exactly the chore-proof ref", choreObjects)
	}
}

// TestS3PhotoStoreValidatesUploadsLikeLocalPhotoStore covers a slice of
// LocalPhotoStore's validation contract (photo_store_test.go covers it
// exhaustively) to prove S3PhotoStore's shared validateAndStage really is
// wired in, not skipped for the object-store path.
func TestS3PhotoStoreValidatesUploadsLikeLocalPhotoStore(t *testing.T) {
	hh := household.NewHouseholdID()
	cases := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{"plain text is rejected", []byte("this is not an image"), domain.ErrUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestS3Store(t, 10<<20)
			if _, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(tc.data)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Put error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	t.Run("oversize upload is rejected", func(t *testing.T) {
		store := newTestS3Store(t, 8)
		if _, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(pngBytes(t))); !errors.Is(err, domain.ErrPhotoTooLarge) {
			t.Fatalf("Put error = %v, want ErrPhotoTooLarge", err)
		}
	})
}

// TestS3PhotoStoreObjectExistsAndPutAt covers NES-133's storage migrator
// port surface (ObjectExister/RawObjectWriter) against real MinIO:
// ObjectExists reports false before the key is written and true after,
// PutAt writes bytes VERBATIM (no sniffing/decoding/hashing) to an EXACT
// caller-chosen key, and the resulting object is readable back through the
// ordinary Open path unchanged.
func TestS3PhotoStoreObjectExistsAndPutAt(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	data := jpegBytes(t)
	ref := domain.StorageRef("households/" + hh.String() + "/photos/aa/put-at.jpg")

	exists, err := store.ObjectExists(context.Background(), ref)
	if err != nil {
		t.Fatalf("ObjectExists before PutAt: %v", err)
	}
	if exists {
		t.Fatal("ObjectExists before PutAt = true, want false")
	}

	if err := store.PutAt(context.Background(), ref, "image/jpeg", bytes.NewReader(data)); err != nil {
		t.Fatalf("PutAt: %v", err)
	}

	exists, err = store.ObjectExists(context.Background(), ref)
	if err != nil {
		t.Fatalf("ObjectExists after PutAt: %v", err)
	}
	if !exists {
		t.Fatal("ObjectExists after PutAt = false, want true")
	}

	rc, err := store.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("Open after PutAt: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read PutAt object: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("PutAt object = %d bytes, want %d matching bytes (verbatim, no re-encoding)", len(got), len(data))
	}

	// PutAt is a plain overwrite at the same key — content-addressed dedup
	// upstream (photoMigrator) is what makes re-calling this a no-op in
	// practice via ObjectExists, not any special behavior on PutAt itself.
	if err := store.PutAt(context.Background(), ref, "image/jpeg", bytes.NewReader(data)); err != nil {
		t.Fatalf("second PutAt to the same key: %v", err)
	}
}

// TestS3PhotoStoreObjectExistsUnknownKey covers ObjectExists' false path
// for a key that was never written at all, distinct from "written then
// deleted" (already covered indirectly by other tests' Delete assertions).
func TestS3PhotoStoreObjectExistsUnknownKey(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	exists, err := store.ObjectExists(context.Background(), domain.StorageRef("households/never/written.jpg"))
	if err != nil {
		t.Fatalf("ObjectExists: %v", err)
	}
	if exists {
		t.Fatal("ObjectExists for an unknown key = true, want false")
	}
}

// TestS3PhotoStoreSkipsSSEForCustomEndpoint covers a verified, deliberate
// deviation from the ticket's original SSE assumption: a real MinIO
// instance with no KMS configured does NOT silently ignore an SSE-S3
// request — it rejects the PutObject outright with 501 NotImplemented
// ("Server side encryption specified but KMS is not configured"). Put
// therefore only requests SSE-S3 against real AWS S3 (no custom Endpoint —
// see the requestSSE field doc); this test's Put succeeding against a
// custom-endpoint (MinIO) store is the proof that no SSE header was sent —
// a Put that DID send one would fail with exactly the 501 above, as
// confirmed while developing this adapter.
func TestS3PhotoStoreSkipsSSEForCustomEndpoint(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	if _, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(jpegBytes(t))); err != nil {
		t.Fatalf("Put against a custom (MinIO) endpoint must not request SSE-S3: %v", err)
	}
}

// --- reaper integration against real MinIO ---

// fakeReaperPhotoRepo/fakeReaperTaskInstancePhotoRepo are minimal
// domain.PhotoRepository/domain.TaskInstancePhotoRepository fakes local to
// this file: the reaper tests below need to control exactly which refs are
// "referenced" against a REAL S3PhotoStore/MinIO backend, without requiring
// a second gated dependency (Postgres) on top of MinIO — the referenced-refs
// query path itself is already covered against real Postgres by
// TestPhotoRepositoryListAllStorageRefs/TestTaskInstancePhotoRepositoryListAllStorageRefs.
// existsOverride lets a test make ExistsByStorageRef (the reaper's
// pre-delete TOCTOU recheck — see ReaperService's doc) diverge from refs,
// simulating a row that commits after the bulk snapshot but before the
// recheck; unset refs fall back to a plain refs lookup.
type fakeReaperPhotoRepo struct {
	refs           []domain.StorageRef
	existsOverride map[domain.StorageRef]bool
}

func (f *fakeReaperPhotoRepo) Create(context.Context, *domain.Photo) error { return nil }
func (f *fakeReaperPhotoRepo) Get(context.Context, domain.PhotoID) (*domain.Photo, error) {
	return nil, domain.ErrPhotoNotFound
}

func (f *fakeReaperPhotoRepo) FindByContentHash(context.Context, household.HouseholdID, string) (*domain.Photo, error) {
	return nil, domain.ErrPhotoNotFound
}

func (f *fakeReaperPhotoRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*domain.Photo, error) {
	return nil, nil
}
func (f *fakeReaperPhotoRepo) Delete(context.Context, domain.PhotoID) error { return nil }

// ListAllStorageRefs/ExistsByStorageRef ignore the backend parameter: these
// MinIO-integration reaper tests exercise exactly one backend (S3) end to
// end, so there is never a second backend's row to filter out here — the
// filtering itself (NES-132) is covered by the dedicated gated Postgres
// tests (TestPhotoRepositoryListAllStorageRefsFiltersByBackend and its
// chore-proof counterpart) and the unit-level reaper tests.
func (f *fakeReaperPhotoRepo) ListAllStorageRefs(context.Context, domain.StorageBackend) ([]domain.StorageRef, error) {
	return f.refs, nil
}

func (f *fakeReaperPhotoRepo) ExistsByStorageRef(_ context.Context, ref domain.StorageRef, _ domain.StorageBackend) (bool, error) {
	if v, ok := f.existsOverride[ref]; ok {
		return v, nil
	}
	for _, r := range f.refs {
		if r == ref {
			return true, nil
		}
	}
	return false, nil
}

// ListByBackend / MigrateStorageBackend are unused by these reaper-focused
// tests; implemented only to satisfy the interface (NES-133's storage
// migrator).
func (f *fakeReaperPhotoRepo) ListByBackend(context.Context, domain.StorageBackend, domain.PhotoID, int) ([]*domain.Photo, error) {
	return nil, nil
}

func (f *fakeReaperPhotoRepo) MigrateStorageBackend(context.Context, domain.PhotoID, domain.StorageRef, domain.StorageBackend, string) (bool, error) {
	return false, nil
}

type fakeReaperTaskInstancePhotoRepo struct{ refs []domain.StorageRef }

func (f *fakeReaperTaskInstancePhotoRepo) Create(context.Context, *domain.TaskInstancePhoto) error {
	return nil
}

func (f *fakeReaperTaskInstancePhotoRepo) Get(context.Context, domain.TaskInstancePhotoID) (*domain.TaskInstancePhoto, error) {
	return nil, domain.ErrTaskInstancePhotoNotFound
}

func (f *fakeReaperTaskInstancePhotoRepo) InstanceExists(context.Context, household.HouseholdID, domain.TaskInstanceID) (bool, error) {
	return false, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) LatestTakenAt(context.Context, household.HouseholdID, domain.TaskInstanceID, domain.PhotoKind) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) ListByInstance(context.Context, household.HouseholdID, domain.TaskInstanceID) ([]*domain.TaskInstancePhoto, error) {
	return nil, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) ListByInstances(context.Context, household.HouseholdID, []domain.TaskInstanceID) ([]*domain.TaskInstancePhoto, error) {
	return nil, nil
}

// ListAllStorageRefs/ExistsByStorageRef ignore the backend parameter — see
// fakeReaperPhotoRepo's identical comment for why.
func (f *fakeReaperTaskInstancePhotoRepo) ListAllStorageRefs(context.Context, domain.StorageBackend) ([]domain.StorageRef, error) {
	return f.refs, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) DeleteUploadedBefore(context.Context, domain.StorageBackend, time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) ListStorageRefsUploadedBefore(context.Context, domain.StorageBackend, time.Time) ([]domain.StorageRef, error) {
	return nil, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) ExistsByStorageRef(_ context.Context, ref domain.StorageRef, _ domain.StorageBackend) (bool, error) {
	for _, r := range f.refs {
		if r == ref {
			return true, nil
		}
	}
	return false, nil
}

// ListByBackend / MigrateStorageBackend are unused by these reaper-focused
// tests; implemented only to satisfy the interface (NES-133's storage
// migrator).
func (f *fakeReaperTaskInstancePhotoRepo) ListByBackend(context.Context, domain.StorageBackend, domain.TaskInstancePhotoID, int) ([]*domain.TaskInstancePhoto, error) {
	return nil, nil
}

func (f *fakeReaperTaskInstancePhotoRepo) MigrateStorageBackend(context.Context, domain.TaskInstancePhotoID, domain.StorageRef, domain.StorageBackend) (bool, error) {
	return false, nil
}

// objectLastModified finds ref's LastModified as MinIO itself reports it,
// via a real ListObjects call — used to anchor the reaper tests below
// against the object's ACTUAL server-recorded timestamp rather than the
// test's own wall clock, so grace-window comparisons can use fabricated
// `now` values (ReaperService.Run already takes now as a parameter) instead
// of real time.Sleep calls, eliminating CI-pause flakiness entirely.
func objectLastModified(t *testing.T, store *adapter.S3PhotoStore, class domain.PhotoClass, ref domain.StorageRef) time.Time {
	t.Helper()
	objects, err := store.ListObjects(context.Background(), class)
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	for _, obj := range objects {
		if obj.Key == ref {
			return obj.LastModified
		}
	}
	t.Fatalf("ref %s not found in ListObjects(%s)", ref, class)
	return time.Time{}
}

// TestS3PhotoStoreReaperDeletesOrphanAfterGraceWindow covers AC3 end to end
// against real MinIO: deleting a photo hides it immediately (simulated here
// by never referencing its ref at all — the row-deletion half is already
// covered by the Postgres-gated repository tests), but the OBJECT survives
// until the grace window elapses; the reaper then removes it, from the
// correct class.
//
// Anchored on the object's REAL, MinIO-reported LastModified (via
// objectLastModified) rather than the test's wall clock: Run's `now`
// parameter is fabricated relative to that anchor, so this test needs no
// real time.Sleep and cannot flake under CI scheduling pauses.
func TestS3PhotoStoreReaperDeletesOrphanAfterGraceWindow(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	result, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(jpegBytes(t)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	lastModified := objectLastModified(t, store, domain.PhotoClassAlbum, result.Ref)

	const grace = time.Hour
	photos := &fakeReaperPhotoRepo{} // no rows reference anything: simulates the row already deleted
	choreProofPhotos := &fakeReaperTaskInstancePhotoRepo{}
	reaper, err := app.NewReaperService(store, store, domain.StorageBackendS3, photos, choreProofPhotos, grace, 0)
	if err != nil {
		t.Fatalf("NewReaperService: %v", err)
	}

	// A `now` still within the grace window of the object's real
	// LastModified — must NOT be deleted yet (it could be a concurrent,
	// not-yet-committed upload).
	before, err := reaper.Run(context.Background(), lastModified.Add(grace/2))
	if err != nil {
		t.Fatalf("Run (before grace window): %v", err)
	}
	if before.OrphansDeleted[domain.PhotoClassAlbum] != 0 {
		t.Fatalf("OrphansDeleted[album] = %d, want 0 before the grace window elapses", before.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if _, err := store.Open(context.Background(), result.Ref); err != nil {
		t.Fatalf("object was reaped before its grace window elapsed: Open = %v", err)
	}

	// A `now` well past the grace window.
	after, err := reaper.Run(context.Background(), lastModified.Add(grace*2))
	if err != nil {
		t.Fatalf("Run (after grace window): %v", err)
	}
	if after.OrphansDeleted[domain.PhotoClassAlbum] != 1 {
		t.Fatalf("OrphansDeleted[album] = %d, want 1 after the grace window elapses", after.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if _, err := store.Open(context.Background(), result.Ref); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Open after reaping = %v, want ErrPhotoNotFound", err)
	}
}

// TestS3PhotoStoreReaperRestoreSafety covers AC5 end to end against real
// MinIO: an object whose referencing row reappears (simulating "restore a
// week-old DB dump against the same bucket") before the reaper ever runs
// must survive — even though it is well past the grace window — because
// referencedRefs sees it as referenced again on this Run.
//
// Anchored on the object's REAL, MinIO-reported LastModified (objectLastModified),
// same as TestS3PhotoStoreReaperDeletesOrphanAfterGraceWindow — no real sleep.
func TestS3PhotoStoreReaperRestoreSafety(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	result, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(jpegBytes(t)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	lastModified := objectLastModified(t, store, domain.PhotoClassAlbum, result.Ref)

	const grace = time.Hour

	// The "restore": the row referencing result.Ref is back, as if a DB
	// backup had just been restored against this same bucket.
	photos := &fakeReaperPhotoRepo{refs: []domain.StorageRef{result.Ref}}
	choreProofPhotos := &fakeReaperTaskInstancePhotoRepo{}
	reaper, err := app.NewReaperService(store, store, domain.StorageBackendS3, photos, choreProofPhotos, grace, 0)
	if err != nil {
		t.Fatalf("NewReaperService: %v", err)
	}

	// Well past the grace window, but the row is referenced again.
	result2, err := reaper.Run(context.Background(), lastModified.Add(grace*2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result2.OrphansDeleted[domain.PhotoClassAlbum] != 0 {
		t.Fatalf("OrphansDeleted[album] = %d, want 0 — the restored row must protect its object", result2.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if _, err := store.Open(context.Background(), result.Ref); err != nil {
		t.Fatalf("restored, still-referenced object must remain openable: Open = %v", err)
	}
	if _, err := store.URL(context.Background(), result.Ref, 5*time.Minute); err != nil {
		t.Fatalf("restored, still-referenced object must still resolve a URL: %v", err)
	}
}

// TestS3PhotoStoreReaperRecheckCatchesRowCommittedAfterSnapshot covers the
// TOCTOU-narrowing fix against real MinIO: a row that commits AFTER the
// bulk ListAllStorageRefs snapshot but BEFORE the per-object delete must
// still protect its object, because sweepClass's targeted
// ExistsByStorageRef recheck — not the stale snapshot — is what gates the
// actual S3 DeleteObject call. existsOverride simulates exactly this: the
// snapshot (refs) is empty, but the recheck reports the ref referenced —
// mirroring reaper_service_test.go's identical unit-level coverage, but
// proving the real S3PhotoStore.Delete call is genuinely never reached.
func TestS3PhotoStoreReaperRecheckCatchesRowCommittedAfterSnapshot(t *testing.T) {
	store := newTestS3Store(t, 10<<20)
	hh := household.NewHouseholdID()
	result, err := store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(jpegBytes(t)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	lastModified := objectLastModified(t, store, domain.PhotoClassAlbum, result.Ref)

	const grace = time.Hour
	photos := &fakeReaperPhotoRepo{existsOverride: map[domain.StorageRef]bool{result.Ref: true}} // refs (snapshot) empty; recheck says referenced
	choreProofPhotos := &fakeReaperTaskInstancePhotoRepo{}
	reaper, err := app.NewReaperService(store, store, domain.StorageBackendS3, photos, choreProofPhotos, grace, 0)
	if err != nil {
		t.Fatalf("NewReaperService: %v", err)
	}

	result2, err := reaper.Run(context.Background(), lastModified.Add(grace*2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result2.OrphansDeleted[domain.PhotoClassAlbum] != 0 {
		t.Fatalf("OrphansDeleted[album] = %d, want 0 — the recheck must catch the mid-run commit even though the snapshot missed it", result2.OrphansDeleted[domain.PhotoClassAlbum])
	}
	if _, err := store.Open(context.Background(), result.Ref); err != nil {
		t.Fatalf("object must survive when the recheck (not the snapshot) reports it referenced: Open = %v", err)
	}
}
