package domain

import "errors"

// Signed-link verification errors, returned by the deeplink/app package's
// Signer.Verify and surfaced by the HTTP adapter as the same friendly
// "rescan from the kiosk" response regardless of which applies — mirroring
// how kiosk activation codes never distinguish "unknown" from "expired" to
// the caller, so a failed verification cannot be used as an oracle to probe
// which failure mode occurred. The two remain distinct SENTINELS (not one
// merged error) purely so the app-layer Signer itself stays independently
// testable for each cause.
var (
	// ErrLinkExpired is returned when a signed deep link's embedded expiry has
	// passed as of the verification instant.
	ErrLinkExpired = errors.New("deeplink: signed link has expired")

	// ErrLinkInvalidSignature is returned when a signed deep link's signature
	// does not match its path, or is malformed (not valid base64url) — i.e. it
	// was never signed by this server, or was tampered with in transit.
	ErrLinkInvalidSignature = errors.New("deeplink: signed link has an invalid signature")
)
