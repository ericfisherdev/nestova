package adapter

import "unicode/utf16"

// gsm7Basic is the GSM 03.38 default alphabet (ETSI TS 100 900 / 3GPP TS
// 23.038) — every character costing exactly one septet in a GSM-7-encoded
// SMS. The escape code (0x1B, which signals "the next character comes from
// the extension table below") is deliberately omitted: it has no meaning
// as a literal body character, so a body that somehow contained it is
// correctly treated as not GSM-7-encodable by gsm7SeptetCost.
var gsm7Basic = map[rune]struct{}{
	'@': {}, '£': {}, '$': {}, '¥': {}, 'è': {}, 'é': {}, 'ù': {}, 'ì': {},
	'ò': {}, 'Ç': {}, '\n': {}, 'Ø': {}, 'ø': {}, '\r': {}, 'Å': {}, 'å': {},
	'Δ': {}, '_': {}, 'Φ': {}, 'Γ': {}, 'Λ': {}, 'Ω': {}, 'Π': {}, 'Ψ': {},
	'Σ': {}, 'Θ': {}, 'Ξ': {}, 'Æ': {}, 'æ': {}, 'ß': {}, 'É': {},
	' ': {}, '!': {}, '"': {}, '#': {}, '¤': {}, '%': {}, '&': {}, '\'': {},
	'(': {}, ')': {}, '*': {}, '+': {}, ',': {}, '-': {}, '.': {}, '/': {},
	'0': {}, '1': {}, '2': {}, '3': {}, '4': {}, '5': {}, '6': {}, '7': {},
	'8': {}, '9': {}, ':': {}, ';': {}, '<': {}, '=': {}, '>': {}, '?': {},
	'¡': {},
	'A': {}, 'B': {}, 'C': {}, 'D': {}, 'E': {}, 'F': {}, 'G': {}, 'H': {},
	'I': {}, 'J': {}, 'K': {}, 'L': {}, 'M': {}, 'N': {}, 'O': {}, 'P': {},
	'Q': {}, 'R': {}, 'S': {}, 'T': {}, 'U': {}, 'V': {}, 'W': {}, 'X': {},
	'Y': {}, 'Z': {}, 'Ä': {}, 'Ö': {}, 'Ñ': {}, 'Ü': {}, '§': {},
	'¿': {},
	'a': {}, 'b': {}, 'c': {}, 'd': {}, 'e': {}, 'f': {}, 'g': {}, 'h': {},
	'i': {}, 'j': {}, 'k': {}, 'l': {}, 'm': {}, 'n': {}, 'o': {}, 'p': {},
	'q': {}, 'r': {}, 's': {}, 't': {}, 'u': {}, 'v': {}, 'w': {}, 'x': {},
	'y': {}, 'z': {}, 'ä': {}, 'ö': {}, 'ñ': {}, 'ü': {}, 'à': {},
}

// gsm7Extended is the GSM 03.38 extension table — each of these costs TWO
// septets (the ESC 0x1B escape plus the character itself), not one.
var gsm7Extended = map[rune]struct{}{
	'\f': {}, '^': {}, '{': {}, '}': {}, '\\': {}, '[': {}, '~': {}, ']': {}, '|': {}, '€': {},
}

// gsm7SeptetCost returns the number of septets r costs in a GSM-7-encoded
// SMS and whether r is GSM-7-encodable at all: 1 septet for the basic
// alphabet, 2 for an extension-table character (the escape costs a septet
// too), or ok=false for anything neither table has (e.g. an emoji or most
// non-Latin scripts), which forces the whole message to UCS-2 instead —
// see isGSM7Encodable.
func gsm7SeptetCost(r rune) (cost int, ok bool) {
	if _, basic := gsm7Basic[r]; basic {
		return 1, true
	}
	if _, extended := gsm7Extended[r]; extended {
		return 2, true
	}
	return 0, false
}

// isGSM7Encodable reports whether every rune in s has a GSM 03.38
// representation. SMS encoding is chosen per MESSAGE, never mixed within
// one segment, so a single character outside both GSM-7 tables (e.g. one
// emoji in an otherwise-ASCII body) forces the ENTIRE body to UCS-2 —
// truncateSMSBody's caller-facing contract, not just this function's.
func isGSM7Encodable(s string) bool {
	for _, r := range s {
		if _, ok := gsm7SeptetCost(r); !ok {
			return false
		}
	}
	return true
}

// gsm7SeptetLength sums s's GSM-7 septet cost. The caller must already
// know isGSM7Encodable(s) is true — this does not itself check.
func gsm7SeptetLength(s string) int {
	total := 0
	for _, r := range s {
		cost, _ := gsm7SeptetCost(r)
		total += cost
	}
	return total
}

const (
	// maxGSM7Septets is a single GSM-7-encoded SMS segment's payload
	// capacity.
	maxGSM7Septets = 160
	// gsm7Ellipsis is appended to a GSM-7-truncated body: three periods,
	// each a basic-alphabet character costing exactly one septet (3
	// septets total).
	gsm7Ellipsis = "..."

	// maxUCS2Units is a single UCS-2-encoded SMS segment's payload
	// capacity, in UTF-16 code units — used whenever the body contains at
	// least one character GSM-7 cannot represent.
	maxUCS2Units = 70
	// ucs2Ellipsis is appended to a UCS-2-truncated body: a single
	// ellipsis character, one UTF-16 code unit (cheaper than three
	// GSM-7-style periods would cost under UCS-2's own budget).
	ucs2Ellipsis = "…"
)

// truncateSMSBody caps body at what fits in a SINGLE SMS segment under
// whichever encoding the carrier will actually use for it: GSM-7 (160
// septets, where an extension-table character like [ ] { } € costs two)
// if every character in body has a GSM-7 representation, or UCS-2 (70
// UTF-16 code units) otherwise, appending an encoding-appropriate ellipsis
// when truncation occurs. A body that would otherwise require multiple
// segments is capped to exactly ONE instead of being silently split by the
// carrier across several, each billed separately (NES-138 AC: "never
// split") — capping at a flat rune count regardless of encoding under-caps
// plain ASCII and over-caps a body containing extension or non-GSM-7
// characters, which is why the encoding is resolved first.
func truncateSMSBody(body string) string {
	if isGSM7Encodable(body) {
		return truncateGSM7(body)
	}
	return truncateUCS2(body)
}

// truncateGSM7 truncates body to maxGSM7Septets septets. The caller must
// already know isGSM7Encodable(body) is true.
func truncateGSM7(body string) string {
	if gsm7SeptetLength(body) <= maxGSM7Septets {
		return body
	}
	budget := maxGSM7Septets - gsm7SeptetLength(gsm7Ellipsis)
	var kept []rune
	used := 0
	for _, r := range body {
		cost, _ := gsm7SeptetCost(r)
		if used+cost > budget {
			break
		}
		used += cost
		kept = append(kept, r)
	}
	return string(kept) + gsm7Ellipsis
}

// truncateUCS2 truncates body to maxUCS2Units UTF-16 code units — never
// splitting a surrogate pair (a rune outside the Basic Multilingual Plane,
// e.g. most emoji, costs 2 units and is kept or dropped as one unit), the
// UTF-16 analogue of never splitting a multi-byte UTF-8 rune.
func truncateUCS2(body string) string {
	runes := []rune(body)
	if len(utf16.Encode(runes)) <= maxUCS2Units {
		return body
	}
	budget := maxUCS2Units - len(utf16.Encode([]rune(ucs2Ellipsis)))
	var kept []rune
	used := 0
	for _, r := range runes {
		cost := len(utf16.Encode([]rune{r}))
		if used+cost > budget {
			break
		}
		used += cost
		kept = append(kept, r)
	}
	return string(kept) + ucs2Ellipsis
}
