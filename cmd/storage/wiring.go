package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediabootstrap "github.com/ericfisherdev/nestova/internal/media/bootstrap"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
	"github.com/ericfisherdev/nestova/internal/platform/db"
)

// wireMediaStorageInitTimeout bounds the S3 backend's startup HeadBucket
// reachability check (see mediaadapter.NewS3PhotoStore's doc), mirroring
// cmd/server's identical photoStoreInitTimeout.
const wireMediaStorageInitTimeout = 10 * time.Second

// mediaWiring is every dependency migrate/verify/reap need: the DB pool
// (closed by the caller), both concrete PhotoStore backends, and both
// repositories.
type mediaWiring struct {
	pool             *pgxpool.Pool
	localStore       mediadomain.PhotoStore
	targetStore      mediadomain.PhotoStore
	targetBackend    mediadomain.StorageBackend
	photos           mediadomain.PhotoRepository
	choreProofPhotos mediadomain.TaskInstancePhotoRepository
}

// wireMediaStorage builds a mediaWiring from cfg, the same way cmd/server's
// composition root does (mediabootstrap.NewPhotoStoreResolver + the
// pgx-backed repositories), with one additional requirement specific to
// this binary: MEDIA_STORAGE_BACKEND=s3 (plus the S3_* settings) must
// already be set in the environment.
//
// This is NOT optional convenience — it is how config.Load itself behaves:
// every S3_* setting's VALIDATION (and PresignTTL/UsePathStyle's very
// PARSING) is gated on cfg.Media.Backend == s3 (see config.go's "resolve
// the media storage backend BEFORE any S3-specific parsing" comment), so
// running this tool with MEDIA_STORAGE_BACKEND still "local" would silently
// construct an S3 client with UsePathStyle forced false regardless of
// S3_USE_PATH_STYLE — breaking MinIO/Garage in a way that is hard to
// diagnose. Failing fast here, with an actionable message, is far better
// than that silent breakage. See docs/storage.md's runbook: the operator
// sets MEDIA_STORAGE_BACKEND=s3 (and the server may already be running with
// it — reads still work for historical local rows via the resolver's
// mixed-state support, see domain.PhotoStoreResolver's doc) BEFORE running
// `storage migrate`/`storage verify`/`storage reap`.
func wireMediaStorage(ctx context.Context, cfg config.Config) (*mediaWiring, error) {
	if cfg.Media.Backend != config.MediaStorageBackendS3 {
		return nil, fmt.Errorf(
			"storage: MEDIA_STORAGE_BACKEND=s3 (plus S3_BUCKET, S3_REGION, and any other required S3_* settings) must be set in the environment to run this command; see docs/storage.md")
	}

	pool, err := db.New(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}

	storeCtx, cancel := context.WithTimeout(ctx, wireMediaStorageInitTimeout)
	defer cancel()
	resolver, writeBackend, err := mediabootstrap.NewPhotoStoreResolver(storeCtx, cfg.Media)
	if err != nil {
		pool.Close()
		return nil, err
	}
	localStore, err := resolver.Resolve(mediadomain.StorageBackendLocal)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("resolve local photo store: %w", err)
	}
	targetStore, err := resolver.Resolve(mediadomain.StorageBackendS3)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("resolve s3 photo store: %w", err)
	}

	return &mediaWiring{
		pool:             pool,
		localStore:       localStore,
		targetStore:      targetStore,
		targetBackend:    writeBackend,
		photos:           mediaadapter.NewPhotoRepository(pool, writeBackend),
		choreProofPhotos: mediaadapter.NewTaskInstancePhotoRepository(pool, writeBackend),
	}, nil
}
