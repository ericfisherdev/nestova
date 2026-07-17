// Package app holds the deeplink bounded context's application-layer logic:
// signing and verifying the QR deep links rendered on the kiosk (NES-129).
// It has no persistence of its own — a signed link is stateless (the path and
// expiry are carried in the URL itself, authenticated by an HMAC) — so there
// is no repository port here, only [Signer].
package app
