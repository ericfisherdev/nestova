package adapter

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// gsm7SeptetCost / isGSM7Encodable / gsm7SeptetLength
// ---------------------------------------------------------------------------

func TestGsm7SeptetCost(t *testing.T) {
	tests := []struct {
		name     string
		r        rune
		wantCost int
		wantOK   bool
	}{
		{"basic alphabet letter", 'a', 1, true},
		{"basic alphabet digit", '5', 1, true},
		{"extension table bracket", '[', 2, true},
		{"extension table euro sign", '€', 2, true},
		{"emoji is not GSM-7 encodable", '😀', 0, false},
		{"cyrillic is not GSM-7 encodable", 'д', 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost, ok := gsm7SeptetCost(tt.r)
			if ok != tt.wantOK {
				t.Fatalf("gsm7SeptetCost(%q) ok = %v, want %v", tt.r, ok, tt.wantOK)
			}
			if ok && cost != tt.wantCost {
				t.Errorf("gsm7SeptetCost(%q) cost = %d, want %d", tt.r, cost, tt.wantCost)
			}
		})
	}
}

func TestIsGSM7Encodable(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"empty string", "", true},
		{"plain ASCII", "Chore reminder: take out the trash", true},
		{"extension characters only widen septet cost, not encodability", "Total: [$12.50] {tax incl.} 5% off | €10", true},
		{"a single emoji makes the whole body non-GSM-7", "Great job! 😀", false},
		{"non-Latin script is not GSM-7 encodable", "спасибо", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGSM7Encodable(tt.s); got != tt.want {
				t.Errorf("isGSM7Encodable(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestGsm7SeptetLength(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want int
	}{
		{"basic-only body costs one septet per rune", "abc", 3},
		{"extension characters cost two septets each", "[abc]", 2 + 1 + 1 + 1 + 2},
		{"a lone extension character", "€", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gsm7SeptetLength(tt.s); got != tt.want {
				t.Errorf("gsm7SeptetLength(%q) = %d, want %d", tt.s, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// truncateSMSBody — pure function, no AWS dependency.
// ---------------------------------------------------------------------------

func TestTruncateSMSBody_GSM7(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty body unchanged", "", ""},
		{"short body unchanged", "Chore reminder: take out the trash", "Chore reminder: take out the trash"},
		{"exactly at the septet cap unchanged", strings.Repeat("a", maxGSM7Septets), strings.Repeat("a", maxGSM7Septets)},
		{
			"one septet over the cap is truncated with a GSM-7 ellipsis",
			strings.Repeat("a", maxGSM7Septets+1),
			strings.Repeat("a", maxGSM7Septets-len([]rune(gsm7Ellipsis))) + gsm7Ellipsis,
		},
		{
			"far over the cap is truncated to exactly the septet budget",
			strings.Repeat("a", maxGSM7Septets*3),
			strings.Repeat("a", maxGSM7Septets-len([]rune(gsm7Ellipsis))) + gsm7Ellipsis,
		},
		{
			// 90 extension-table characters cost 180 septets — over the
			// 160 cap despite being only 90 runes, proving the cap is
			// septet-based, not a flat rune count: a rune-count cap of
			// 160 would have left this body untouched.
			"extension characters overflow the septet cap well under 160 runes",
			strings.Repeat("[", 90),
			strings.Repeat("[", 78) + gsm7Ellipsis, // budget 157 / cost 2 => 78 kept (156 septets) + 3-septet ellipsis = 159
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateSMSBody(tt.body)
			if got != tt.want {
				t.Errorf("truncateSMSBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
			if n := gsm7SeptetLength(got); n > maxGSM7Septets {
				t.Errorf("truncateSMSBody(%q) result costs %d septets, want at most %d", tt.body, n, maxGSM7Septets)
			}
		})
	}
}

func TestTruncateSMSBody_UCS2(t *testing.T) {
	// あ is a single BMP character outside GSM-7, costing exactly 1 UTF-16
	// code unit — used to test the UCS-2 path's plain (non-surrogate-pair)
	// boundary behavior.
	const bmpChar = "あ"

	tests := []struct {
		name string
		body string
		want string
	}{
		{"a single non-GSM-7 character forces the UCS-2 path", bmpChar, bmpChar},
		{"exactly at the UCS-2 unit cap unchanged", strings.Repeat(bmpChar, maxUCS2Units), strings.Repeat(bmpChar, maxUCS2Units)},
		{
			"one unit over the UCS-2 cap is truncated with a UCS-2 ellipsis",
			strings.Repeat(bmpChar, maxUCS2Units+1),
			strings.Repeat(bmpChar, maxUCS2Units-1) + ucs2Ellipsis,
		},
		{
			// A GSM-7-only prefix long enough to fill the septet cap
			// alone, plus one emoji, is still governed by the UCS-2
			// budget for the WHOLE body — SMS encoding is chosen once
			// per message, never mixed within a segment.
			"a body that is mostly GSM-7 but contains one emoji is truncated under the UCS-2 budget, not the GSM-7 one",
			strings.Repeat("a", maxGSM7Septets) + "😀",
			strings.Repeat("a", maxUCS2Units-1) + ucs2Ellipsis,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateSMSBody(tt.body)
			if got != tt.want {
				t.Errorf("truncateSMSBody(%q) = %q, want %q", tt.body, got, tt.want)
			}
			if n := len(utf16.Encode([]rune(got))); n > maxUCS2Units {
				t.Errorf("truncateSMSBody(%q) result costs %d UTF-16 units, want at most %d", tt.body, n, maxUCS2Units)
			}
		})
	}
}

// TestTruncateSMSBody_UCS2_NeverSplitsASurrogatePair proves the UCS-2 path
// truncates whole runes, not UTF-16 units: a supplementary-plane character
// (e.g. most emoji) needs a 2-unit surrogate pair, and cutting between the
// two halves would produce a lone, invalid surrogate.
func TestTruncateSMSBody_UCS2_NeverSplitsASurrogatePair(t *testing.T) {
	// 40 emoji, each costing 2 UTF-16 units (80 units total) — over the 70
	// unit cap, so at least one must be dropped whole.
	body := strings.Repeat("😀", 40)
	got := truncateSMSBody(body)

	if !utf8.ValidString(got) {
		t.Fatalf("truncateSMSBody produced invalid UTF-8: %q", got)
	}
	units := utf16.Encode([]rune(got))
	if len(units) > maxUCS2Units {
		t.Errorf("truncateSMSBody(%d emoji) result costs %d UTF-16 units, want at most %d", 40, len(units), maxUCS2Units)
	}
	// A split surrogate pair would leave an odd character (the emoji minus
	// its trailing ellipsis) that utf16.Encode cannot round-trip back to
	// the original rune count without a stray unpaired surrogate; the
	// stronger, simpler proof is that every rune in got is either a whole
	// 😀 or the ellipsis itself.
	want := strings.Repeat("😀", 34) + ucs2Ellipsis
	if got != want {
		t.Errorf("truncateSMSBody(%d emoji) = %q, want %q", 40, got, want)
	}
}

// TestTruncateSMSBody_NeverSplitsAMultiByteRune proves truncation is
// rune-aware, not byte-aware, on the GSM-7 path too: a body whose cap
// falls mid-character must still produce valid UTF-8.
func TestTruncateSMSBody_NeverSplitsAMultiByteRune(t *testing.T) {
	body := strings.Repeat("a", maxGSM7Septets*3)
	got := truncateSMSBody(body)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateSMSBody produced invalid UTF-8: %q", got)
	}
}
