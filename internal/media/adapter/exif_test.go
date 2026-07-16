package adapter_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/media/adapter"
)

// exifJPEG builds a minimal JPEG (SOI + APP1/Exif + EOI) carrying a single IFD0
// DateTime tag (0x0132), so the EXIF reader has a real capture date to decode.
// datetime must be the 19-char "YYYY:MM:DD HH:MM:SS" EXIF form.
func exifJPEG(datetime string) []byte {
	ds := append([]byte(datetime), 0x00) // 20 bytes incl. NUL terminator

	var tiff bytes.Buffer
	tiff.WriteString("II")                     // little-endian byte order
	tiff.Write([]byte{0x2A, 0x00})             // TIFF magic (42)
	tiff.Write([]byte{0x08, 0x00, 0x00, 0x00}) // IFD0 begins at offset 8
	tiff.Write([]byte{0x01, 0x00})             // IFD0: 1 entry
	tiff.Write([]byte{0x32, 0x01})             // tag 0x0132 (DateTime)
	tiff.Write([]byte{0x02, 0x00})             // type 2 (ASCII)
	tiff.Write([]byte{0x14, 0x00, 0x00, 0x00}) // count 20
	tiff.Write([]byte{0x1A, 0x00, 0x00, 0x00}) // value at offset 26
	tiff.Write([]byte{0x00, 0x00, 0x00, 0x00}) // next IFD: none
	tiff.Write(ds)                             // the DateTime string @ offset 26

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	segLen := len(payload) + 2 // APP1 length includes the 2-byte length field

	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8})                      // SOI
	out.Write([]byte{0xFF, 0xE1})                      // APP1 marker
	out.Write([]byte{byte(segLen >> 8), byte(segLen)}) // length (big-endian)
	out.Write(payload)                                 //
	out.Write([]byte{0xFF, 0xD9})                      // EOI
	return out.Bytes()
}

func TestExifTakenAtPresent(t *testing.T) {
	got := adapter.NewExifReader().TakenAt(bytes.NewReader(exifJPEG("2021:08:15 12:34:56")))
	if got == nil {
		t.Fatal("TakenAt = nil, want the EXIF capture time")
	}
	// goexif parses the zoneless EXIF time in time.Local; the adapter normalizes
	// to UTC, so the expectation is the same local instant converted to UTC.
	want := time.Date(2021, 8, 15, 12, 34, 56, 0, time.Local).UTC()
	if !got.Equal(want) {
		t.Fatalf("TakenAt = %s, want %s", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("TakenAt location = %s, want UTC", got.Location())
	}
}

func TestExifTakenAtAbsentOrInvalid(t *testing.T) {
	r := adapter.NewExifReader()
	// A plain JPEG with no EXIF segment.
	if got := r.TakenAt(bytes.NewReader(jpegBytes(t))); got != nil {
		t.Fatalf("TakenAt(no exif) = %s, want nil", got)
	}
	// Non-image bytes must not panic and must return nil.
	if got := r.TakenAt(bytes.NewReader([]byte("definitely not an image"))); got != nil {
		t.Fatalf("TakenAt(garbage) = %s, want nil", got)
	}
}
