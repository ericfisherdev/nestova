package adapter_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	return img
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage()); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(), nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// largeJPEGBytes builds a real, decodable w-by-h JPEG large enough (a few
// hundred KB or more, depending on dimensions) that reading it in a single
// buffered chunk would be obviously larger than any reasonable streaming
// chunk size — used to prove Put streams rather than buffers.
func largeJPEGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func newStore(t *testing.T, maxBytes int64) *adapter.LocalPhotoStore {
	t.Helper()
	s, err := adapter.NewLocalPhotoStore(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("NewLocalPhotoStore: %v", err)
	}
	return s
}

// maxChunkReader tracks the largest single read buffer the caller ever
// requested (len(p), not the bytes actually returned), so a test can assert
// the caller never tried to slurp the whole source in one shot. Tracking the
// requested size rather than the returned count matters: an io.ReadAll-style
// caller reveals its intent to buffer everything by asking for very large
// buffers as its internal buffer grows, regardless of how much the
// underlying source happens to have left to hand back on any given call — a
// check keyed on the returned byte count could pass by coincidence if the
// source's remaining data is small.
type maxChunkReader struct {
	r            io.Reader
	maxRequested int
}

func (m *maxChunkReader) Read(p []byte) (int, error) {
	if len(p) > m.maxRequested {
		m.maxRequested = len(p)
	}
	return m.r.Read(p)
}

func TestPutStoresAndOpensAndDeletes(t *testing.T) {
	s := newStore(t, 10<<20)
	hh := household.NewHouseholdID()
	want := pngBytes(t)

	result, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if result.Ref == "" {
		t.Fatal("Put returned an empty ref")
	}
	if result.ContentHash == "" {
		t.Fatal("Put returned an empty content hash")
	}
	if result.SizeBytes != int64(len(want)) {
		t.Fatalf("Put SizeBytes = %d, want %d", result.SizeBytes, len(want))
	}
	if result.ContentType != "image/png" {
		t.Fatalf("Put ContentType = %q, want image/png", result.ContentType)
	}

	rc, err := s.Open(context.Background(), result.Ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("Open returned %d bytes, want %d", len(got), len(want))
	}

	if err := s.Delete(context.Background(), result.Ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Delete is idempotent.
	if err := s.Delete(context.Background(), result.Ref); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := s.Open(context.Background(), result.Ref); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Open after delete error = %v, want ErrPhotoNotFound", err)
	}
}

func TestPutContentAddressedDeduplicates(t *testing.T) {
	s := newStore(t, 10<<20)
	hh := household.NewHouseholdID()
	data := jpegBytes(t)
	r1, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	r2, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if r1.Ref != r2.Ref {
		t.Fatalf("identical bytes produced different refs: %s vs %s", r1.Ref, r2.Ref)
	}
	if r1.ContentHash != r2.ContentHash {
		t.Fatalf("identical bytes produced different hashes: %s vs %s", r1.ContentHash, r2.ContentHash)
	}
}

func TestPutRejections(t *testing.T) {
	hh := household.NewHouseholdID()
	// Sniffs as image/jpeg (the magic prefix Put's http.DetectContentType call
	// keys on) but has no valid JPEG structure beyond that, so the
	// image.DecodeConfig cross-check must still reject it.
	sniffsAsJPEGButUndecodable := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
	cases := []struct {
		name    string
		store   *adapter.LocalPhotoStore
		data    []byte
		wantErr error
	}{
		// A renamed .txt (AC2): Put never looks at a filename or client-declared
		// type, only the sniffed bytes, so plain text is rejected regardless of
		// what extension or Content-Type a client might have sent it under.
		{"renamed .txt / plain text content", newStore(t, 10<<20), []byte("this is not an image, just plain text"), domain.ErrUnsupportedMediaType},
		{"oversize", newStore(t, 8), pngBytes(t), domain.ErrPhotoTooLarge},
		{"sniffs as an image but does not decode", newStore(t, 10<<20), sniffsAsJPEGButUndecodable, domain.ErrInvalidPhoto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.store.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(tc.data)); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Put error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestPutRejectionLeavesNoPartialFile covers AC1: a rejected (oversize)
// upload must not leave a partial file behind in the store root.
func TestPutRejectionLeavesNoPartialFile(t *testing.T) {
	root := t.TempDir()
	s, err := adapter.NewLocalPhotoStore(root, 8)
	if err != nil {
		t.Fatalf("NewLocalPhotoStore: %v", err)
	}
	hh := household.NewHouseholdID()
	if _, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(pngBytes(t))); !errors.Is(err, domain.ErrPhotoTooLarge) {
		t.Fatalf("Put error = %v, want ErrPhotoTooLarge", err)
	}

	var files []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk store root: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("rejected upload left files behind in the store root: %v", files)
	}
}

// TestPutStreamsWithoutBufferingWholeFile is a direct behavioral proof, for
// this specific (moderately large) fixture, that Put reads in bounded chunks
// (io.Copy's default buffer) rather than one call sized to the whole upload.
//
// It is not, on its own, a reliable general guard against a future regression
// to io.ReadAll: io.ReadAll grows its internal buffer geometrically (starting
// at 512 bytes, ~1.5x per growth step — see io.ReadAll's source), so the
// single largest Read it issues only exceeds a given threshold once enough
// chunks have accumulated; for a small enough fixture, or a smaller threshold
// here, io.ReadAll could still finish (hit EOF) before ever requesting a
// buffer this test would flag, letting that regression pass by coincidence.
// TestUploadPathNeverBuffersWholeFile (upload_streaming_test.go) is the
// deterministic, fixture-size-independent check for that — it inspects the
// source for the call itself rather than sampling read-buffer sizes. Kept
// alongside it as a complementary, concrete demonstration that streaming
// actually happens for a real (if modest) upload.
func TestPutStreamsWithoutBufferingWholeFile(t *testing.T) {
	s := newStore(t, 20<<20)
	hh := household.NewHouseholdID()
	big := largeJPEGBytes(t, 900, 900)
	tracker := &maxChunkReader{r: bytes.NewReader(big)}

	if _, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, tracker); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if tracker.maxRequested == 0 {
		t.Fatal("Put never read from the source reader")
	}
	// io.Copy's default internal buffer is 32 KiB; allow headroom for the
	// sniff read without permitting anything close to len(big).
	const maxReasonableChunk = 128 << 10
	if tracker.maxRequested > maxReasonableChunk {
		t.Fatalf("Put requested a %d-byte read buffer (upload was %d bytes); it must stream in bounded chunks, not buffer the whole upload", tracker.maxRequested, len(big))
	}
}

func TestOpenUnknownRef(t *testing.T) {
	s := newStore(t, 10<<20)
	if _, err := s.Open(context.Background(), domain.StorageRef("nope/aa/deadbeef.jpg")); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Open(unknown) error = %v, want ErrPhotoNotFound", err)
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	s := newStore(t, 10<<20)
	if _, err := s.Open(context.Background(), domain.StorageRef("../../etc/passwd")); !errors.Is(err, domain.ErrInvalidPhoto) {
		t.Fatalf("traversal ref error = %v, want ErrInvalidPhoto", err)
	}
}

// TestPutNamespacesKeyByClass covers AC2: identical bytes uploaded under
// different domain.PhotoClass values must land under different storage
// prefixes, so a chore-proof photo (or a reward image) can never collide
// with, or be reinterpreted as, an album photo even when the content is
// byte-identical.
func TestPutNamespacesKeyByClass(t *testing.T) {
	s := newStore(t, 10<<20)
	hh := household.NewHouseholdID()
	data := jpegBytes(t)

	album, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(album): %v", err)
	}
	choreProof, err := s.Put(context.Background(), hh, domain.PhotoClassChoreProof, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(choreProof): %v", err)
	}
	rewardImage, err := s.Put(context.Background(), hh, domain.PhotoClassRewardImage, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(rewardImage): %v", err)
	}

	if album.Ref == choreProof.Ref || album.Ref == rewardImage.Ref || choreProof.Ref == rewardImage.Ref {
		t.Fatalf("identical bytes under different classes must not share a ref: album=%s choreProof=%s rewardImage=%s", album.Ref, choreProof.Ref, rewardImage.Ref)
	}
	// The content hash itself is class-independent (it is a property of the
	// bytes, not of where they are namespaced) — only the ref's prefix differs.
	if album.ContentHash != choreProof.ContentHash || album.ContentHash != rewardImage.ContentHash {
		t.Fatalf("identical bytes produced different content hashes across classes: album=%s choreProof=%s rewardImage=%s", album.ContentHash, choreProof.ContentHash, rewardImage.ContentHash)
	}

	cases := []struct {
		name   string
		ref    string
		prefix string
	}{
		{"album", album.Ref.String(), "photos"},
		{"chore proof", choreProof.Ref.String(), "chore-photos"},
		{"reward image", rewardImage.Ref.String(), "reward-images"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := "households/" + hh.String() + "/" + tc.prefix + "/"
			if !strings.HasPrefix(tc.ref, want) {
				t.Fatalf("ref %q does not start with %q", tc.ref, want)
			}
		})
	}
}

// TestPutRejectsUnknownClass covers Put's fail-fast contract for an
// unrecognized domain.PhotoClass: it must reject before writing anything,
// leaving no partial file behind (mirroring
// TestPutRejectionLeavesNoPartialFile's shape for the oversize case).
func TestPutRejectsUnknownClass(t *testing.T) {
	root := t.TempDir()
	s, err := adapter.NewLocalPhotoStore(root, 10<<20)
	if err != nil {
		t.Fatalf("NewLocalPhotoStore: %v", err)
	}
	hh := household.NewHouseholdID()

	// Both an out-of-range value and the deliberately-invalid zero value
	// must be rejected — the zero value especially, so a caller that forgot
	// to choose a class can never default into the album namespace.
	for _, class := range []domain.PhotoClass{domain.PhotoClass(99), domain.PhotoClassUnspecified} {
		if _, err := s.Put(context.Background(), hh, class, bytes.NewReader(jpegBytes(t))); err == nil {
			t.Fatalf("Put with invalid PhotoClass %v must fail", class)
		}
	}

	var files []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk store root: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("rejected upload left files behind in the store root: %v", files)
	}
}

// TestOpenAndDeleteServeLegacyRef covers AC4: a photo stored before the
// class-namespaced key scheme landed (a bare <household>/<aa>/<hash>.<ext>
// ref, with no "households/" or class prefix) must remain servable —
// LocalPhotoStore treats ref as an opaque relative path in Open/Delete and
// never assumes its shape, so a pre-existing file at the legacy layout is
// found exactly as it always was.
func TestOpenAndDeleteServeLegacyRef(t *testing.T) {
	root := t.TempDir()
	s, err := adapter.NewLocalPhotoStore(root, 10<<20)
	if err != nil {
		t.Fatalf("NewLocalPhotoStore: %v", err)
	}
	hh := household.NewHouseholdID()
	want := jpegBytes(t)
	legacyRef := domain.StorageRef(filepath.ToSlash(filepath.Join(hh.String(), "de", "deadbeef.jpg")))

	legacyPath := filepath.Join(root, filepath.FromSlash(legacyRef.String()))
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("create legacy dir: %v", err)
	}
	if err := os.WriteFile(legacyPath, want, 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	rc, err := s.Open(context.Background(), legacyRef)
	if err != nil {
		t.Fatalf("Open(legacy ref): %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("Open(legacy ref) returned %d bytes, want %d", len(got), len(want))
	}

	if err := s.Delete(context.Background(), legacyRef); err != nil {
		t.Fatalf("Delete(legacy ref): %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete(legacy ref) did not remove the file, stat err = %v", err)
	}
}

// TestURL covers the PhotoStore.URL contract: a stored ref resolves to a
// non-empty locator, and an unknown ref reports ErrPhotoNotFound — the same
// not-found contract as Open, since URL confirms existence the same way.
func TestURL(t *testing.T) {
	s := newStore(t, 10<<20)
	hh := household.NewHouseholdID()

	stored, err := s.Put(context.Background(), hh, domain.PhotoClassAlbum, bytes.NewReader(jpegBytes(t)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.URL(context.Background(), stored.Ref, 5*time.Minute)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if got == "" {
		t.Fatal("URL returned an empty locator for a stored ref")
	}

	if _, err := s.URL(context.Background(), domain.StorageRef("nope/aa/deadbeef.jpg"), 5*time.Minute); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("URL(unknown) error = %v, want ErrPhotoNotFound", err)
	}

	// A directory ref (e.g. the households prefix itself) is not a stored
	// photo and must be rejected the same way.
	dirRef := domain.StorageRef(path.Dir(stored.Ref.String()))
	if _, err := s.URL(context.Background(), dirRef, 5*time.Minute); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("URL(directory) error = %v, want ErrPhotoNotFound", err)
	}
}
