package adapter

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"time"

	"github.com/xor-gate/goexif2/exif"

	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// exifDateLayout is the "YYYY:MM:DD HH:MM:SS" form every EXIF date/time tag
// uses, mirroring the layout goexif2's own (*Exif).DateTime uses internally.
const exifDateLayout = "2006:01:02 15:04:05"

// reencodeQuality is the JPEG quality used when Scrub must fully re-encode a
// photo to bake in its EXIF Orientation (see Scrub's doc). 90 keeps visible
// quality loss minimal for a proof-of-work photo while still compressing
// well below the original — re-encoding only ever runs for the minority of
// uploads whose Orientation is not already 1 (upright)/0 (unknown).
const reencodeQuality = 90

// TakenAtAndOrientation returns the EXIF capture time (UTC) and the
// Orientation tag from a JPEG's already-buffered bytes, or (nil, 0) when the
// bytes carry no decodable EXIF at all. Hardened against a crafted image the
// same way TakenAt is: a panic deep in the third-party parser on malformed
// input is recovered, never propagated.
//
// Capture time comes from DateTimeOriginal ONLY — no DateTime/
// DateTimeDigitized fallback (see the interface doc on
// domain.ChoreProofExif for why: those tags don't guarantee the photo was
// just taken with a camera, which is the whole point of the chore-proof
// freshness gate). This intentionally makes TakenAtAndOrientation stricter
// than TakenAt (the album path's EXIF reader), which still prefers
// DateTimeOriginal but falls back to DateTime — the album path has no
// freshness/provenance requirement to protect.
//
// No EXIF OffsetTimeOriginal (tag 0x9011) handling is attempted: goexif2
// v1.1.0's internal tag table only loads tag ids it explicitly maps
// (LoadTags drops anything else during parsing, with no public hook to
// extend that table short of forking the library or re-implementing its
// unexported JPEG-APP1-segment location logic from scratch), so
// OffsetTimeOriginal is unreachable through the library's public API.
// Because Nestova stores no household-level timezone anywhere today (there
// is no such column in the household schema), a naive (offset-less) EXIF
// timestamp is interpreted in the server's own local time zone — exactly
// what goexif2's own DateTime() already falls back to (it also honors a
// Canon MakerNote timezone field when present, which this function keeps by
// reusing the same tag lookup DateTime() performs).
//
// Deployment assumption this relies on: Nestova is deployed as a
// single-household LAN appliance (one server, one family, one physical
// location — see the project's deployment docs), so the server's own local
// timezone IS the household's timezone in every deployment this ships to
// today; "fall back to server-local time" is therefore an accurate
// inference here, not a guess papering over a gap. ChoreProofPhotoService's
// freshness window (MEDIA_CHORE_PROOF_FRESHNESS_WINDOW) relies on the same
// assumption for the same reason. If Nestova ever supports multiple
// households on one server, or a household whose members are not
// physically local to the server's own clock/timezone, this assumption
// breaks and a real household-level timezone setting becomes necessary —
// this function (and the freshness check) are the two places that would
// need to change.
func (ExifReader) TakenAtAndOrientation(data []byte) (taken *time.Time, orientation int) {
	defer func() {
		if rec := recover(); rec != nil {
			taken, orientation = nil, 0
		}
	}()
	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0
	}
	return dateTimeOriginal(x), orientationFromTags(x)
}

// dateTimeOriginal returns the EXIF DateTimeOriginal tag, normalized to UTC,
// or nil when absent or unparseable — see TakenAtAndOrientation's doc for
// why this is the ONLY tag consulted (no DateTime/DateTimeDigitized
// fallback).
func dateTimeOriginal(x *exif.Exif) *time.Time {
	loc := time.Local
	if tz, err := x.TimeZone(); err == nil && tz != nil {
		loc = tz
	}
	tag, err := x.Get(exif.DateTimeOriginal)
	if err != nil {
		return nil
	}
	raw, err := tag.StringVal()
	if err != nil {
		return nil
	}
	t, err := time.ParseInLocation(exifDateLayout, raw, loc)
	if err != nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// orientationFromTags returns the EXIF Orientation tag's value (1-8), or 0
// when absent, unreadable, or outside that range.
func orientationFromTags(x *exif.Exif) int {
	tag, err := x.Get(exif.Orientation)
	if err != nil {
		return 0
	}
	v, err := tag.Int(0)
	if err != nil || v < 1 || v > 8 {
		return 0
	}
	return v
}

// Scrub removes every EXIF tag from a JPEG's bytes, never just the GPS IFD.
//
// Deliberate, documented tradeoff: goexif2 is read-only, and a
// surgical "remove only the GPS IFD, keep everything else" rewrite would
// mean hand-editing a TIFF directory structure in place (patching an IFD's
// entry count and byte offsets after deleting entries) — real work, and
// still leaves every OTHER EXIF field (camera make/model/serial number,
// software version, etc.) on a photo whose privacy motivation (per NES-119's
// ticket: these are often kids' proof-of-chore photos) is not limited to
// GPS. Stripping the WHOLE EXIF APP1 segment is simpler, strictly more
// private, and is what this function does for the common case (Orientation
// already 1 or 0): stripJPEGExif below removes the "Exif\0\0"-signed APP1
// segment at the byte level, leaving every other JPEG segment (and the
// entropy-coded scan data) untouched — no re-compression, no quality loss.
//
// The one piece of EXIF metadata that visibly matters for a STORED photo is
// Orientation: a camera held sideways writes upright pixel data and just
// declares Orientation != 1 rather than rotating the pixels itself, so a
// blind whole-segment strip would make the stored photo display sideways
// (nothing left to tell a viewer to rotate it). When orientation is
// anything other than 1 (already upright) or 0 (unknown — nothing to
// correct), Scrub instead fully decodes the image and re-encodes it with the
// orientation baked into the pixel data (reencodeUpright below) — heavier
// (one JPEG re-compression pass, at reencodeQuality) but only for the
// minority of uploads that actually need rotating, and it also strips ALL
// metadata as a side effect, since Go's jpeg.Encode never writes EXIF.
func (ExifReader) Scrub(data []byte, orientation int) ([]byte, error) {
	if orientation > 1 && orientation <= 8 {
		return reencodeUpright(data, orientation)
	}
	return stripJPEGExif(data)
}

// reencodeUpright decodes data, applies the EXIF Orientation transform o (2-8)
// to the pixel data, and re-encodes as a fresh JPEG with no EXIF at all.
//
// Bounds the claimed pixel count via DecodeConfig BEFORE the full Decode
// below, which allocates a full pixel buffer sized to it — a decompression
// bomb (a small file whose header claims enormous dimensions) would
// otherwise let this allocation run unchecked. This mirrors
// LocalPhotoStore.Put's identical maxDecodePixels guard (photo_store.go, in
// this same package) verbatim, including the constant — Scrub runs BEFORE
// Put ever sees the bytes (see ChoreProofPhotoService's doc), so Put's own
// guard cannot be relied on to catch this first.
func reencodeUpright(data []byte, o int) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Wrapped as domain.ErrInvalidPhoto (not a bare infra-style error):
		// a caller-supplied image that fails to even decode its header is a
		// client-input problem, and the web layer maps ErrInvalidPhoto to
		// 400 — a bare error here would fall through to a misleading 500.
		return nil, fmt.Errorf("%w: decode image config for orientation bake: %v", domain.ErrInvalidPhoto, err)
	}
	if pixels := int64(cfg.Width) * int64(cfg.Height); pixels > maxDecodePixels {
		return nil, fmt.Errorf("%w: image is %dx%d (%d pixels), exceeds the %d-pixel limit", domain.ErrInvalidPhoto, cfg.Width, cfg.Height, pixels, maxDecodePixels)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: decode image for orientation bake: %v", domain.ErrInvalidPhoto, err)
	}
	upright := applyOrientation(img, o)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, upright, &jpeg.Options{Quality: reencodeQuality}); err != nil {
		return nil, fmt.Errorf("media/adapter: re-encode oriented image: %w", err)
	}
	return out.Bytes(), nil
}

// applyOrientation returns a new RGBA image with src reoriented according to
// the EXIF Orientation tag o (2-8; callers only invoke this for o != 1/0).
// The transforms are defined by the EXIF/TIFF spec (Exif 2.3 section 4.6.4,
// tag 0x0112):
//
//	2: flip horizontal                6: rotate 90 CW
//	3: rotate 180                     7: flip horizontal, then rotate 90 CW
//	4: flip vertical                  8: rotate 270 CW (90 CCW)
//	5: flip horizontal, then rotate 270 CW (== transpose)
func applyOrientation(src image.Image, o int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 { // 5, 6, 7, 8 all swap width/height.
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			dx, dy := x, y
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, c)
		}
	}
	return dst
}

// JPEG marker bytes stripJPEGExif parses. Only the markers it needs to
// recognize are named; every other marker is copied through unexamined.
const (
	jpegMarkerByte  = 0xFF
	jpegSOIMarker   = 0xD8
	jpegEOIMarker   = 0xD9
	jpegSOSMarker   = 0xDA
	jpegTEMMarker   = 0x01
	jpegRestart0    = 0xD0
	jpegRestart7    = 0xD7
	jpegAPP1Marker  = 0xE1
	jpegLengthBytes = 2
)

// exifAPP1Signature is the 6-byte payload prefix ("Exif\0\0") that
// distinguishes an EXIF APP1 segment from any other APP1 use (e.g. XMP,
// which this function deliberately leaves alone — only EXIF is in scope).
var exifAPP1Signature = []byte("Exif\x00\x00")

// stripJPEGExif returns data with every EXIF ("Exif\0\0"-signed) APP1
// segment removed, leaving every other JPEG segment and the entropy-coded
// scan data byte-for-byte untouched. Non-JPEG input (no SOI marker at all)
// is returned unchanged, nil error — that is a different, legitimate case
// from "is a JPEG but its internal segment structure is malformed", which is
// what everything below actually guards against.
//
// Hardened against malformed/truncated/adversarial input: the segment walk
// only runs ahead of the Start-Of-Scan marker (where every APPn/COM/DQT/etc.
// segment lives), is fully bounds-checked, and — this is the load-bearing
// choice — REJECTS with domain.ErrInvalidPhoto the moment anything looks
// genuinely malformed (a byte where a marker's code was expected but isn't
// 0xFF, a length field that doesn't fit, dangling fill bytes with no marker
// code) rather than falling back to copying the unexamined remainder of
// data verbatim. That fallback used to be the behavior here, and it was a
// privacy hole: a structurally-broken segment BEFORE a real EXIF APP1
// segment (e.g. SOI + one stray junk byte + a valid EXIF segment + a valid
// image) would make the walk bail out at the junk byte and copy everything
// after it — including the still-intact EXIF — straight into the "scrubbed"
// output. Rejecting instead is the privacy-safe default: a chore-proof
// photo is expected to come straight from a real camera, so a structurally
// broken JPEG is not worth salvaging, and the caller (PhotoStore.Put
// downstream, or this rejection directly) treats it the same as any other
// invalid upload.
//
// The JPEG spec permits any number of extra 0xFF fill bytes immediately
// before a marker's code byte outside scan data — real encoders do emit
// this occasionally — and this walk consumes them explicitly (see the
// inner loop below) rather than treating a second consecutive 0xFF as
// malformed, which would otherwise reject perfectly legitimate photos.
// Fill bytes are copied through unchanged when the segment they precede is
// KEPT; when that segment is the EXIF one being dropped, its fill bytes are
// dropped right along with it — they only ever padded up to a segment that
// no longer exists in the output, so keeping them behind would serve no
// purpose.
func stripJPEGExif(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != jpegMarkerByte || data[1] != jpegSOIMarker {
		return data, nil
	}
	out := make([]byte, 0, len(data))
	out = append(out, data[0], data[1])
	i := 2
	for i < len(data) {
		if data[i] != jpegMarkerByte {
			return nil, fmt.Errorf("%w: malformed JPEG segment structure (expected a marker byte)", domain.ErrInvalidPhoto)
		}
		// Consume any legal 0xFF fill bytes ahead of the actual marker code
		// byte; j lands on the first non-0xFF byte, which is the code.
		j := i + 1
		for j < len(data) && data[j] == jpegMarkerByte {
			j++
		}
		if j >= len(data) {
			return nil, fmt.Errorf("%w: truncated JPEG (dangling fill bytes with no marker code)", domain.ErrInvalidPhoto)
		}
		marker := data[j]
		switch {
		case marker == jpegSOSMarker, marker == jpegEOIMarker:
			// Everything from here on is scan data (or the end of the
			// file); no more segments to inspect — a normal, successful
			// termination of the walk, not malformed input.
			return append(out, data[i:]...), nil
		case marker == jpegTEMMarker || (marker >= jpegRestart0 && marker <= jpegRestart7):
			// Standalone markers: no length field follows.
			out = append(out, data[i:j+1]...)
			i = j + 1
		default:
			if j+1+jpegLengthBytes > len(data) {
				return nil, fmt.Errorf("%w: truncated JPEG segment length field", domain.ErrInvalidPhoto)
			}
			segLen := int(data[j+1])<<8 | int(data[j+2])
			if segLen < jpegLengthBytes || j+1+segLen > len(data) {
				return nil, fmt.Errorf("%w: malformed or truncated JPEG segment length", domain.ErrInvalidPhoto)
			}
			segEnd := j + 1 + segLen
			if !isExifAPP1(marker, data[j+3:segEnd]) {
				out = append(out, data[i:segEnd]...)
			}
			i = segEnd
		}
	}
	return out, nil
}

// isExifAPP1 reports whether marker/payload identify an EXIF APP1 segment
// (as opposed to XMP or any other APP1 use, which are left untouched).
// payload is the segment's content after its 2-byte length field.
func isExifAPP1(marker byte, payload []byte) bool {
	return marker == jpegAPP1Marker &&
		len(payload) >= len(exifAPP1Signature) &&
		bytes.Equal(payload[:len(exifAPP1Signature)], exifAPP1Signature)
}

var _ domain.ChoreProofExif = ExifReader{}
