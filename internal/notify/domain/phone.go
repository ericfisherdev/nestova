package domain

import (
	"fmt"
	"regexp"
)

// e164Pattern matches a valid E.164-formatted phone number: a leading '+',
// then 1 to 15 digits with no leading zero (a leading zero would make the
// country-code digit ambiguous). This is the exact format AWS End User
// Messaging's SendTextMessage requires for DestinationPhoneNumber.
var e164Pattern = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)

// E164Phone is a validated E.164-formatted phone number (e.g.
// "+15551234567"), mirroring media/domain/ids.go's validated-wrapper
// pattern for a string primitive instead of a uuid.UUID one. The zero
// value is never a valid phone number — every E164Phone in circulation was
// constructed by ParseE164Phone.
type E164Phone string

// String returns the E.164 phone number string.
func (p E164Phone) String() string { return string(p) }

// ParseE164Phone validates s as an E.164-formatted phone number and
// returns it wrapped as an E164Phone, or an error when s does not match.
//
// Validating the FORMAT before any AWS API call matters specifically for
// SMS: AWS End User Messaging bills a SendTextMessage attempt against a
// malformed destination the SAME as a successful send — a request that was
// always going to fail still costs money the instant it leaves this
// process. Rejecting a malformed number here, before any network call, is
// the only way to avoid paying for a guaranteed failure.
func ParseE164Phone(s string) (E164Phone, error) {
	if !e164Pattern.MatchString(s) {
		return "", fmt.Errorf("notify: invalid E.164 phone number %q", s)
	}
	return E164Phone(s), nil
}
