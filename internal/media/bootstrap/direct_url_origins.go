package bootstrap

import (
	"context"
	"errors"
	"fmt"

	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
)

// directURLOriginer is implemented by a PhotoStore whose URL method returns a
// link on another origin (mediaadapter.S3PhotoStore). It is declared here, by
// its only consumer, so the domain PhotoStore port stays free of a web-policy
// concern.
type directURLOriginer interface {
	DirectURLOrigin(ctx context.Context) (string, error)
}

// DirectURLOrigins lists the origins the browser is redirected to for photo
// bytes, so the HTTP layer can allow them as image sources (NES-212): with
// MEDIA_STORAGE_BACKEND=s3, /photos/{id}/raw answers with a 302 to a presigned
// URL on the S3 endpoint, and a Content-Security-Policy that only allows
// 'self' blocks the redirected image.
//
// It returns nil when the deployment has no store that redirects, which is
// the local-only case. The empty result is not an error.
//
// Errors: a wrapped error when a redirecting store cannot report its origin.
// It never returns mediadomain.ErrStoreNotConfigured; that means "no S3 store",
// which is the nil result above.
func DirectURLOrigins(ctx context.Context, resolver mediadomain.PhotoStoreResolver) ([]string, error) {
	store, err := resolver.Resolve(mediadomain.StorageBackendS3)
	if errors.Is(err, mediadomain.ErrStoreNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve s3 photo store: %w", err)
	}
	originer, ok := store.(directURLOriginer)
	if !ok {
		return nil, nil
	}
	origin, err := originer.DirectURLOrigin(ctx)
	if err != nil {
		return nil, fmt.Errorf("find s3 photo origin: %w", err)
	}
	return []string{origin}, nil
}
