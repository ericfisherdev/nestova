package qrcode_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/qrcode"
)

// pngMagic is the 8-byte signature every valid PNG file starts with.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

func TestPNGDataURI_ReturnsDecodablePNG(t *testing.T) {
	uri, err := qrcode.PNGDataURI("https://nestova.example.ts.net/go/claim-task/abc-123?exp=1&sig=xyz", 8)
	if err != nil {
		t.Fatalf("PNGDataURI: %v", err)
	}

	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(uri, prefix) {
		t.Fatalf("PNGDataURI() = %q, want prefix %q", uri, prefix)
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, prefix))
	if err != nil {
		t.Fatalf("decode base64 payload: %v", err)
	}
	if !bytes.HasPrefix(raw, pngMagic) {
		t.Errorf("decoded payload does not start with the PNG magic bytes")
	}
}

func TestPNGDataURI_DifferentContentProducesDifferentImages(t *testing.T) {
	a, err := qrcode.PNGDataURI("https://nestova.example.ts.net/go/claim-task/1", 8)
	if err != nil {
		t.Fatalf("PNGDataURI(a): %v", err)
	}
	b, err := qrcode.PNGDataURI("https://nestova.example.ts.net/go/claim-task/2", 8)
	if err != nil {
		t.Fatalf("PNGDataURI(b): %v", err)
	}
	if a == b {
		t.Error("PNGDataURI produced identical output for different content")
	}
}

func TestPNGDataURI_RejectsInvalidModuleSize(t *testing.T) {
	tests := []struct {
		name       string
		moduleSize int
	}{
		{"zero", 0},
		{"negative", -1},
		{"above uint8 range", 256},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := qrcode.PNGDataURI("https://nestova.example.ts.net/go/claim-task/1", tt.moduleSize)
			if !errors.Is(err, qrcode.ErrInvalidModuleSize) {
				t.Fatalf("PNGDataURI(moduleSize=%d) error = %v, want %v", tt.moduleSize, err, qrcode.ErrInvalidModuleSize)
			}
		})
	}
}

func TestPNGDataURI_AcceptsBoundaryModuleSizes(t *testing.T) {
	// 1 (the lower bound) and 50 (comfortably within range, but still
	// multi-digit) — NOT the literal upper bound 255: even at the smallest
	// possible QR (a 1-character payload, ~21 modules per side), 255 renders
	// a ~5355x5355px image, disproportionately slow for what it adds. The
	// precise upper edge of the accepted range (255 vs. the rejected 256) is
	// already exercised, far more cheaply, by
	// TestPNGDataURI_RejectsInvalidModuleSize's "above uint8 range" case,
	// which fails PNGDataURI's own size check before any rendering happens.
	for _, size := range []int{1, 50} {
		if _, err := qrcode.PNGDataURI("https://nestova.example.ts.net/go/claim-task/1", size); err != nil {
			t.Errorf("PNGDataURI(moduleSize=%d): %v", size, err)
		}
	}
}
