package domain

import "strings"

// splitPlainDecimal splits s into its whole and fractional digit strings. It
// accepts only unsigned plain decimals — digits, optionally followed by a dot and
// at least one digit. Exponents, signs (other than a negative zero), separators, whitespace and non-ASCII
// digits are rejected, as is a fraction longer than maxFractionDigits. The
// fraction is empty when s has no dot.
func splitPlainDecimal(s string, maxFractionDigits int) (whole, fraction string, ok bool) {
	// A negative zero ("-0", "-0.00") reads as zero, as it always has; any
	// other signed value stays refused.
	if unsigned, found := strings.CutPrefix(s, "-"); found && isZero(unsigned) {
		s = unsigned
	}
	whole, fraction, hasDot := strings.Cut(s, ".")
	if !isDigits(whole) || (hasDot && !isDigits(fraction)) {
		return "", "", false
	}
	if len(fraction) > maxFractionDigits {
		return "", "", false
	}
	return whole, fraction, true
}

// isZero reports whether s is a plain decimal made only of zeros.
func isZero(s string) bool {
	whole, fraction, hasDot := strings.Cut(s, ".")
	return isDigits(whole) && (!hasDot || isDigits(fraction)) && strings.Trim(s, "0.") == ""
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
