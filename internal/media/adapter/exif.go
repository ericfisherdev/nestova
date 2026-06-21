package adapter

import (
	"bytes"
	"time"

	"github.com/xor-gate/goexif2/exif"
)

// ExifReader is a domain.ExifReader that reads the EXIF capture time from image
// bytes using the xor-gate/goexif2 fork. EXIF parsing runs over untrusted upload
// bytes, so TakenAt is hardened to never crash on malformed input.
type ExifReader struct{}

// NewExifReader returns an ExifReader.
func NewExifReader() ExifReader { return ExifReader{} }

// TakenAt returns the photo's EXIF capture time normalized to UTC, or nil when
// the image carries no usable EXIF date. A missing tag, an undecodable EXIF
// block, or a panic deep in the parser on malformed input is not an error — the
// photo is simply stored without a taken_at. The recover guard ensures a crafted
// image cannot crash the server through the third-party parser.
func (ExifReader) TakenAt(data []byte) (taken *time.Time) {
	defer func() {
		if r := recover(); r != nil {
			taken = nil
		}
	}()
	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	// DateTime() prefers DateTimeOriginal and falls back to DateTime; it returns
	// the camera/local zone, so normalize to UTC for the app-wide UTC convention.
	t, err := x.DateTime()
	if err != nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}
