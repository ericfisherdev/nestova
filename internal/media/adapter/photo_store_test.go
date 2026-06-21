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
	"testing"

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

func newStore(t *testing.T, maxBytes int64) *adapter.LocalPhotoStore {
	t.Helper()
	s, err := adapter.NewLocalPhotoStore(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("NewLocalPhotoStore: %v", err)
	}
	return s
}

func TestPutStoresAndOpensAndDeletes(t *testing.T) {
	s := newStore(t, 10<<20)
	hh := household.NewHouseholdID()
	want := pngBytes(t)

	ref, err := s.Put(context.Background(), hh, want, "image/png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref == "" {
		t.Fatal("Put returned an empty ref")
	}

	rc, err := s.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("Open returned %d bytes, want %d", len(got), len(want))
	}

	if err := s.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Delete is idempotent.
	if err := s.Delete(context.Background(), ref); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := s.Open(context.Background(), ref); !errors.Is(err, domain.ErrPhotoNotFound) {
		t.Fatalf("Open after delete error = %v, want ErrPhotoNotFound", err)
	}
}

func TestPutContentAddressedDeduplicates(t *testing.T) {
	s := newStore(t, 10<<20)
	hh := household.NewHouseholdID()
	data := jpegBytes(t)
	r1, err := s.Put(context.Background(), hh, data, "image/jpeg")
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	r2, err := s.Put(context.Background(), hh, data, "image/jpeg")
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if r1 != r2 {
		t.Fatalf("identical bytes produced different refs: %s vs %s", r1, r2)
	}
}

func TestPutRejections(t *testing.T) {
	hh := household.NewHouseholdID()
	cases := []struct {
		name        string
		store       *adapter.LocalPhotoStore
		data        []byte
		contentType string
		wantErr     error
	}{
		{"unsupported type", newStore(t, 10<<20), pngBytes(t), "application/pdf", domain.ErrUnsupportedMediaType},
		{"oversize", newStore(t, 8), pngBytes(t), "image/png", domain.ErrPhotoTooLarge},
		{"not an image", newStore(t, 10<<20), []byte("this is not an image"), "image/png", domain.ErrInvalidPhoto},
		{"type mismatch (png bytes declared jpeg)", newStore(t, 10<<20), pngBytes(t), "image/jpeg", domain.ErrInvalidPhoto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.store.Put(context.Background(), hh, tc.data, tc.contentType); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Put error = %v, want %v", err, tc.wantErr)
			}
		})
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
