package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"

	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// genericPINError is the message shown for a PIN verification failure that
// must not disclose whether the member has no PIN enrolled or entered the
// wrong one (NES-165 AC: "Verification errors never disclose whether a
// member is enrolled"). It is shown by this settings page and, through
// PINVerificationMessage, by the chore complete/skip surfaces that call
// PINService.AuthorizeTaskAction (NES-166).
const genericPINError = "That PIN could not be verified. Please try again."

// genericPINLockedError is shown while a member is in the PIN attempt
// limiter's backoff window. Unlike genericPINError, a lockout is
// deliberately visible rather than collapsed — this settings page shows it
// directly (SectionView's LockedUntilLabel) precisely so it never needs to
// be inferred from this message alone.
const genericPINLockedError = "Too many incorrect PINs. Please wait a few minutes and try again."

// pinInvalidFormatError is shown when a submitted PIN is not 4-8 digits.
const pinInvalidFormatError = "PINs must be 4 to 8 digits."

// PINVerificationMessage maps a PINService.Verify/AuthorizeTaskAction error
// to its user-facing message, collapsing authdomain.ErrPINMismatch and
// ErrPINNotEnrolled into the same genericPINError so a failed verification
// never discloses which member is enrolled; authdomain.ErrPINLocked is
// reported distinctly since the lockout itself must stay visible. ok is
// false for any error this function does not recognize (an internal error
// the caller must log and surface as a 500, not show to the member). It is
// exported because the tasks and deeplink adapters gate their chore
// complete/skip surfaces on the same errors (NES-166) and must render the
// identical non-disclosing text this settings page does.
func PINVerificationMessage(err error) (msg string, ok bool) {
	switch {
	case errors.Is(err, authdomain.ErrPINLocked):
		return genericPINLockedError, true
	case errors.Is(err, authdomain.ErrPINMismatch), errors.Is(err, authdomain.ErrPINNotEnrolled):
		return genericPINError, true
	default:
		return "", false
	}
}

// PINWebHandlers serves the auth context's per-member PIN section of the
// shared /settings page (NES-165): self-service set/clear for every
// member, and an owner/adult's set/reset for a child. Like MFAWebHandlers,
// it never writes an HTTP response for a mutation's success path — the
// composition root (cmd/server/home.go's registerSettingsPage) does, after
// calling SectionView here.
type PINWebHandlers struct {
	pin        *authapp.PINService
	households household.HouseholdRepository
	sm         *scs.SessionManager
	logger     *slog.Logger
}

// NewPINWebHandlers constructs PINWebHandlers with all required
// dependencies. It panics when any dependency is nil so misconfigured
// composition roots are caught at startup rather than at the first HTTP
// request.
func NewPINWebHandlers(pin *authapp.PINService, households household.HouseholdRepository, sm *scs.SessionManager, logger *slog.Logger) *PINWebHandlers {
	if pin == nil {
		panic("auth/adapter: NewPINWebHandlers requires a non-nil PINService")
	}
	if households == nil {
		panic("auth/adapter: NewPINWebHandlers requires a non-nil HouseholdRepository")
	}
	if sm == nil {
		panic("auth/adapter: NewPINWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("auth/adapter: NewPINWebHandlers requires a non-nil logger")
	}
	return &PINWebHandlers{pin: pin, households: households, sm: sm, logger: logger}
}

// SectionView builds the PIN section's view model for member — rendered
// for every member regardless of role. errMsg re-shows an inline failure
// message from a mutation on this same response.
func (h *PINWebHandlers) SectionView(ctx context.Context, member *household.Member, errMsg string) (components.PINSettingsView, error) {
	enrolled, err := h.pin.IsEnrolled(ctx, member.ID)
	if err != nil {
		return components.PINSettingsView{}, err
	}
	view := components.PINSettingsView{
		Enrolled:  enrolled,
		CSRFToken: GetCSRFToken(ctx, h.sm),
		Error:     errMsg,
	}
	if lockedUntil, locked := h.pin.LockedUntil(member.ID); locked {
		view.LockedUntilLabel = lockedUntil.Format(webauthnDisplayDateLayout)
	}

	if member.Role.CanAdminister() {
		others, err := h.households.ListMembers(ctx, member.HouseholdID)
		if err != nil {
			return components.PINSettingsView{}, err
		}
		enrolledIDs, err := h.pin.EnrolledMembers(ctx, member.HouseholdID)
		if err != nil {
			return components.PINSettingsView{}, err
		}
		enrolledSet := make(map[household.MemberID]bool, len(enrolledIDs))
		for _, id := range enrolledIDs {
			enrolledSet[id] = true
		}

		view.IsAdmin = true
		view.OtherMembers = make([]components.PINMemberOption, 0, len(others))
		for _, m := range others {
			if m.ID == member.ID {
				continue
			}
			option := components.PINMemberOption{ID: m.ID.String(), DisplayName: m.DisplayName, Enrolled: enrolledSet[m.ID]}
			if lockedUntil, locked := h.pin.LockedUntil(m.ID); locked {
				option.LockedUntilLabel = lockedUntil.Format(webauthnDisplayDateLayout)
			}
			view.OtherMembers = append(view.OtherMembers, option)
		}
	}

	return view, nil
}

// Set handles the mutation behind POST /settings/pin: a member sets or
// changes their own PIN. ok=false means a hard failure was already written
// directly; ok=true means the caller must compose the full page at status
// with errMsg embedded.
func (h *PINWebHandlers) Set(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, "", 0, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, "", 0, false
	}

	pin := strings.TrimSpace(r.FormValue("pin"))
	if err := h.pin.Set(r.Context(), member.ID, member.HouseholdID, pin); err != nil {
		if errors.Is(err, authdomain.ErrInvalidPINFormat) {
			return member, pinInvalidFormatError, http.StatusBadRequest, true
		}
		h.logger.ErrorContext(r.Context(), "pin: set", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	h.logger.InfoContext(r.Context(), "pin set", "member_id", member.ID.String())
	return member, "", http.StatusOK, true
}

// Clear handles the mutation behind POST /settings/pin/clear: a member
// removes their own PIN. Same ok/status contract as Set.
func (h *PINWebHandlers) Clear(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, "", 0, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, "", 0, false
	}

	if err := h.pin.Clear(r.Context(), member.ID, member.HouseholdID); err != nil {
		if errors.Is(err, authdomain.ErrPINNotEnrolled) {
			// Nothing to clear — a benign no-op, not a failure worth
			// showing an error for (e.g. a double-submitted form).
			return member, "", http.StatusOK, true
		}
		h.logger.ErrorContext(r.Context(), "pin: clear", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	h.logger.InfoContext(r.Context(), "pin cleared", "member_id", member.ID.String())
	return member, "", http.StatusOK, true
}

// SetForMember handles the mutation behind POST
// /settings/members/{id}/pin: an owner or adult sets or changes another
// member's (typically a child's) PIN. Gated on
// household.Role.CanAdminister(), mirroring
// kioskadapter.SettingsWebHandlers.requireParent's convention.
func (h *PINWebHandlers) SetForMember(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireAdmin(w, r)
	if !ok {
		return nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, "", 0, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, "", 0, false
	}

	targetID, err := household.ParseMemberID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		http.Error(w, "invalid member id", http.StatusBadRequest)
		return nil, "", 0, false
	}

	if !h.targetInHousehold(w, r, targetID, member.HouseholdID) {
		return nil, "", 0, false
	}

	pin := strings.TrimSpace(r.FormValue("pin"))
	if err := h.pin.SetForMember(r.Context(), targetID, member.HouseholdID, pin); err != nil {
		if errors.Is(err, authdomain.ErrInvalidPINFormat) {
			return member, pinInvalidFormatError, http.StatusBadRequest, true
		}
		if errors.Is(err, household.ErrMemberNotFound) {
			http.Error(w, "member not found", http.StatusNotFound)
			return nil, "", 0, false
		}
		h.logger.ErrorContext(r.Context(), "pin: set for member", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	h.logger.InfoContext(r.Context(), "pin set by parent", "member_id", targetID.String(), "acting_member_id", member.ID.String())
	return member, "", http.StatusOK, true
}

// ResetForMember handles the mutation behind POST
// /settings/members/{id}/pin/reset: an owner or adult clears another
// member's PIN AND any active lockout. Gated on
// household.Role.CanAdminister(), same as SetForMember.
func (h *PINWebHandlers) ResetForMember(w http.ResponseWriter, r *http.Request) (member *household.Member, errMsg string, status int, ok bool) {
	member, ok = h.requireAdmin(w, r)
	if !ok {
		return nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, "", 0, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, "", 0, false
	}

	targetID, err := household.ParseMemberID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		http.Error(w, "invalid member id", http.StatusBadRequest)
		return nil, "", 0, false
	}

	if !h.targetInHousehold(w, r, targetID, member.HouseholdID) {
		return nil, "", 0, false
	}

	if err := h.pin.ResetForMember(r.Context(), targetID, member.HouseholdID); err != nil && !errors.Is(err, authdomain.ErrPINNotEnrolled) {
		h.logger.ErrorContext(r.Context(), "pin: reset for member", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	h.logger.InfoContext(r.Context(), "pin reset by parent", "member_id", targetID.String(), "acting_member_id", member.ID.String())
	return member, "", http.StatusOK, true
}

// targetInHousehold reports whether targetID names a member of householdID,
// writing a 404 and returning false when it does not.
//
// Both admin routes take the target from the URL path, so without this an
// owner or adult of one household could act on a member id belonging to
// another — the roles gate WHO may administer, never WHOSE members. The
// repository is scoped too (SetPIN via the composite tenant FK, ClearPIN via
// its own predicate); this check is the layer that turns a cross-household
// id into an honest 404 instead of a silent no-op, and it keeps the
// lockout reset — which lives in memory, beyond any SQL predicate's reach —
// from being triggered for a member of another household.
func (h *PINWebHandlers) targetInHousehold(
	w http.ResponseWriter,
	r *http.Request,
	targetID household.MemberID,
	householdID household.HouseholdID,
) bool {
	target, err := h.households.GetMember(r.Context(), targetID)
	if err != nil {
		if errors.Is(err, household.ErrMemberNotFound) {
			http.Error(w, "member not found", http.StatusNotFound)
			return false
		}
		h.logger.ErrorContext(r.Context(), "pin: resolve target member", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return false
	}
	if target.HouseholdID != householdID {
		// Same response as an unknown id: an admin must not be able to tell a
		// foreign member id apart from one that does not exist.
		http.Error(w, "member not found", http.StatusNotFound)
		return false
	}
	return true
}

// requireMember resolves the current member — any role, since a PIN is a
// per-member self-service concern like MFA.
func (h *PINWebHandlers) requireMember(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// requireAdmin resolves the current member and enforces the
// owner-or-adult gate shared by both admin PIN mutations, mirroring
// kioskadapter.SettingsWebHandlers.requireParent's convention.
func (h *PINWebHandlers) requireAdmin(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := h.requireMember(w, r)
	if !ok {
		return nil, false
	}
	if !member.Role.CanAdminister() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	return member, true
}
