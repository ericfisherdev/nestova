package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"path"
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

// photoBytesCacheControl is the Cache-Control applied to an actual photo's
// bytes wherever they are served or stored — private (a photo is
// household-scoped, so a shared/proxy cache must never store it) with a
// one-hour freshness window a client may cache locally. Shared by
// WebHandlers.Raw/ChoreProofWebHandlers.Raw's streamed-body branch (a local
// backend) and S3PhotoStore.Put/URL (baked into the object at upload time
// via PutObjectInput.CacheControl, and reasserted on each presigned GET via
// ResponseCacheControl so it doesn't fall back to whatever permissive
// default the object store would otherwise apply) — one literal, not a
// value duplicated across three call sites that could drift. Deliberately
// distinct from the 302 REDIRECT response's own Cache-Control
// ("private, no-store", set directly in the two Raw handlers): the redirect
// response carries a short-lived bearer credential and must never be
// cached at all, whereas the underlying image bytes it points at are safe
// to cache privately for an hour, matching the local-backend streaming
// behavior exactly.
const photoBytesCacheControl = "private, max-age=3600"

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

// sniffLen is how many leading bytes validateAndStage reads to determine an
// upload's true content type via http.DetectContentType — the client's
// declared Content-Type is never consulted, so a renamed or mislabeled file
// cannot slip past it.
const sniffLen = 512

// tempFilePattern names the staging file validateAndStage streams an upload
// into before it is hashed, validated, and handed back to the caller.
const tempFilePattern = "upload-*.tmp"

// maxDecodePixels bounds width*height for the full image.Decode below,
// checked against image.DecodeConfig's header-only dimensions before that
// decode allocates a full pixel buffer — a guard against a decompression bomb
// (a small file whose header claims enormous dimensions). 50 megapixels
// comfortably covers real camera photos (a 45 MP sensor is toward the high
// end of current consumer hardware) while bounding worst-case decode memory.
const maxDecodePixels = 50_000_000

// stagedUpload is the outcome of validateAndStage: a temp file on disk
// (Path) holding the complete, validated upload, plus the facts computed
// while streaming it there. The caller owns Path — either renaming it into
// its final content-addressed home (LocalPhotoStore.Put) or uploading it and
// discarding it (S3PhotoStore.Put) — and must call removeStaged(Path) once
// done with it (both callers defer this; a successful rename is a no-op
// remove, since the file no longer exists at Path).
type stagedUpload struct {
	Path        string
	ContentType string
	ContentHash string
	SizeBytes   int64
}

// validateAndStage streams r into a new temp file under dir, applying the
// SAME validation every PhotoStore backend must guarantee regardless of
// where bytes ultimately land: sniffs the true content type from the first
// sniffLen bytes (never a caller-supplied claim), enforces maxUploadBytes
// while copying, and cross-validates that the bytes actually decode as the
// sniffed type (rejecting a corrupt/truncated payload or one whose magic
// bytes lied about its format) — extracted here so LocalPhotoStore and
// S3PhotoStore (NES-132) share one implementation of "what makes an upload
// acceptable" rather than each maintaining its own copy that could drift.
//
// On any rejection the staging file is removed and the returned stagedUpload
// is the zero value; on success the caller is responsible for removeStaged
// once it is done with the file (see stagedUpload's doc).
func validateAndStage(dir string, maxUploadBytes int64, r io.Reader) (stagedUpload, error) {
	// Caps total bytes read (sniff + copy) at maxUploadBytes+1: enough to detect
	// an oversize upload without ever buffering more than that in flight.
	limited := io.LimitReader(r, maxUploadBytes+1)

	sniff := make([]byte, sniffLen)
	n, err := io.ReadFull(limited, sniff)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return stagedUpload{}, fmt.Errorf("media/adapter: read upload: %w", err)
	}
	sniff = sniff[:n]
	sniffedType := canonicalType(http.DetectContentType(sniff))
	if _, ok := acceptedTypes[sniffedType]; !ok {
		return stagedUpload{}, fmt.Errorf("%w: %q", domain.ErrUnsupportedMediaType, sniffedType)
	}

	tmp, err := os.CreateTemp(dir, tempFilePattern)
	if err != nil {
		return stagedUpload{}, fmt.Errorf("media/adapter: create staging file: %w", err)
	}
	tmpPath := tmp.Name()
	staged := false
	defer func() {
		_ = tmp.Close()
		if !staged {
			removeStaged(tmpPath)
		}
	}()

	hasher := sha256.New()
	// Replay the already-consumed sniff prefix ahead of whatever remains on
	// limited, so the full stream (not just the tail after sniffing) reaches
	// both the staging file and the hasher.
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.MultiReader(bytes.NewReader(sniff), limited))
	if err != nil {
		return stagedUpload{}, fmt.Errorf("media/adapter: write upload: %w", err)
	}
	if written > maxUploadBytes {
		return stagedUpload{}, fmt.Errorf("%w: %d bytes exceeds the %d-byte limit", domain.ErrPhotoTooLarge, written, maxUploadBytes)
	}

	// The bytes must actually decode as the sniffed type; this rejects a
	// corrupt/empty payload or content whose magic bytes lied about its format.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return stagedUpload{}, fmt.Errorf("media/adapter: seek staged upload: %w", err)
	}
	cfg, format, err := image.DecodeConfig(tmp)
	if err != nil || formatToType[format] != sniffedType {
		return stagedUpload{}, fmt.Errorf("%w: bytes are not a valid %s image", domain.ErrInvalidPhoto, sniffedType)
	}
	// Check claimed dimensions before the full decode below allocates a pixel
	// buffer sized to them — a small file can still claim an enormous width/
	// height (a decompression bomb), and DecodeConfig alone never allocates
	// enough to reveal that.
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxDecodePixels {
		return stagedUpload{}, fmt.Errorf("%w: image is %dx%d (%d pixels), exceeds the %d-pixel limit", domain.ErrInvalidPhoto, cfg.Width, cfg.Height, pixels, maxDecodePixels)
	}
	// DecodeConfig only reads the header, so a file truncated partway through
	// its entropy-coded image data can still pass it; a full Decode is the only
	// way to catch that.
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return stagedUpload{}, fmt.Errorf("media/adapter: seek staged upload: %w", err)
	}
	if _, _, err := image.Decode(tmp); err != nil {
		return stagedUpload{}, fmt.Errorf("%w: image data is truncated or corrupt", domain.ErrInvalidPhoto)
	}

	staged = true
	return stagedUpload{
		Path:        tmpPath,
		ContentType: sniffedType,
		ContentHash: hex.EncodeToString(hasher.Sum(nil)),
		SizeBytes:   written,
	}, nil
}

// removeStaged discards a staged upload's temp file. Idempotent: a caller
// that has already renamed the file away (LocalPhotoStore.Put) or otherwise
// removed it may call this again harmlessly, since os.Remove on a missing
// path is silently ignored here.
func removeStaged(path string) {
	_ = os.Remove(path)
}

// classKeyPrefix maps a domain.PhotoClass to the literal path segment every
// PhotoStore backend namespaces its keys under (see buildStorageKey), so one
// class's bytes can never land under, or be confused with, another class's
// prefix. An exhaustive switch — never a bare map or a free-form string
// passed through — means an unrecognized PhotoClass fails Put loudly instead
// of silently defaulting into some namespace.
func classKeyPrefix(class domain.PhotoClass) (string, error) {
	switch class {
	case domain.PhotoClassAlbum:
		return "photos", nil
	case domain.PhotoClassChoreProof:
		return "chore-photos", nil
	case domain.PhotoClassRewardImage:
		return "reward-images", nil
	default:
		return "", fmt.Errorf("media/adapter: unknown photo class %d", class)
	}
}

// buildStorageKey builds the class-namespaced, content-addressed key every
// PhotoStore backend uses — households/<household>/<class-prefix>/<aa>/
// <hash>.<ext> — shared by LocalPhotoStore (where it becomes a relative
// filesystem path) and S3PhotoStore (where it becomes an object key
// verbatim) so the key layout can never drift between backends. Built with
// "path".Join (forward slashes only, never the OS path separator) since an
// S3 key is never OS-path-shaped; LocalPhotoStore's resolve converts to the
// OS separator only at the point it actually touches the filesystem.
func buildStorageKey(householdID household.HouseholdID, class domain.PhotoClass, sum, ext string) (string, error) {
	classPrefix, err := classKeyPrefix(class)
	if err != nil {
		return "", err
	}
	return path.Join("households", householdID.String(), classPrefix, sum[:2], sum+"."+ext), nil
}

// canonicalType lowercases a content type and strips any parameters (e.g.
// "image/jpeg; charset=binary" -> "image/jpeg").
func canonicalType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}
