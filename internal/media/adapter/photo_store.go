package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// LocalPhotoStore is a domain.PhotoStore backed by the local filesystem.
// Photos are content-addressed (sha256) under
// MEDIA_ROOT/households/<household>/<class-prefix>/<aa>/<hash>.<ext> (see
// classKeyPrefix for the class-prefix values), so identical bytes uploaded
// for the same domain.PhotoClass de-duplicate on disk, a ref never collides
// across households, and — the reason the class segment exists — bytes
// uploaded under one class can never collide with, or be resolved as, another
// class's bytes even if the content happens to be byte-identical (e.g. a
// chore-proof photo can never land under an album's key space). Refs
// predating this class segment (a bare <household>/<aa>/<hash>.<ext>, no
// "households/" or class prefix) remain servable: Open and Delete treat ref
// as an opaque relative path and never assume its shape, so a legacy ref
// resolves exactly as it always did.
//
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

// Put streams r to a staging file under root via validateAndStage — never
// buffering the whole upload in memory, sniffing the true content type from
// the bytes themselves (the caller never supplies one; it is not trusted),
// cross-validating that the bytes actually decode as that type — then
// atomically renames the staged file into its content-addressed,
// class-namespaced home (see buildStorageKey). Any rejection — including an
// unrecognized class — removes the staging file, leaving no partial upload
// behind.
func (s *LocalPhotoStore) Put(_ context.Context, householdID household.HouseholdID, class domain.PhotoClass, r io.Reader) (domain.PutResult, error) {
	if !class.Valid() {
		return domain.PutResult{}, fmt.Errorf("media/adapter: unknown photo class %d", class)
	}
	staged, err := validateAndStage(s.root, s.maxUploadBytes, r)
	if err != nil {
		return domain.PutResult{}, err
	}
	renamed := false
	defer func() {
		if !renamed {
			removeStaged(staged.Path)
		}
	}()

	ref, err := buildStorageKey(householdID, class, staged.ContentHash, acceptedTypes[staged.ContentType])
	if err != nil {
		return domain.PutResult{}, err
	}
	full, err := s.resolve(ref)
	if err != nil {
		return domain.PutResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: create photo dir: %w", err)
	}
	// Content-addressed, so a file already at full is byte-identical; Rename is
	// atomic and harmlessly replaces it.
	if err := os.Rename(staged.Path, full); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: finalize upload: %w", err)
	}
	renamed = true

	return domain.PutResult{
		Ref: domain.StorageRef(ref), ContentHash: staged.ContentHash,
		SizeBytes: staged.SizeBytes, ContentType: staged.ContentType,
	}, nil
}

// Open streams a stored photo's bytes; ErrPhotoNotFound when the ref is
// unknown. The returned *os.File natively satisfies domain.PhotoReader (Read,
// ReadAt, Seek, Close), which lets EXIF extraction read directly from disk
// instead of first buffering the file into memory.
func (s *LocalPhotoStore) Open(_ context.Context, ref domain.StorageRef) (domain.PhotoReader, error) {
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

// URL confirms ref resolves to a stored object and returns ref's own string
// as a stable, non-navigable locator, or ErrPhotoNotFound when ref is
// unknown; ttl is ignored (see the domain.PhotoStore.URL doc for why a local
// backend cannot honestly return a browser-navigable URL from ref alone, and
// why that is fine — no caller uses this today).
func (s *LocalPhotoStore) URL(_ context.Context, ref domain.StorageRef, _ time.Duration) (string, error) {
	full, err := s.resolve(ref.String())
	if err != nil {
		return "", err
	}
	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", domain.ErrPhotoNotFound, ref)
		}
		return "", fmt.Errorf("media/adapter: stat photo: %w", err)
	}
	// A directory (e.g. ref "households") is not a stored photo.
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", domain.ErrPhotoNotFound, ref)
	}
	return ref.String(), nil
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

// SupportsDirectURL always reports false: LocalPhotoStore's URL never
// returns a browser-navigable locator (see URL's own doc), so no caller may
// redirect a client to it.
func (s *LocalPhotoStore) SupportsDirectURL() bool { return false }
