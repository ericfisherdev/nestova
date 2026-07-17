// Package domain models a QR deep link's action vocabulary: the small,
// closed set of kiosk-initiated actions a member's phone can be sent to
// perform (claim a chore, complete a chore, add a chore, redeem a reward),
// and the sentinel errors returned when a signed link fails verification.
//
// A deep link's signature is NEVER an authorization grant by itself — it only
// makes the URL non-forgeable (so a stranger cannot construct one by
// guessing). The member's own session plus each target bounded context's
// existing domain rules (claim eligibility, point balance, tenant isolation,
// etc.) are what actually authorize the action; see [Action] and the
// internal/deeplink/app package for the signing/verification logic itself.
package domain
