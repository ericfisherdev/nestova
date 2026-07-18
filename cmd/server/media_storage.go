package main

import (
	"context"
	"fmt"

	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// newPhotoStoreResolver builds the domain.PhotoStoreResolver every media
// service reads through, plus the write-target StorageBackend Upload writes
// new photos to (NES-132 mixed-state fix — see domain.PhotoStoreResolver's
// doc for the full rationale). This is the one place in the composition
// root that knows both concrete PhotoStore adapters exist; every other
// consumer depends on the resolver/domain.PhotoStore interfaces alone.
//
// The LOCAL store is ALWAYS constructed, regardless of mediaCfg.Backend: it
// has a safe, zero-required-config default (see LocalPhotoStore's doc), and
// constructing it unconditionally is what keeps historical local-backed
// rows readable after an operator switches MEDIA_STORAGE_BACKEND to s3 —
// the resolver can still route THOSE rows' reads to it. The S3 store is
// constructed ONLY when s3 is actually selected, since it requires real
// bucket/credential configuration a local-only deployment should never be
// forced to provide (see config.go's S3-gating fix).
//
// ctx bounds the S3 backend's startup HeadBucket reachability check (see
// mediaadapter.NewS3PhotoStore's doc) — the caller is expected to derive it
// with a bounded timeout (main.go uses ~10s) so a network-unreachable S3
// endpoint fails the boot promptly instead of hanging indefinitely; the
// local store's construction has no meaningful cancellation point and
// ignores ctx entirely.
func newPhotoStoreResolver(ctx context.Context, mediaCfg config.MediaConfig) (mediadomain.PhotoStoreResolver, mediadomain.StorageBackend, error) {
	localStore, err := mediaadapter.NewLocalPhotoStore(mediaCfg.Root, mediaCfg.MaxUploadBytes)
	if err != nil {
		return nil, "", fmt.Errorf("create local photo store: %w", err)
	}
	stores := map[mediadomain.StorageBackend]mediadomain.PhotoStore{
		mediadomain.StorageBackendLocal: localStore,
	}

	writeBackend := mediaStorageBackend(mediaCfg.Backend)
	if mediaCfg.Backend == config.MediaStorageBackendS3 {
		s3Store, err := mediaadapter.NewS3PhotoStore(ctx, mediaadapter.S3Params{
			Endpoint:        mediaCfg.S3.Endpoint,
			Region:          mediaCfg.S3.Region,
			Bucket:          mediaCfg.S3.Bucket,
			AccessKeyID:     mediaCfg.S3.AccessKeyID,
			SecretAccessKey: mediaCfg.S3.SecretAccessKey,
			UsePathStyle:    mediaCfg.S3.UsePathStyle,
			PresignTTL:      mediaCfg.S3.PresignTTL,
			MaxUploadBytes:  mediaCfg.MaxUploadBytes,
		})
		if err != nil {
			return nil, "", fmt.Errorf("create s3 photo store: %w", err)
		}
		stores[mediadomain.StorageBackendS3] = s3Store
	}

	return mediaadapter.NewStoreResolver(stores), writeBackend, nil
}

// mediaStorageBackend converts the config-owned MediaStorageBackend into the
// media/domain.StorageBackend both PhotoRepository and
// TaskInstancePhotoRepository are constructed with, and that
// newPhotoStoreResolver returns as the write target (NES-132), so every row
// either repository writes — and every new upload — is stamped with/sent
// to whichever backend is genuinely active. The two types share the same
// underlying string values ("local"/"s3") by construction; config.Load's
// own validate() already rejects anything else at startup, so this
// conversion cannot fail — kept as a named function (not an inline cast at
// each call site) purely so every consumer is unmistakably built from the
// SAME source of truth as newPhotoStoreResolver's own backend switch above.
func mediaStorageBackend(backend config.MediaStorageBackend) mediadomain.StorageBackend {
	return mediadomain.StorageBackend(backend)
}
