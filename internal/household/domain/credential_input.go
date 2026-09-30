package domain

import (
	"errors"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// Domain errors returned by ValidateEmail and ValidatePassword (NES-196).
var (
	// ErrEmailMalformed is returned by ValidateEmail when the address is not a
	// single bare addr-spec (for example a@@b.com or "Name <a@b.com>").
	ErrEmailMalformed = errors.New("household: member email is malformed")
	// ErrEmailTooLong is returned by ValidateEmail when the local part exceeds
	// MaxEmailLocalPartLength or the whole address exceeds MaxEmailLength.
	ErrEmailTooLong = errors.New("household: member email is too long")
	// ErrPasswordTooLong is returned by ValidatePassword when the password
	// exceeds MaxPasswordLength characters.
	ErrPasswordTooLong = errors.New("household: member password is too long")
)

// Bounds for member credentials. The email limits come from RFC 5321
// (64-octet local part, 254-octet address). The password cap bounds what
// onboarding and add-member will hash and store as a new credential. Login and
// password re-verification are deliberately not capped: a member whose
// password predates the cap must still be able to sign in.
const (
	MaxEmailLocalPartLength = 64
	MaxEmailLength          = 254
	MaxPasswordLength       = 256
)

// ValidateEmail returns ErrEmailMalformed unless email is exactly one bare
// addr-spec, and ErrEmailTooLong when it exceeds the RFC 5321 length limits.
// Lengths are counted in bytes, as the RFC counts octets. Internationalized
// domains are valid. Blank-email handling stays with the caller.
func ValidateEmail(email string) error {
	if strings.Count(email, "@") != 1 {
		return ErrEmailMalformed
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		return ErrEmailMalformed
	}
	localPart, _, _ := strings.Cut(email, "@")
	if len(localPart) > MaxEmailLocalPartLength || len(email) > MaxEmailLength {
		return ErrEmailTooLong
	}
	return nil
}

// ValidatePassword returns ErrPasswordTooLong when password exceeds
// MaxPasswordLength runes. The minimum length stays with the caller.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}
