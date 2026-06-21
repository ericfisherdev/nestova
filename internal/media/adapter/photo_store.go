package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"

	// Image format decoders registered for image.DecodeConfig: jpeg/png from the
	// standard library, webp from x/image (its init registers the format).
	_ "image/jpeg"
	_ "image/png"

	// webp decoder registered for image.DecodeConfig (the package init registers it).
	_ "golang.org/x/image/webp"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// acceptedTypes maps an accepted upload content type to the file extension used
// for the stored object. Anything else is rejected as ErrUnsupportedMediaType.
var acceptedTypes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
}

// formatToType maps an image.DecodeConfig format name back to its content type,
// so the declared type can be cross-checked against the actual bytes.
var formatToType = map[string]string{
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"webp": "image/webp",
}

// LocalPhotoStore is a domain.PhotoStore backed by the local filesystem. Photos
// are content-addressed (sha256) under MEDIA_ROOT/<household>/<aa>/<hash>.<ext>,
// so identical uploads de-duplicate and a ref never collides across households.
// It is constructor-injected and swappable for an object-store adapter later.
type LocalPhotoStore struct {
	root           string
	maxUploadBytes int64
}

// NewLocalPhotoStore returns a store rooted at root (created if missing),
// rejecting a blank root or a non-positive size cap.
func NewLocalPhotoStore(root string, maxUploadBytes int64) (*LocalPhotoStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("media/adapter: photo store root must not be blank")
	}
	if maxUploadBytes <= 0 {
		return nil, fmt.Errorf("media/adapter: max upload bytes must be positive, got %d", maxUploadBytes)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("media/adapter: create photo store root: %w", err)
	}
	return &LocalPhotoStore{root: root, maxUploadBytes: maxUploadBytes}, nil
}

// Put validates and stores the upload, returning the StorageRef to persist on the
// Photo. The declared contentType must be an accepted image type and the bytes
// must actually decode as that type — the bytes, not the client's claim, are
// authoritative.
func (s *LocalPhotoStore) Put(_ context.Context, householdID household.HouseholdID, data []byte, contentType string) (domain.StorageRef, error) {
	if int64(len(data)) > s.maxUploadBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", domain.ErrPhotoTooLarge, len(data), s.maxUploadBytes)
	}
	ext, ok := acceptedTypes[canonicalType(contentType)]
	if !ok {
		return "", fmt.Errorf("%w: %q", domain.ErrUnsupportedMediaType, contentType)
	}
	// The bytes must be a real image of the declared type; this rejects a spoofed
	// content type or a corrupt/empty payload.
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || formatToType[format] != canonicalType(contentType) {
		return "", fmt.Errorf("%w: bytes are not a valid %s image", domain.ErrInvalidPhoto, canonicalType(contentType))
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	ref := filepath.ToSlash(filepath.Join(householdID.String(), hash[:2], hash+"."+ext))

	full, err := s.resolve(ref)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("media/adapter: create photo dir: %w", err)
	}
	// Content-addressed, so re-storing identical bytes is a harmless overwrite.
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", fmt.Errorf("media/adapter: write photo: %w", err)
	}
	return domain.StorageRef(ref), nil
}

// Open streams a stored photo's bytes; ErrPhotoNotFound when the ref is unknown.
func (s *LocalPhotoStore) Open(_ context.Context, ref domain.StorageRef) (io.ReadCloser, error) {
	full, err := s.resolve(ref.String())
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", domain.ErrPhotoNotFound, ref)
		}
		return nil, fmt.Errorf("media/adapter: open photo: %w", err)
	}
	return f, nil
}

// Delete removes a stored photo; a missing file is not an error (idempotent).
func (s *LocalPhotoStore) Delete(_ context.Context, ref domain.StorageRef) error {
	full, err := s.resolve(ref.String())
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("media/adapter: delete photo: %w", err)
	}
	return nil
}

// resolve joins ref onto root and guards against path traversal: the cleaned
// absolute path must stay within root.
func (s *LocalPhotoStore) resolve(ref string) (string, error) {
	full := filepath.Join(s.root, filepath.FromSlash(ref))
	rootAbs, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("media/adapter: resolve root: %w", err)
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("media/adapter: resolve ref: %w", err)
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: storage ref escapes the store root", domain.ErrInvalidPhoto)
	}
	return full, nil
}

// canonicalType lowercases a content type and strips any parameters (e.g.
// "image/jpeg; charset=binary" -> "image/jpeg").
func canonicalType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}
