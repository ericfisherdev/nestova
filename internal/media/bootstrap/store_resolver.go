// Package bootstrap builds the media bounded context's composition-root-only
// dependencies — today, just the PhotoStore resolver/write-backend pair
// (NewPhotoStoreResolver) — so every binary that needs a fully-wired
// domain.PhotoStoreResolver (cmd/server and, since NES-133, cmd/storage)
// constructs it identically from the same config.MediaConfig, instead of
// each maintaining its own copy of the local/S3 selection logic. It is
// deliberately its own package, not folded into internal/media/adapter:
// adapter's own doc (see S3Params) states that package never depends on
// internal/platform/config, so a config-consuming builder belongs in a
// peer package instead.
package bootstrap

import (
	"context"
	"fmt"

	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// NewPhotoStoreResolver builds the domain.PhotoStoreResolver every media
// service reads through, plus the write-target StorageBackend Upload writes
// new photos to (NES-132 mixed-state fix — see domain.PhotoStoreResolver's
// doc for the full rationale). This is the one place in either binary's
// composition root that knows both concrete PhotoStore adapters exist;
// every other consumer depends on the resolver/domain.PhotoStore interfaces
// alone.
//
// The LOCAL store is ALWAYS constructed, regardless of mediaCfg.Backend: it
// has a safe, zero-required-config default (see LocalPhotoStore's doc), and
// constructing it unconditionally is what keeps historical local-backed
// rows readable after an operator switches MEDIA_STORAGE_BACKEND to s3 —
// the resolver can still route THOSE rows' reads to it, and NES-133's
// `storage migrate`/`storage verify` commands need direct access to it
// regardless of which backend is currently configured for new writes. The
// S3 store is constructed ONLY when s3 is actually selected (mediaCfg.Backend
// == config.MediaStorageBackendS3), since it requires real bucket/credential
// configuration a local-only deployment should never be forced to provide
// (see config.go's S3-gating fix) — this is also why NES-133's storage
// tooling requires MEDIA_STORAGE_BACKEND=s3 (plus the S3_* settings) to be
// set in its environment before `storage migrate`/`storage verify`/`storage
// reap` can do anything useful: without it, this function never registers
// an S3 store at all, and every S3-dependent operation fails fast with
// domain.ErrStoreNotConfigured rather than silently doing nothing.
//
// ctx bounds the S3 backend's startup HeadBucket reachability check (see
// mediaadapter.NewS3PhotoStore's doc) — the caller is expected to derive it
// with a bounded timeout so a network-unreachable S3 endpoint fails the
// boot promptly instead of hanging indefinitely; the local store's
// construction has no meaningful cancellation point and ignores ctx
// entirely.
func NewPhotoStoreResolver(ctx context.Context, mediaCfg config.MediaConfig) (mediadomain.PhotoStoreResolver, mediadomain.StorageBackend, error) {
	localStore, err := mediaadapter.NewLocalPhotoStore(mediaCfg.Root, mediaCfg.MaxUploadBytes)
	if err != nil {
		return nil, "", fmt.Errorf("create local photo store: %w", err)
	}
	stores := map[mediadomain.StorageBackend]mediadomain.PhotoStore{
		mediadomain.StorageBackendLocal: localStore,
	}

	writeBackend := StorageBackend(mediaCfg.Backend)
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

// StorageBackend converts the config-owned MediaStorageBackend into the
// media/domain.StorageBackend both PhotoRepository and
// TaskInstancePhotoRepository are constructed with, and that
// NewPhotoStoreResolver returns as the write target (NES-132), so every row
// either repository writes — and every new upload — is stamped with/sent
// to whichever backend is genuinely active. The two types share the same
// underlying string values ("local"/"s3") by construction; config.Load's
// own validate() already rejects anything else at startup, so this
// conversion cannot fail — kept as a named function (not an inline cast at
// each call site) purely so every consumer is unmistakably built from the
// SAME source of truth as NewPhotoStoreResolver's own backend switch above.
func StorageBackend(backend config.MediaStorageBackend) mediadomain.StorageBackend {
	return mediadomain.StorageBackend(backend)
}
