package main

// Gated MinIO + Postgres integration coverage for NES-133's storage
// migrator/verifier, exercising the REAL adapters (LocalPhotoStore,
// S3PhotoStore, the pgx-backed PhotoRepository/TaskInstancePhotoRepository)
// rather than fakes — the same MinIO test pattern
// internal/media/adapter/photo_store_s3_test.go establishes
// (NESTOVA_TEST_S3_ENDPOINT-gated, a disposable per-test bucket), paired
// with the same gated-Postgres pattern internal/media/adapter/postgres_test.go
// establishes (NESTOVA_TEST_DATABASE_URL-gated, a reset+migrated schema, a
// database name ending in "test" as a safety rail).
//
// Run against a disposable MinIO container and the shared gated test
// database, e.g.:
//
//	docker run -d --name nes133-minio -e MINIO_ROOT_USER=test \
//	  -e MINIO_ROOT_PASSWORD=testtest123 -p 127.0.0.1:59001:9000 minio/minio server /data
//	NESTOVA_TEST_S3_ENDPOINT=http://127.0.0.1:59001 \
//	NESTOVA_TEST_DATABASE_URL=postgres://... \
//	  go test ./cmd/storage/...

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
)

const (
	integrationS3TestEndpointEnv = "NESTOVA_TEST_S3_ENDPOINT"
	integrationS3AccessKey       = "test"
	integrationS3SecretKey       = "testtest123"
	integrationS3Region          = "us-east-1"
)

// preResetSweep mirrors internal/media/adapter/postgres_test.go's identical
// helper (unexported there, so not importable from this separate binary):
// a best-effort DELETE of any lingering s3-stamped photo/task_instance_photo
// rows, run immediately before EVERY migrate.Reset call — migration 00032's
// down-migration deliberately aborts while ANY row is stamped 's3' (correct
// for a real production rollback, fatal to a disposable test fixture that
// leaves one behind, whether from this file's own tests or a genuinely
// concurrent run against the same shared database). See that helper's doc
// for the full rationale; errors are discarded entirely here for the same
// reason (a virgin database has neither table yet).
func preResetSweep(ctx context.Context, dsn string) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	_, _ = conn.Exec(ctx, `DELETE FROM task_instance_photo WHERE storage_backend = 's3'`)
	_, _ = conn.Exec(ctx, `DELETE FROM photo WHERE storage_backend = 's3'`)
}

// newIntegrationPool returns a pool against this binary's own derived
// database (NES-149), freshly reset and migrated. preResetSweep runs
// before each reset (see its own doc) via the dbtest pre-reset hook.
func newIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.NewIsolatedPool(t, "storage", dbtest.WithPreReset(preResetSweep))
}

// newIntegrationS3Store mirrors photo_store_s3_test.go's newTestS3Store: a
// disposable, uniquely-named bucket against NESTOVA_TEST_S3_ENDPOINT.
func newIntegrationS3Store(t *testing.T) *mediaadapter.S3PhotoStore {
	t.Helper()
	endpoint := os.Getenv(integrationS3TestEndpointEnv)
	if endpoint == "" {
		t.Skipf("set %s to run the storage migrator/verifier integration tests", integrationS3TestEndpointEnv)
	}

	bucket := fmt.Sprintf("nes133-test-%d", time.Now().UnixNano())
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(integrationS3Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(integrationS3AccessKey, integrationS3SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("load AWS config for raw test client: %v", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create test bucket %q: %v", bucket, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(cleanupCtx)
			if err != nil {
				t.Logf("cleanup: list objects in %q: %v", bucket, err)
				return
			}
			for _, obj := range page.Contents {
				if _, err := client.DeleteObject(cleanupCtx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: obj.Key}); err != nil {
					t.Logf("cleanup: delete object %q: %v", aws.ToString(obj.Key), err)
				}
			}
		}
		if _, err := client.DeleteBucket(cleanupCtx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Logf("cleanup: delete test bucket %q: %v", bucket, err)
		}
	})

	store, err := mediaadapter.NewS3PhotoStore(ctx, mediaadapter.S3Params{
		Endpoint: endpoint, Region: integrationS3Region, Bucket: bucket,
		AccessKeyID: integrationS3AccessKey, SecretAccessKey: integrationS3SecretKey,
		UsePathStyle: true, PresignTTL: 5 * time.Minute, MaxUploadBytes: 10 << 20,
	})
	if err != nil {
		t.Fatalf("NewS3PhotoStore: %v", err)
	}
	return store
}

func integrationTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedIntegrationHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	if _, err := pool.Exec(integrationTestCtx(t), `INSERT INTO household (id, name) VALUES ($1, $2)`, id.String(), "Storage Integration Test"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

// testJPEGBytes encodes a genuinely decodable JPEG whose pixel content is
// derived from marker, so different markers produce distinct content
// hashes — LocalPhotoStore.Put runs REAL validation (content sniffing plus
// a full image decode), so this integration suite's seed data must be an
// actual image, unlike the hermetic unit tests' fake stores.
func testJPEGBytes(t *testing.T, marker byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.RGBA{R: marker, G: 128, B: 64, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// putLocalAndCreateAlbumRow uploads data through the REAL LocalPhotoStore
// (full validation, genuine content-addressed key) and persists a matching
// photo row via the REAL PhotoRepository, mirroring exactly what
// PhotoService.Upload does — the migrator/verifier's real starting state.
func putLocalAndCreateAlbumRow(ctx context.Context, t *testing.T, local *mediaadapter.LocalPhotoStore, photos *mediaadapter.PhotoRepository, hh household.HouseholdID, data []byte) *mediadomain.Photo {
	t.Helper()
	result, err := local.Put(ctx, hh, mediadomain.PhotoClassAlbum, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("local Put: %v", err)
	}
	photo := &mediadomain.Photo{
		ID: mediadomain.NewPhotoID(), HouseholdID: hh, StorageRef: result.Ref,
		ContentHash: result.ContentHash, SizeBytes: result.SizeBytes, ContentType: result.ContentType,
	}
	if err := photos.Create(ctx, photo); err != nil {
		t.Fatalf("Create photo row: %v", err)
	}
	return photo
}

// TestIntegrationMigrateVerifyLifecycle covers NES-133's core lifecycle end
// to end against real MinIO and real Postgres: AC1 (interrupting/resuming
// the migrator is safe and idempotent), AC2 (a hash mismatch aborts that
// photo's flip and is reported, while the migrator continues with other
// rows), AC3 (verify detects a deleted object and reports it, and does not
// false-positive on a clean state), and AC4 (--delete-local removes a local
// file only after verification).
func TestIntegrationMigrateVerifyLifecycle(t *testing.T) {
	pool := newIntegrationPool(t)
	s3Store := newIntegrationS3Store(t)
	localRoot := t.TempDir()
	local, err := mediaadapter.NewLocalPhotoStore(localRoot, 10<<20)
	if err != nil {
		t.Fatalf("NewLocalPhotoStore: %v", err)
	}
	// Both repositories are constructed bound to StorageBackendLocal, NOT
	// s3: Create's own configured backend is what stamps each SEEDED row
	// 'local' here (mirroring the pre-migration app, where the write target
	// really was local) — every method photoMigrator/verifier actually call
	// (ListByBackend, ExistsByStorageRef, MigrateStorageBackend,
	// ListAllStorageRefs) takes its own explicit backend parameter and does
	// not consult the repository's configured backend at all, so this same
	// local-bound instance is equally correct for driving the migrator.
	photos := mediaadapter.NewPhotoRepository(pool, mediadomain.StorageBackendLocal)
	choreProofPhotos := mediaadapter.NewTaskInstancePhotoRepository(pool, mediadomain.StorageBackendLocal)
	ctx := integrationTestCtx(t)
	hh := seedIntegrationHousehold(t, pool)
	// This test deliberately leaves several rows stamped 's3' (that is the
	// point of testing the migrator). Deleting the household cascades to
	// both photo and task_instance_photo (ON DELETE CASCADE, 00017/00029),
	// clearing them before newIntegrationPool's own Cleanup attempts its
	// migration rollback — 00032's down-migration hard-aborts while any
	// 's3'-stamped row lingers, mirroring
	// TestPhotoRepositoryCreateStampsConfiguredBackend's identical
	// precedent in postgres_test.go. Registered AFTER ctx's own
	// t.Cleanup(cancel) so it runs first (t.Cleanup is LIFO) while ctx is
	// still valid.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM household WHERE id = $1`, hh.String())
	})

	rowA := putLocalAndCreateAlbumRow(ctx, t, local, photos, hh, testJPEGBytes(t, 10))
	rowB := putLocalAndCreateAlbumRow(ctx, t, local, photos, hh, testJPEGBytes(t, 20))

	migrator, err := newPhotoMigrator(local, s3Store, mediadomain.StorageBackendS3, photos, choreProofPhotos, 10<<20, nil)
	if err != nil {
		t.Fatalf("newPhotoMigrator: %v", err)
	}

	// AC1: first pass migrates both rows.
	first, err := migrator.Migrate(ctx, migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}})
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if first.Classes[0].Migrated != 2 {
		t.Fatalf("first Migrate: Migrated = %d, want 2", first.Classes[0].Migrated)
	}
	rowAAfter, err := photos.Get(ctx, rowA.ID)
	if err != nil {
		t.Fatalf("Get rowA after migrate: %v", err)
	}
	if rowAAfter.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatalf("rowA StorageBackend = %q, want s3", rowAAfter.StorageBackend)
	}
	if _, err := s3Store.Open(ctx, rowAAfter.StorageRef); err != nil {
		t.Fatalf("s3 object for rowA not found after migrate: %v", err)
	}

	// AC1 (resume/idempotency): re-running finds nothing left to migrate,
	// and does not error or duplicate anything.
	second, err := migrator.Migrate(ctx, migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}})
	if err != nil {
		t.Fatalf("second (resume) Migrate: %v", err)
	}
	if second.Classes[0].Migrated != 0 {
		t.Fatalf("second Migrate: Migrated = %d, want 0 (already migrated)", second.Classes[0].Migrated)
	}

	// AC3: clean verify reports no data loss.
	v, err := newVerifier(local, s3Store, photos, choreProofPhotos)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	cleanResult, err := v.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify (clean): %v", err)
	}
	if cleanResult.HasDataLoss() {
		t.Fatalf("Verify (clean) reported data loss: %+v", cleanResult)
	}

	// AC3: delete rowB's object directly from the bucket (simulating an
	// operator/bug deleting it out-of-band) — verify must catch it.
	if err := s3Store.Delete(ctx, rowB.StorageRef); err != nil {
		t.Fatalf("delete rowB's object directly: %v", err)
	}
	dirtyResult, err := v.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify (after deleting an object): %v", err)
	}
	if !dirtyResult.HasDataLoss() {
		t.Fatal("Verify after deleting an object should report data loss")
	}
	rowBAfter, err := photos.Get(ctx, rowB.ID)
	if err != nil {
		t.Fatalf("Get rowB: %v", err)
	}
	foundRowB := false
	for _, c := range dirtyResult.S3 {
		if c.Class != mediadomain.PhotoClassAlbum {
			continue
		}
		for _, ref := range c.RowsWithoutObject {
			if ref == rowBAfter.StorageRef {
				foundRowB = true
			}
		}
	}
	if !foundRowB {
		t.Fatalf("Verify did not report rowB's ref %s in RowsWithoutObject: %+v", rowBAfter.StorageRef, dirtyResult.S3)
	}

	// AC2: a fresh row whose local bytes no longer match its recorded hash
	// must abort its flip and be reported, while the migrator keeps going.
	corrupt := putLocalAndCreateAlbumRow(ctx, t, local, photos, hh, testJPEGBytes(t, 30))
	// Overwrite the local file's bytes directly on disk (bypassing Put's
	// validation, which a genuine corruption event obviously would too) to
	// simulate on-disk corruption between original upload and migration.
	overwriteLocalFile(t, localRoot, corrupt.StorageRef, []byte("CORRUPTED — different bytes entirely, but still starts like a JPEG"))
	good := putLocalAndCreateAlbumRow(ctx, t, local, photos, hh, testJPEGBytes(t, 40))

	mismatchResult, err := migrator.Migrate(ctx, migrateOptions{Classes: []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum}})
	if err != nil {
		t.Fatalf("Migrate (with a corrupted row): %v", err)
	}
	if mismatchResult.Classes[0].HashMismatches != 1 {
		t.Fatalf("HashMismatches = %d, want 1", mismatchResult.Classes[0].HashMismatches)
	}
	if mismatchResult.Classes[0].Migrated != 1 {
		t.Fatalf("Migrated = %d, want 1 (the migrator must continue past the mismatch)", mismatchResult.Classes[0].Migrated)
	}
	corruptAfter, err := photos.Get(ctx, corrupt.ID)
	if err != nil {
		t.Fatalf("Get corrupt row: %v", err)
	}
	if corruptAfter.StorageBackend != mediadomain.StorageBackendLocal {
		t.Fatalf("corrupt row's StorageBackend = %q, want local (flip must be aborted)", corruptAfter.StorageBackend)
	}
	goodAfter, err := photos.Get(ctx, good.ID)
	if err != nil {
		t.Fatalf("Get good row: %v", err)
	}
	if goodAfter.StorageBackend != mediadomain.StorageBackendS3 {
		t.Fatal("the uncorrupted row must still migrate despite the sibling mismatch")
	}

	// AC4: --delete-local removes a row's local file once verified.
	solo := putLocalAndCreateAlbumRow(ctx, t, local, photos, hh, testJPEGBytes(t, 50))
	soloOriginalRef := solo.StorageRef
	deleteResult, err := migrator.Migrate(ctx, migrateOptions{
		Classes:     []mediadomain.PhotoClass{mediadomain.PhotoClassAlbum},
		DeleteLocal: true,
	})
	if err != nil {
		t.Fatalf("Migrate (--delete-local): %v", err)
	}
	if deleteResult.Classes[0].DeletedLocal < 1 {
		t.Fatalf("DeletedLocal = %d, want at least 1", deleteResult.Classes[0].DeletedLocal)
	}
	if _, err := local.Open(ctx, soloOriginalRef); err == nil {
		t.Fatal("solo photo's local file should have been deleted after --delete-local")
	}
}

// overwriteLocalFile writes data directly to ref's on-disk path under root,
// bypassing LocalPhotoStore's public API entirely — used only to simulate
// local file corruption AFTER a legitimate upload already assigned that
// ref (a genuine corruption event would look exactly like this: the bytes
// on disk changing out from under an unchanged content_sha256 row). This
// is a test-only helper in cmd/storage, not adapter code — the hexagonal
// boundary test (internal/media/adapter/hexagonal_boundary_test.go) scans
// only internal/media/domain and internal/media/app, neither of which this
// file is part of.
func overwriteLocalFile(t *testing.T, root string, ref mediadomain.StorageRef, data []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(ref.String()))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("overwrite local file %s: %v", path, err)
	}
}
