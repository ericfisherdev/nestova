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
	"net/http"
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

// acceptedTypes maps an accepted (server-sniffed) content type to the file
// extension used for the stored object. Anything else is rejected as
// ErrUnsupportedMediaType — this is also the HEIC/non-image rejection path,
// since HEIC and any other unlisted type simply is not a key in this map. Keys
// are domain.ContentType* — the same constants Photo.Validate checks against —
// so the accept-list has one source of truth across the domain and adapter.
var acceptedTypes = map[string]string{
	domain.ContentTypeJPEG: "jpg",
	domain.ContentTypePNG:  "png",
	domain.ContentTypeWebP: "webp",
}

// formatToType maps an image.DecodeConfig format name back to its content type,
// so the sniffed type can be cross-checked against the actual decoded bytes.
var formatToType = map[string]string{
	"jpeg": domain.ContentTypeJPEG,
	"png":  domain.ContentTypePNG,
	"webp": domain.ContentTypeWebP,
}

// sniffLen is how many leading bytes Put reads to determine the upload's true
// content type via http.DetectContentType — the client's declared Content-Type
// is never consulted, so a renamed or mislabeled file cannot slip past it.
const sniffLen = 512

// tempFilePattern names the staging file Put streams an upload into before it
// is hashed, validated, and atomically renamed into its content-addressed home.
const tempFilePattern = "upload-*.tmp"

// maxDecodePixels bounds width*height for the full image.Decode below,
// checked against image.DecodeConfig's header-only dimensions before that
// decode allocates a full pixel buffer — a guard against a decompression bomb
// (a small file whose header claims enormous dimensions). 50 megapixels
// comfortably covers real camera photos (a 45 MP sensor is toward the high
// end of current consumer hardware) while bounding worst-case decode memory.
const maxDecodePixels = 50_000_000

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

// Put streams r to a staging file under root — never buffering the whole
// upload in memory — while hashing it and enforcing the size cap, sniffs the
// true content type from the first sniffLen bytes (the caller never supplies
// one; it is not trusted), cross-validates that the bytes actually decode as
// that type, and atomically renames the staging file into its content-addressed
// home. Any rejection removes the staging file, leaving no partial upload
// behind.
func (s *LocalPhotoStore) Put(_ context.Context, householdID household.HouseholdID, r io.Reader) (domain.PutResult, error) {
	// Caps total bytes read (sniff + copy) at maxUploadBytes+1: enough to detect
	// an oversize upload without ever buffering more than that in flight.
	limited := io.LimitReader(r, s.maxUploadBytes+1)

	sniff := make([]byte, sniffLen)
	n, err := io.ReadFull(limited, sniff)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return domain.PutResult{}, fmt.Errorf("media/adapter: read upload: %w", err)
	}
	sniff = sniff[:n]
	sniffedType := canonicalType(http.DetectContentType(sniff))
	ext, ok := acceptedTypes[sniffedType]
	if !ok {
		return domain.PutResult{}, fmt.Errorf("%w: %q", domain.ErrUnsupportedMediaType, sniffedType)
	}

	tmp, err := os.CreateTemp(s.root, tempFilePattern)
	if err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: create staging file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		_ = tmp.Close()
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	// Replay the already-consumed sniff prefix ahead of whatever remains on
	// limited, so the full stream (not just the tail after sniffing) reaches
	// both the staging file and the hasher.
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.MultiReader(bytes.NewReader(sniff), limited))
	if err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: write upload: %w", err)
	}
	if written > s.maxUploadBytes {
		return domain.PutResult{}, fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", domain.ErrPhotoTooLarge, written, s.maxUploadBytes)
	}

	// The bytes must actually decode as the sniffed type; this rejects a
	// corrupt/empty payload or content whose magic bytes lied about its format.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: seek staged upload: %w", err)
	}
	cfg, format, err := image.DecodeConfig(tmp)
	if err != nil || formatToType[format] != sniffedType {
		return domain.PutResult{}, fmt.Errorf("%w: bytes are not a valid %s image", domain.ErrInvalidPhoto, sniffedType)
	}
	// Check claimed dimensions before the full decode below allocates a pixel
	// buffer sized to them — a small file can still claim an enormous width/
	// height (a decompression bomb), and DecodeConfig alone never allocates
	// enough to reveal that.
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxDecodePixels {
		return domain.PutResult{}, fmt.Errorf("%w: image is %dx%d (%d pixels), exceeds the %d-pixel limit", domain.ErrInvalidPhoto, cfg.Width, cfg.Height, pixels, maxDecodePixels)
	}
	// DecodeConfig only reads the header, so a file truncated partway through
	// its entropy-coded image data can still pass it; a full Decode is the only
	// way to catch that.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: seek staged upload: %w", err)
	}
	if _, _, err := image.Decode(tmp); err != nil {
		return domain.PutResult{}, fmt.Errorf("%w: image data is truncated or corrupt", domain.ErrInvalidPhoto)
	}

	sum := hex.EncodeToString(hasher.Sum(nil))
	ref := filepath.ToSlash(filepath.Join(householdID.String(), sum[:2], sum+"."+ext))
	full, err := s.resolve(ref)
	if err != nil {
		return domain.PutResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: create photo dir: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: close staged upload: %w", err)
	}
	// Content-addressed, so a file already at full is byte-identical; Rename is
	// atomic and harmlessly replaces it.
	if err := os.Rename(tmpPath, full); err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: finalize upload: %w", err)
	}
	renamed = true

	return domain.PutResult{Ref: domain.StorageRef(ref), ContentHash: sum, SizeBytes: written, ContentType: sniffedType}, nil
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
