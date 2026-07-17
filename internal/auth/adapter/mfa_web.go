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
	"github.com/ericfisherdev/nestova/internal/platform/qrcode"
	"github.com/ericfisherdev/nestova/web/components"
)

// mfaQRModuleSize is the pixels-per-QR-module passed to qrcode.PNGDataURI for
// the enrollment QR, rendered at 200x200 CSS pixels (web/components/mfa_settings.templ)
// for a close-range scan by the member's own phone.
const mfaQRModuleSize = 6

// genericMFAError is the message shown for any MFA failure that must not
// leak which specific check failed (wrong code vs. wrong recovery code vs.
// no active enrollment), mirroring authdomain.ErrInvalidCredentials' no-user-
// enumeration convention for the login flow.
const genericMFAError = "That code could not be verified. Please try again."

// genericOwnerReauthError is shown when the household-owner admin reset
// fails, without distinguishing a wrong password from an invalid target
// member (no enumeration of which check failed).
const genericOwnerReauthError = "That could not be completed. Check your password and try again."

// MFAWebHandlers serves the auth context's per-member MFA section of the
// shared /settings page (NES-134): enrollment, confirmation, recovery codes,
// disenrollment, and the household-owner admin reset. Like
// kioskadapter.SettingsWebHandlers, it never writes an HTTP response for a
// mutation's success path that needs the OTHER section recomposed — the
// composition root (cmd/server/home.go's registerSettingsPage) does, after
// calling SectionView here. Disenroll and AdminReset succeed via a
// self-contained redirect (no reveal to compose), mirroring
// kioskadapter.SettingsWebHandlers.RevokeKioskToken.
type MFAWebHandlers struct {
	mfa        *authapp.MFAService
	households household.HouseholdRepository
	sm         *scs.SessionManager
	logger     *slog.Logger
}

// NewMFAWebHandlers constructs MFAWebHandlers with all required
// dependencies. It panics when any dependency is nil so misconfigured
// composition roots are caught at startup rather than at the first HTTP
// request.
func NewMFAWebHandlers(mfa *authapp.MFAService, households household.HouseholdRepository, sm *scs.SessionManager, logger *slog.Logger) *MFAWebHandlers {
	if mfa == nil {
		panic("auth/adapter: NewMFAWebHandlers requires a non-nil MFAService")
	}
	if households == nil {
		panic("auth/adapter: NewMFAWebHandlers requires a non-nil HouseholdRepository")
	}
	if sm == nil {
		panic("auth/adapter: NewMFAWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("auth/adapter: NewMFAWebHandlers requires a non-nil logger")
	}
	return &MFAWebHandlers{mfa: mfa, households: households, sm: sm, logger: logger}
}

// SectionView builds the MFA section's view model for member — rendered for
// every member regardless of role. enrollReveal and recoveryReveal embed a
// one-time reveal produced by THIS response only (nil on every other
// render); errMsg re-shows an inline failure message from a mutation on
// this same response.
func (h *MFAWebHandlers) SectionView(
	ctx context.Context,
	member *household.Member,
	enrollReveal *components.MFAEnrollReveal,
	recoveryReveal *components.MFARecoveryCodesReveal,
	errMsg string,
) (components.MFASettingsView, error) {
	enrollment, err := h.mfa.Status(ctx, member.ID)
	if err != nil {
		return components.MFASettingsView{}, err
	}

	view := components.MFASettingsView{
		Status:              mfaStatus(enrollment),
		EnrollReveal:        enrollReveal,
		RecoveryCodesReveal: recoveryReveal,
		CSRFToken:           GetCSRFToken(ctx, h.sm),
		Error:               errMsg,
	}

	if member.Role == household.RoleOwner {
		others, err := h.households.ListMembers(ctx, member.HouseholdID)
		if err != nil {
			return components.MFASettingsView{}, err
		}
		view.IsOwner = true
		view.OtherMembers = make([]components.MFAMemberOption, 0, len(others))
		for _, m := range others {
			if m.ID == member.ID {
				continue
			}
			view.OtherMembers = append(view.OtherMembers, components.MFAMemberOption{ID: m.ID.String(), DisplayName: m.DisplayName})
		}
	}

	return view, nil
}

// mfaStatus maps an enrollment (possibly nil) to its display status.
func mfaStatus(enrollment *authdomain.MFAEnrollment) components.MFAEnrollmentStatus {
	switch {
	case enrollment == nil:
		return components.MFAStatusNotEnrolled
	case enrollment.Confirmed():
		return components.MFAStatusActive
	default:
		return components.MFAStatusPending
	}
}

// Enroll handles the mutation behind POST /settings/mfa/enroll: it verifies
// CSRF, generates a fresh TOTP secret for the current member (replacing any
// existing UNCONFIRMED enrollment — see authapp.MFAService.BeginEnrollment),
// and renders the QR/manual-entry reveal. On any hard failure it writes the
// response directly and returns ok=false; on success it returns the member
// and the one-time reveal for the caller to compose into the full page.
func (h *MFAWebHandlers) Enroll(w http.ResponseWriter, r *http.Request) (member *household.Member, reveal *components.MFAEnrollReveal, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, nil, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, nil, false
	}

	secret, otpauthURL, err := h.mfa.BeginEnrollment(r.Context(), member.ID, member.HouseholdID, member.DisplayName)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "mfa: begin enrollment", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, nil, false
	}
	qr, err := qrcode.PNGDataURI(otpauthURL, mfaQRModuleSize)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "mfa: render enrollment qr", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, nil, false
	}
	return member, &components.MFAEnrollReveal{QRDataURI: qr, ManualEntrySecret: secret}, true
}

// Confirm handles the mutation behind POST /settings/mfa/confirm. ok=false
// means a hard failure was already written directly; ok=true means the
// caller must compose the full page at status with recoveryReveal/errMsg
// embedded (recoveryReveal is non-nil only on a successful confirm, which
// generates the member's first batch of recovery codes).
func (h *MFAWebHandlers) Confirm(w http.ResponseWriter, r *http.Request) (member *household.Member, recoveryReveal *components.MFARecoveryCodesReveal, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, "", 0, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, nil, "", 0, false
	}

	codes, err := h.mfa.ConfirmEnrollment(r.Context(), member.ID, r.FormValue("code"))
	if err != nil {
		if isExpectedMFAError(err) {
			return member, nil, genericMFAError, http.StatusUnauthorized, true
		}
		h.logger.ErrorContext(r.Context(), "mfa: confirm enrollment", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, nil, "", 0, false
	}
	return member, &components.MFARecoveryCodesReveal{Codes: codes}, "", http.StatusOK, true
}

// RegenerateRecoveryCodes handles the mutation behind POST
// /settings/mfa/recovery-codes/regenerate. Same ok/status contract as
// Confirm.
func (h *MFAWebHandlers) RegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) (member *household.Member, reveal *components.MFARecoveryCodesReveal, errMsg string, status int, ok bool) {
	member, ok = h.requireMember(w, r)
	if !ok {
		return nil, nil, "", 0, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, "", 0, false
	}
	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, nil, "", 0, false
	}

	codes, err := h.mfa.RegenerateRecoveryCodes(r.Context(), member.ID, r.FormValue("code"))
	if err != nil {
		if isExpectedMFAError(err) {
			return member, nil, genericMFAError, http.StatusUnauthorized, true
		}
		h.logger.ErrorContext(r.Context(), "mfa: regenerate recovery codes", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, nil, "", 0, false
	}
	return member, &components.MFARecoveryCodesReveal{Codes: codes}, "", http.StatusOK, true
}

// Disenroll handles the mutation behind POST /settings/mfa/disenroll: it
// verifies EITHER the submitted current TOTP code or an unused recovery
// code. ok=false means the caller does nothing further — either because a
// hard failure was written directly, or because a successful disenroll
// redirects on its own (mirroring
// kioskadapter.SettingsWebHandlers.RevokeKioskToken, since there is no
// reveal to compose). ok=true means a soft (credential) failure: the caller
// must compose the full page at status with errMsg embedded.
func (h *MFAWebHandlers) Disenroll(w http.ResponseWriter, r *http.Request, redirectTo string) (member *household.Member, errMsg string, status int, ok bool) {
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

	err := h.mfa.Disenroll(r.Context(), member.ID, member.HouseholdID, r.FormValue("totp_code"), r.FormValue("recovery_code"))
	if err != nil {
		if isExpectedMFAError(err) {
			return member, genericMFAError, http.StatusUnauthorized, true
		}
		h.logger.ErrorContext(r.Context(), "mfa: disenroll", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return nil, "", 0, false
	}
	h.logger.InfoContext(r.Context(), "mfa disenrolled", "member_id", member.ID.String())
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	return nil, "", 0, false
}

// AdminReset handles the mutation behind POST /settings/mfa/reset: the
// household owner resets another member's MFA (e.g. a lost-device recovery
// path), after re-entering their OWN password. Same ok/status contract as
// Disenroll — a successful reset redirects on its own; a failure that needs
// re-rendering the page (wrong password, unknown target member) returns
// ok=true with errMsg; a hard failure (CSRF, not the owner) is written
// directly.
func (h *MFAWebHandlers) AdminReset(w http.ResponseWriter, r *http.Request, redirectTo string) (member *household.Member, errMsg string, status int, ok bool) {
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
	if member.Role != household.RoleOwner {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, "", 0, false
	}

	targetID, err := household.ParseMemberID(strings.TrimSpace(r.FormValue("member_id")))
	if err != nil {
		return member, genericOwnerReauthError, http.StatusBadRequest, true
	}
	target, err := h.households.GetMember(r.Context(), targetID)
	if err != nil || target.HouseholdID != member.HouseholdID {
		// A target outside the owner's own household is treated identically
		// to an unknown member — no signal is given either way.
		return member, genericOwnerReauthError, http.StatusBadRequest, true
	}

	ownerPassword := r.FormValue("owner_password")
	resetErr := h.mfa.ResetMemberMFA(r.Context(), member.ID, member.Role, ownerPassword, member.HouseholdID, targetID)
	if resetErr != nil {
		switch {
		case errors.Is(resetErr, authdomain.ErrOwnerReauthRequired), errors.Is(resetErr, authdomain.ErrMFANotEnrolled):
			return member, genericOwnerReauthError, http.StatusUnauthorized, true
		case errors.Is(resetErr, authdomain.ErrNotHouseholdOwner):
			http.Error(w, "forbidden", http.StatusForbidden)
			return nil, "", 0, false
		default:
			h.logger.ErrorContext(r.Context(), "mfa: admin reset", "error", resetErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return nil, "", 0, false
		}
	}
	h.logger.InfoContext(r.Context(), "mfa reset by household owner", "member_id", targetID.String(), "owner_id", member.ID.String())
	http.Redirect(w, r, redirectTo, http.StatusSeeOther)
	return nil, "", 0, false
}

// requireMember resolves the current member — any role, unlike
// kioskadapter.SettingsWebHandlers.requireParent — since MFA is a per-member
// self-service concern (NES-134).
func (h *MFAWebHandlers) requireMember(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	member, ok := CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// isExpectedMFAError reports whether err is one of the well-known,
// user-facing MFA verification failures (as opposed to an internal error
// that should be logged and surfaced as a 500).
func isExpectedMFAError(err error) bool {
	return errors.Is(err, authdomain.ErrMFANotEnrolled) ||
		errors.Is(err, authdomain.ErrMFAAlreadyEnrolled) ||
		errors.Is(err, authdomain.ErrInvalidTOTPCode) ||
		errors.Is(err, authdomain.ErrRecoveryCodeInvalid) ||
		errors.Is(err, authdomain.ErrMFAVerificationRequired)
}
