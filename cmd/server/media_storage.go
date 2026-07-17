package main

import (
	"context"
	"fmt"

	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/config"
)

// newPhotoStore selects and constructs the domain.PhotoStore backend
// (NES-132) from mediaCfg.Backend — the one place in the composition root
// that knows both concrete adapters exist; every other consumer (the media
// services, the web handlers) depends on the domain.PhotoStore interface
// alone. ctx bounds the S3 backend's startup HeadBucket reachability check
// (see mediaadapter.NewS3PhotoStore's doc); the local backend ignores it,
// since creating a directory has no meaningful cancellation point.
func newPhotoStore(ctx context.Context, mediaCfg config.MediaConfig) (mediadomain.PhotoStore, error) {
	switch mediaCfg.Backend {
	case config.MediaStorageBackendLocal:
		return mediaadapter.NewLocalPhotoStore(mediaCfg.Root, mediaCfg.MaxUploadBytes)
	case config.MediaStorageBackendS3:
		return mediaadapter.NewS3PhotoStore(ctx, mediaadapter.S3Params{
			Endpoint:        mediaCfg.S3.Endpoint,
			Region:          mediaCfg.S3.Region,
			Bucket:          mediaCfg.S3.Bucket,
			AccessKeyID:     mediaCfg.S3.AccessKeyID,
			SecretAccessKey: mediaCfg.S3.SecretAccessKey,
			UsePathStyle:    mediaCfg.S3.UsePathStyle,
			PresignTTL:      mediaCfg.S3.PresignTTL,
			MaxUploadBytes:  mediaCfg.MaxUploadBytes,
		})
	default:
		// config.Load's validate() already rejects an unknown backend at
		// startup, so this is unreachable in practice — kept as a loud
		// failure (not a silent default) rather than trusting that
		// invariant blindly here too.
		return nil, fmt.Errorf("media: unknown storage backend %q", mediaCfg.Backend)
	}
}

// mediaStorageBackend converts the config-owned MediaStorageBackend into the
// media/domain.StorageBackend both PhotoRepository and
// TaskInstancePhotoRepository are constructed with (NES-132), so every row
// either repository writes is stamped with whichever backend is genuinely
// active — see PhotoRepository.Create's doc. The two types share the same
// underlying string values ("local"/"s3") by construction; config.Load's
// own validate() already rejects anything else at startup, so this
// conversion cannot fail — kept as a named function (not an inline cast at
// each call site) purely so both repositories are unmistakably built from
// the SAME source of truth as newPhotoStore's own backend switch above.
func mediaStorageBackend(backend config.MediaStorageBackend) mediadomain.StorageBackend {
	return mediadomain.StorageBackend(backend)
}
