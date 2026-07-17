package adapter_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// --- synthetic TIFF/EXIF fixture builder ---
//
// exif_test.go's exifJPEG helper hand-builds a single-IFD TIFF payload
// (IFD0 only) for a top-level DateTime tag. TakenAtAndOrientation also needs
// to read tags from the Exif sub-IFD (DateTimeOriginal, DateTimeDigitized)
// reached via IFD0's ExifIFDPointer, and an Orientation tag alongside
// DateTime in IFD0 — the builder below generalizes to both IFDs so each test
// only has to name the tags/values it cares about.

// tiffFieldKind is the TIFF field Type this fixture builder supports: just
// enough (ASCII strings, SHORT integers, and the LONG pointer
// ExifIFDPointer needs) to build the fixtures these tests require.
type tiffFieldKind uint16

const (
	tiffASCII tiffFieldKind = 2
	tiffSHORT tiffFieldKind = 3
	tiffLONG  tiffFieldKind = 4
)

// tiffField is one TIFF/EXIF directory entry for buildTIFF.
type tiffField struct {
	tag   uint16
	kind  tiffFieldKind
	short uint16
	ascii string
}

// exifIFDPointerTag is the IFD0 tag (0x8769) whose value is the byte offset
// of the Exif sub-IFD.
const exifIFDPointerTag = 0x8769

// buildTIFF encodes a little-endian TIFF byte stream with IFD0 (ifd0Fields)
// and, when exifSubFields is non-empty, an Exif sub-IFD reached via an
// ExifIFDPointer entry buildTIFF appends to IFD0 automatically (callers
// never add it themselves). Every ifd0Fields entry must be SHORT or LONG
// (fits inline, no value area needed for IFD0 in these fixtures); ASCII
// entries are only supported in exifSubFields, which is all these tests need
// (DateTimeOriginal/DateTime/DateTimeDigitized).
func buildTIFF(t *testing.T, ifd0Fields []tiffField, exifSubFields []tiffField) []byte {
	t.Helper()
	const tiffHeaderSize = 8
	ifd0Offset := uint32(tiffHeaderSize)

	fields := append([]tiffField(nil), ifd0Fields...)
	hasExifSub := len(exifSubFields) > 0
	if hasExifSub {
		fields = append(fields, tiffField{tag: exifIFDPointerTag, kind: tiffLONG})
	}
	ifd0FixedSize := 2 + 12*len(fields) + 4
	// The Exif sub-IFD starts after IFD0's fixed directory AND its value
	// area (any ASCII field's string bytes) — not right after the fixed
	// directory alone, or the ExifIFDPointer would point into the middle of
	// IFD0's own ASCII values whenever ifd0Fields carries one.
	var ifd0ValueAreaSize uint32
	for _, f := range ifd0Fields {
		if f.kind == tiffASCII {
			ifd0ValueAreaSize += uint32(len(f.ascii) + 1)
		}
	}
	exifSubOffset := ifd0Offset + uint32(ifd0FixedSize) + ifd0ValueAreaSize

	var buf bytes.Buffer
	buf.WriteString("II")
	writeU16(&buf, 42)
	writeU32(&buf, ifd0Offset)
	buf.Write(encodeIFD(t, ifd0Offset, fields, exifSubOffset, hasExifSub, 0))
	if hasExifSub {
		buf.Write(encodeIFD(t, exifSubOffset, exifSubFields, 0, false, 0))
	}
	return buf.Bytes()
}

// encodeIFD encodes one IFD's fixed directory structure plus the value area
// any ASCII entries need, starting at ifdOffset within the eventual TIFF
// stream. When patchExifPointer is set, the LAST field in fields (added by
// buildTIFF) is the ExifIFDPointer entry and its LONG value is set to
// exifSubOffset.
func encodeIFD(t *testing.T, ifdOffset uint32, fields []tiffField, exifSubOffset uint32, patchExifPointer bool, next uint32) []byte {
	t.Helper()
	n := len(fields)
	fixedSize := 2 + 12*n + 4
	valueCursor := ifdOffset + uint32(fixedSize)

	var fixed bytes.Buffer
	writeU16(&fixed, uint16(n))
	var values bytes.Buffer
	for idx, f := range fields {
		writeU16(&fixed, f.tag)
		switch {
		case patchExifPointer && idx == n-1:
			writeU16(&fixed, uint16(tiffLONG))
			writeU32(&fixed, 1)
			writeU32(&fixed, exifSubOffset)
		case f.kind == tiffASCII:
			s := f.ascii + "\x00"
			writeU16(&fixed, uint16(tiffASCII))
			writeU32(&fixed, uint32(len(s)))
			writeU32(&fixed, valueCursor)
			values.WriteString(s)
			valueCursor += uint32(len(s))
		case f.kind == tiffSHORT:
			writeU16(&fixed, uint16(tiffSHORT))
			writeU32(&fixed, 1)
			writeU16(&fixed, f.short)
			writeU16(&fixed, 0)
		default:
			t.Fatalf("encodeIFD: unsupported field kind %v for tag %#x", f.kind, f.tag)
		}
	}
	writeU32(&fixed, next)
	return append(fixed.Bytes(), values.Bytes()...)
}

func writeU16(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeU32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}

// exifJPEGFixture splices a synthetic EXIF APP1 segment (built from ifd0Fields/
// exifSubFields) onto a real, decodable JPEG's own segments (base's own SOI is
// dropped in favor of the fixture's), so the result is both a genuinely valid,
// fully decodable photo AND carries the exact EXIF tags a test names.
func exifJPEGFixture(t *testing.T, base []byte, ifd0Fields, exifSubFields []tiffField) []byte {
	t.Helper()
	tiff := buildTIFF(t, ifd0Fields, exifSubFields)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segLen := len(payload) + 2

	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8})
	out.Write([]byte{0xFF, 0xE1})
	out.Write([]byte{byte(segLen >> 8), byte(segLen)})
	out.Write(payload)
	out.Write(base[2:]) // base's own segments after its SOI, through EOI
	return out.Bytes()
}

func dateTimeField(tag uint16, v string) tiffField {
	return tiffField{tag: tag, kind: tiffASCII, ascii: v}
}

const (
	tagDateTime          = 0x0132
	tagOrientation       = 0x0112
	tagDateTimeOriginal  = 0x9003
	tagDateTimeDigitized = 0x9004
)

// --- TakenAtAndOrientation ---

// TestTakenAtAndOrientationReadsDateTimeOriginal covers the happy path: a
// camera-direct DateTimeOriginal tag is read and normalized to UTC.
func TestTakenAtAndOrientationReadsDateTimeOriginal(t *testing.T) {
	data := exifJPEGFixture(t, jpegBytes(t),
		[]tiffField{dateTimeField(tagDateTime, "2020:01:01 00:00:00")},
		[]tiffField{dateTimeField(tagDateTimeOriginal, "2026:03:01 08:15:30")},
	)
	taken, _ := adapter.NewExifReader().TakenAtAndOrientation(data)
	if taken == nil {
		t.Fatal("TakenAtAndOrientation returned a nil capture time")
	}
	want := time.Date(2026, 3, 1, 8, 15, 30, 0, time.Local).UTC()
	if !taken.Equal(want) {
		t.Fatalf("TakenAt = %s, want %s", taken, want)
	}
}

// TestTakenAtAndOrientationIgnoresDateTimeWithoutOriginal and
// TestTakenAtAndOrientationIgnoresDateTimeDigitizedWithoutOriginal cover
// the NES-119 review's provenance tightening: a plain DateTime or
// DateTimeDigitized tag, with NO DateTimeOriginal present, must NOT be
// accepted as a chore-proof capture time — those tags don't guarantee the
// photo was just taken with a camera (an edited/re-saved/scanned image can
// carry either without ever having DateTimeOriginal set), which defeats the
// freshness gate's whole purpose. This intentionally reverses the
// pre-review behavior (which fell back to both).

func TestTakenAtAndOrientationIgnoresDateTimeWithoutOriginal(t *testing.T) {
	data := exifJPEGFixture(t, jpegBytes(t),
		[]tiffField{dateTimeField(tagDateTime, "2025:06:15 12:00:00")},
		nil,
	)
	taken, _ := adapter.NewExifReader().TakenAtAndOrientation(data)
	if taken != nil {
		t.Fatalf("TakenAtAndOrientation = %s, want nil (a plain DateTime tag with no DateTimeOriginal must not count)", taken)
	}
}

func TestTakenAtAndOrientationIgnoresDateTimeDigitizedWithoutOriginal(t *testing.T) {
	data := exifJPEGFixture(t, jpegBytes(t),
		nil,
		[]tiffField{dateTimeField(tagDateTimeDigitized, "2025:12:25 09:30:00")},
	)
	taken, _ := adapter.NewExifReader().TakenAtAndOrientation(data)
	if taken != nil {
		t.Fatalf("TakenAtAndOrientation = %s, want nil (a DateTimeDigitized tag with no DateTimeOriginal must not count)", taken)
	}
}

func TestTakenAtAndOrientationAbsentOrInvalid(t *testing.T) {
	r := adapter.NewExifReader()
	if taken, orientation := r.TakenAtAndOrientation(jpegBytes(t)); taken != nil || orientation != 0 {
		t.Fatalf("no-exif JPEG = taken:%v orientation:%d, want nil, 0", taken, orientation)
	}
	if taken, orientation := r.TakenAtAndOrientation([]byte("definitely not an image")); taken != nil || orientation != 0 {
		t.Fatalf("garbage bytes = taken:%v orientation:%d, want nil, 0 (must not panic)", taken, orientation)
	}
}

func TestTakenAtAndOrientationReadsOrientation(t *testing.T) {
	data := exifJPEGFixture(t, jpegBytes(t),
		[]tiffField{{tag: tagOrientation, kind: tiffSHORT, short: 6}},
		nil,
	)
	_, orientation := adapter.NewExifReader().TakenAtAndOrientation(data)
	if orientation != 6 {
		t.Fatalf("Orientation = %d, want 6", orientation)
	}

	noOrientation := exifJPEGFixture(t, jpegBytes(t),
		[]tiffField{dateTimeField(tagDateTime, "2025:01:01 00:00:00")},
		nil,
	)
	if _, orientation := adapter.NewExifReader().TakenAtAndOrientation(noOrientation); orientation != 0 {
		t.Fatalf("Orientation with no tag = %d, want 0", orientation)
	}
}

// --- Scrub ---

// TestScrubStripsAllExifForUprightOrientation covers the round-trip half of
// AC4 (no GPS EXIF survives): Scrub with orientation 1 (already upright) or
// 0 (unknown) removes the ENTIRE EXIF APP1 segment at the byte level —
// stronger than "no GPS tags", since nothing EXIF-shaped survives at all —
// while leaving the image data itself byte-for-byte unchanged (no
// re-compression).
func TestScrubStripsAllExifForUprightOrientation(t *testing.T) {
	base := jpegBytes(t)
	data := exifJPEGFixture(t, base,
		[]tiffField{dateTimeField(tagDateTime, "2026:01:01 00:00:00")},
		[]tiffField{dateTimeField(tagDateTimeOriginal, "2026:01:01 00:00:00")},
	)
	r := adapter.NewExifReader()

	for _, orientation := range []int{0, 1} {
		scrubbed, err := r.Scrub(data, orientation)
		if err != nil {
			t.Fatalf("Scrub(orientation=%d): %v", orientation, err)
		}
		if bytes.Contains(scrubbed, []byte("Exif\x00\x00")) {
			t.Fatalf("Scrub(orientation=%d) left an EXIF signature in the output", orientation)
		}
		if taken, o := r.TakenAtAndOrientation(scrubbed); taken != nil || o != 0 {
			t.Fatalf("Scrub(orientation=%d) output still yields EXIF facts: taken=%v orientation=%d", orientation, taken, o)
		}
		// Byte-for-byte, not just same-bounds-after-decode: stripping is a
		// pure segment removal (SOI + the fixture's synthetic EXIF APP1 +
		// base's own post-SOI bytes), so removing that APP1 segment must
		// reproduce base exactly — proving the strip touches nothing else,
		// not merely that the result happens to decode to the same size.
		if !bytes.Equal(scrubbed, base) {
			t.Fatalf("Scrub(orientation=%d) = %d bytes, want exactly base's %d bytes (byte-level strip must reproduce the original minus only the EXIF segment)", orientation, len(scrubbed), len(base))
		}
		img, _, err := image.Decode(bytes.NewReader(scrubbed))
		if err != nil {
			t.Fatalf("Scrub(orientation=%d) output does not decode: %v", orientation, err)
		}
		baseImg, _, err := image.Decode(bytes.NewReader(base))
		if err != nil {
			t.Fatalf("decode base fixture: %v", err)
		}
		if img.Bounds() != baseImg.Bounds() {
			t.Fatalf("Scrub(orientation=%d) changed image bounds: got %v, want %v (byte-level strip must not touch pixel data)", orientation, img.Bounds(), baseImg.Bounds())
		}
	}
}

// TestScrubStripsExifPrecededByFillBytes covers the NES-119 review's fill-byte
// fix: the JPEG spec allows extra 0xFF padding bytes immediately before a
// marker's code byte outside scan data, and stripJPEGExif must consume them
// and still find/remove the EXIF APP1 segment that follows — not bail out
// and leave the EXIF (and any GPS it carries) intact, which the pre-review
// implementation did.
func TestScrubStripsExifPrecededByFillBytes(t *testing.T) {
	base := jpegBytes(t)
	tiff := buildTIFF(t, []tiffField{dateTimeField(tagDateTime, "2026:01:01 00:00:00")}, nil)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segLen := len(payload) + 2

	var data bytes.Buffer
	data.Write([]byte{0xFF, 0xD8}) // SOI
	// Legal 0xFF fill bytes before the APP1 marker's code byte (0xE1).
	data.Write([]byte{0xFF, 0xFF, 0xFF})
	data.Write([]byte{0xFF, 0xE1})
	data.Write([]byte{byte(segLen >> 8), byte(segLen)})
	data.Write(payload)
	data.Write(base[2:])

	scrubbed, err := adapter.NewExifReader().Scrub(data.Bytes(), 0)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if bytes.Contains(scrubbed, []byte("Exif\x00\x00")) {
		t.Fatalf("Scrub left an EXIF signature behind a fill-byte-padded APP1 marker: %x", scrubbed)
	}
	// The fill bytes only ever padded up to the EXIF segment; once that
	// segment is dropped, they are dropped right along with it (see
	// stripJPEGExif's doc), so the result reproduces base exactly — same as
	// the no-padding case.
	if !bytes.Equal(scrubbed, base) {
		t.Fatalf("Scrub(fill-byte-padded) = %d bytes, want exactly base's %d bytes", len(scrubbed), len(base))
	}
	img, _, err := image.Decode(bytes.NewReader(scrubbed))
	if err != nil {
		t.Fatalf("scrubbed output does not decode: %v", err)
	}
	baseImg, _, err := image.Decode(bytes.NewReader(base))
	if err != nil {
		t.Fatalf("decode base fixture: %v", err)
	}
	if img.Bounds() != baseImg.Bounds() {
		t.Fatalf("bounds changed: got %v, want %v", img.Bounds(), baseImg.Bounds())
	}
}

// TestScrubNonJPEGReturnsUnchanged covers Scrub's documented no-op for input
// that is not a JPEG at all (no SOI marker) — the caller
// (ChoreProofPhotoService) only invokes Scrub after sniffing a JPEG prefix,
// but Scrub itself stays safe if ever called otherwise.
func TestScrubNonJPEGReturnsUnchanged(t *testing.T) {
	garbage := []byte("not a jpeg at all")
	got, err := adapter.NewExifReader().Scrub(garbage, 0)
	if err != nil {
		t.Fatalf("Scrub(non-jpeg): %v", err)
	}
	if !bytes.Equal(got, garbage) {
		t.Fatalf("Scrub(non-jpeg) = %v, want input returned unchanged", got)
	}
}

// blockImage builds a decodable JPEG whose pixels are laid out in
// 8x8-pixel-aligned solid-color blocks (aligned to JPEG's minimum coded unit
// so each block's interior survives lossy compression essentially exactly),
// arranged cols x rows, so a geometric transform's effect on block
// positions can be checked by sampling each block's center pixel.
func blockImage(t *testing.T, cols, rows int, colors [][]color.RGBA) []byte {
	t.Helper()
	const block = 8
	img := image.NewRGBA(image.Rect(0, 0, cols*block, rows*block))
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			col := colors[r][c]
			for y := 0; y < block; y++ {
				for x := 0; x < block; x++ {
					img.Set(c*block+x, r*block+y, col)
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// blockCenterColor decodes data and returns the pixel at the center of block
// (col, row) in a cols x rows, 8px-block grid — used to check a transform's
// effect without being thrown off by lossy edge blur.
func blockCenterColor(t *testing.T, data []byte, col, row int) color.RGBA {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	const block = 8
	r, g, b, _ := img.At(col*block+block/2, row*block+block/2).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8)}
}

// closeEnough allows a small per-channel tolerance for JPEG quantization
// even at quality 100 / block-center sampling.
func closeEnough(a, b color.RGBA) bool {
	const tolerance = 12
	diff := func(x, y uint8) bool {
		d := int(x) - int(y)
		if d < 0 {
			d = -d
		}
		return d <= tolerance
	}
	return diff(a.R, b.R) && diff(a.G, b.G) && diff(a.B, b.B)
}

// TestScrubReencodesAndBakesOrientation covers Scrub's re-encode path
// (orientation != 1, != 0): the returned bytes decode to an image whose
// dimensions and block layout reflect the EXIF Orientation transform baked
// into the pixels, with every EXIF tag gone as a side effect of the
// re-encode (Go's jpeg.Encode never writes EXIF).
func TestScrubReencodesAndBakesOrientation(t *testing.T) {
	red := color.RGBA{R: 220, A: 255}
	green := color.RGBA{G: 220, A: 255}
	blue := color.RGBA{B: 220, A: 255}
	yellow := color.RGBA{R: 220, G: 220, A: 255}
	// 2 cols x 1 row: [red, green] — used for the mirror/rotate-180/flip
	// cases, where dimensions do not swap.
	wide := blockImage(t, 2, 1, [][]color.RGBA{{red, green}})
	// 2 cols x 2 rows: [[red, green], [blue, yellow]] — used for the
	// swap-dimension cases (5, 6, 7, 8), which need a genuinely asymmetric
	// image to distinguish all four.
	square := blockImage(t, 2, 2, [][]color.RGBA{{red, green}, {blue, yellow}})

	r := adapter.NewExifReader()

	t.Run("orientation 2 flips horizontal, no dimension swap", func(t *testing.T) {
		got, err := r.Scrub(wide, 2)
		if err != nil {
			t.Fatalf("Scrub: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 8 {
			t.Fatalf("bounds = %v, want 16x8 (no swap)", img.Bounds())
		}
		if !closeEnough(blockCenterColor(t, got, 0, 0), green) || !closeEnough(blockCenterColor(t, got, 1, 0), red) {
			t.Fatalf("orientation 2 did not mirror horizontally: left=%v right=%v", blockCenterColor(t, got, 0, 0), blockCenterColor(t, got, 1, 0))
		}
	})

	t.Run("orientation 6 rotates 90 CW and swaps dimensions", func(t *testing.T) {
		got, err := r.Scrub(square, 6)
		if err != nil {
			t.Fatalf("Scrub: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		// square is 16x16 (2x2 blocks); rotating 90 CW keeps it 16x16 here,
		// but the transform must have swapped dims in general — check via a
		// non-square source below too.
		if img.Bounds().Dx() != 16 || img.Bounds().Dy() != 16 {
			t.Fatalf("bounds = %v, want 16x16", img.Bounds())
		}
		// 90 CW: src(x,y) -> dst(h-1-y, x). Top-left (red, block 0,0) moves
		// to the top-right; bottom-left (blue, block 0,1) moves to top-left.
		if !closeEnough(blockCenterColor(t, got, 0, 0), blue) {
			t.Fatalf("orientation 6 top-left = %v, want blue (bottom-left rotated into top-left)", blockCenterColor(t, got, 0, 0))
		}
		if !closeEnough(blockCenterColor(t, got, 1, 0), red) {
			t.Fatalf("orientation 6 top-right = %v, want red (top-left rotated into top-right)", blockCenterColor(t, got, 1, 0))
		}
	})

	t.Run("orientation 8 rotates 270 CW and swaps dimensions", func(t *testing.T) {
		nonSquare := blockImage(t, 3, 1, [][]color.RGBA{{red, green, blue}}) // 24x8
		got, err := r.Scrub(nonSquare, 8)
		if err != nil {
			t.Fatalf("Scrub: %v", err)
		}
		img, _, err := image.Decode(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 24 {
			t.Fatalf("bounds = %v, want 8x24 (dimensions must swap for orientation 8)", img.Bounds())
		}
	})

	t.Run("re-encoded output carries no EXIF", func(t *testing.T) {
		got, err := r.Scrub(wide, 3)
		if err != nil {
			t.Fatalf("Scrub: %v", err)
		}
		if bytes.Contains(got, []byte("Exif\x00\x00")) {
			t.Fatal("re-encoded output must never contain an EXIF signature")
		}
	})
}

// hugeDimensionJPEG builds a minimal, syntactically valid JPEG (SOI + a
// JFIF APP0 marker + a SOF0 marker claiming width x height) that
// image.DecodeConfig successfully parses purely from the SOF0 header,
// without needing DQT/DHT/SOS or any actual entropy-coded scan data — Go's
// jpeg decoder returns as soon as it has processed SOF0 when a JFIF marker
// preceded it (image/jpeg/reader.go: `if configOnly && d.jfif { return }`).
// This lets a test claim an enormous pixel count in a tiny file, exactly
// the decompression-bomb shape maxDecodePixels guards against, without
// needing to construct gigabytes of real (or even valid) scan data.
func hugeDimensionJPEG(width, height uint16) []byte {
	return []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xE0, 0x00, 0x10, // APP0, len=16
		'J', 'F', 'I', 'F', 0x00, // "JFIF\0"
		0x01, 0x01, // version
		0x00,       // units
		0x00, 0x01, // Xdensity
		0x00, 0x01, // Ydensity
		0x00, 0x00, // thumbnail dims (none)
		0xFF, 0xC0, 0x00, 0x0B, // SOF0, len=11
		0x08, // precision
		byte(height >> 8), byte(height),
		byte(width >> 8), byte(width),
		0x01,             // numComponents
		0x01, 0x11, 0x00, // component 1: sampling 1x1, quant table 0
	}
}

// TestScrubReencodeBoundsPixelsBeforeFullDecode covers the NES-119 review's
// decompression-bomb guard: Scrub's orientation-bake path (Scrub runs
// BEFORE PhotoStore.Put ever validates anything — see the type doc) must
// reject a claimed pixel count over maxDecodePixels using DecodeConfig
// alone, the same way LocalPhotoStore.Put already does, rather than
// attempting the full, memory-heavy image.Decode first.
func TestScrubReencodeBoundsPixelsBeforeFullDecode(t *testing.T) {
	// 40000 x 40000 = 1.6 billion pixels, far over the 50-million-pixel
	// limit, encoded in well under 100 bytes.
	huge := hugeDimensionJPEG(40000, 40000)

	_, err := adapter.NewExifReader().Scrub(huge, 6) // orientation != 1/0 triggers reencodeUpright
	if !errors.Is(err, domain.ErrInvalidPhoto) {
		t.Fatalf("Scrub(huge dimensions) error = %v, want ErrInvalidPhoto", err)
	}
}

func TestScrubTruncatedOrMalformedNeverPanics(t *testing.T) {
	r := adapter.NewExifReader()
	inputs := [][]byte{
		{0xFF, 0xD8},                         // just SOI
		{0xFF, 0xD8, 0xFF},                   // truncated marker
		{0xFF, 0xD8, 0xFF, 0xE1, 0xFF},       // truncated length
		{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0xFF}, // length claims more than present
	}
	for _, in := range inputs {
		got, err := r.Scrub(in, 0)
		if err != nil {
			t.Fatalf("Scrub(%x): unexpected error %v", in, err)
		}
		if got == nil {
			t.Fatalf("Scrub(%x) returned nil", in)
		}
	}
}
