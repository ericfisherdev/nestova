// Package qrcode is a thin platform seam over a QR-encoding library,
// mirroring internal/platform/render: it renders a QR code entirely
// server-side and returns it as a self-contained "data:image/png;base64,..."
// URI, so callers (Templ components) embed it directly in an <img src> with
// no client-side QR JS and no extra HTTP round trip to fetch the image.
//
// The library used is github.com/yeqown/go-qrcode/v2 (NES-129) — the ticket
// named skip2/go-qrcode, but that module is unmaintained (last release
// predates Go modules' current tooling expectations, no open-source activity
// in years); yeqown/go-qrcode is actively maintained and was chosen instead.
package qrcode

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	qr "github.com/yeqown/go-qrcode/v2"
	stdwriter "github.com/yeqown/go-qrcode/writer/standard"
)

// dataURIPrefix precedes the base64-encoded PNG bytes in the returned string.
const dataURIPrefix = "data:image/png;base64,"

// ErrInvalidModuleSize is returned by PNGDataURI when moduleSize falls
// outside the range standard.WithQRWidth's uint8 parameter can represent
// (1-255). Validating here, before the int→uint8 conversion, turns an
// out-of-range value into an explicit error instead of a silently wrapped
// (e.g. 256 → 0) or negative-to-huge-unsigned pixel size.
var ErrInvalidModuleSize = errors.New("qrcode: moduleSize must be between 1 and 255")

// PNGDataURI renders content (typically an absolute deep-link URL) as a QR
// code and returns it as a "data:image/png;base64,..." URI. moduleSize scales
// the PNG's pixel dimensions (pixels per QR module — see
// standard.WithQRWidth) and must be in [1, 255]; a caller with no specific
// sizing need can pass a small constant like 8, which comfortably reads at
// kiosk-card size.
//
// Rendering is entirely in-memory (a bytes.Buffer, never a temp file), so
// this is safe to call on every kiosk page render with no filesystem access
// or cleanup concern.
func PNGDataURI(content string, moduleSize int) (string, error) {
	if moduleSize <= 0 || moduleSize > 255 {
		return "", fmt.Errorf("%w: got %d", ErrInvalidModuleSize, moduleSize)
	}

	code, err := qr.New(content)
	if err != nil {
		return "", fmt.Errorf("qrcode: encode: %w", err)
	}

	var buf bytes.Buffer
	writer := stdwriter.NewWithWriter(
		nopWriteCloser{&buf},
		stdwriter.WithBuiltinImageEncoder(stdwriter.PNG_FORMAT),
		stdwriter.WithQRWidth(uint8(moduleSize)),
	)
	if err := code.Save(writer); err != nil {
		return "", fmt.Errorf("qrcode: render png: %w", err)
	}

	return dataURIPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// nopWriteCloser adapts a bytes.Buffer (an io.Writer) to the io.WriteCloser
// the standard writer requires, since a Buffer needs no closing.
type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }
