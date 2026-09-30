package adapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"

	"github.com/alexedwards/scs/v2"
)

const (
	// FormTokenField is the name of the hidden input a create form embeds its
	// one-time token in.
	FormTokenField = "form_token"
	// sessionKeyFormTokens is the session key holding the one-time form
	// tokens issued to the member and not yet consumed.
	sessionKeyFormTokens = "form_tokens"
	// formTokenLen is the token length in bytes (a 32-character hex string).
	formTokenLen = 16
	// maxPendingFormTokens bounds the tokens kept per session so a member who
	// opens many forms without submitting them cannot grow the session
	// without limit. The oldest token is dropped first.
	maxPendingFormTokens = 16
)

// IssueFormToken mints a one-time token for a create form and records it in
// the session. The form embeds it as a hidden field; ConsumeFormToken spends
// it once. It returns "" only when crypto/rand fails, which makes every later
// check fail, the safe outcome (see GetCSRFToken).
func IssueFormToken(ctx context.Context, sm *scs.SessionManager) string {
	b := make([]byte, formTokenLen)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	token := hex.EncodeToString(b)

	pending := pendingFormTokens(ctx, sm)
	if len(pending) >= maxPendingFormTokens {
		pending = pending[len(pending)-maxPendingFormTokens+1:]
	}
	sm.Put(ctx, sessionKeyFormTokens, append(slices.Clone(pending), token))
	return token
}

// HasFormToken reports whether the request presents a form token that was
// issued and not yet consumed. It does not consume the token, so a submission
// that fails validation can be corrected and resubmitted.
func HasFormToken(r *http.Request, sm *scs.SessionManager) bool {
	presented := r.FormValue(FormTokenField)
	return presented != "" && slices.Contains(pendingFormTokens(r.Context(), sm), presented)
}

// ConsumeFormToken spends the token the request presents, so a second POST
// carrying it (Back, then resubmit) is rejected by HasFormToken. Call it only
// after the create succeeded. Consuming a token that is not pending is a
// no-op.
//
// The session is read and written per request without a lock, so two POSTs
// carrying the same token at the same instant can both pass HasFormToken, and
// a stale session write from another tab can bring a spent token back. This
// token closes the sequential resubmit only; a form that uses it must also
// stop a double click in the browser (the reward form disables its submit
// button on submit). An atomic claim would need the token persisted with the
// row it guards.
func ConsumeFormToken(r *http.Request, sm *scs.SessionManager) {
	presented := r.FormValue(FormTokenField)
	pending := pendingFormTokens(r.Context(), sm)
	remaining := slices.DeleteFunc(slices.Clone(pending), func(t string) bool { return t == presented })
	if len(remaining) != len(pending) {
		sm.Put(r.Context(), sessionKeyFormTokens, remaining)
	}
}

// pendingFormTokens returns the session's unconsumed form tokens, or nil.
func pendingFormTokens(ctx context.Context, sm *scs.SessionManager) []string {
	pending, _ := sm.Get(ctx, sessionKeyFormTokens).([]string)
	return pending
}
