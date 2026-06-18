package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"

	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// minPasswordLen is the minimum accepted password length for onboarding and
// member provisioning.
const minPasswordLen = 8

// credentialStore is the minimal outbound port used by OnboardingHandlers for
// credential reads. It is satisfied by *CredentialRepository and by test fakes,
// keeping OnboardingHandlers decoupled from the concrete pgx type. Credential
// writes go through the Provisioner so they join the same atomic transaction as
// the member insert.
type credentialStore interface {
	// EmailExists reports whether any member already owns the given email
	// address. Used as a pre-check to surface a friendly "already in use"
	// message before attempting a write.
	EmailExists(ctx context.Context, email string) (bool, error)
}

// Provisioner performs the atomic, multi-table writes that back onboarding and
// member creation. Each method runs as a single transaction so a partial
// failure (e.g. member inserted but credentials not stored) cannot leave the
// database in an inconsistent state. The implementation lives in the
// composition root (cmd/server) so this package does not import the household
// adapter, preserving bounded-context independence.
type Provisioner interface {
	// ProvisionHousehold creates the household, adds the owner member, and stores
	// the owner's credentials in one transaction. It surfaces
	// household.ErrDuplicateMember and authdomain.ErrEmailAlreadyInUse from the
	// underlying repositories unchanged.
	ProvisionHousehold(ctx context.Context, hh *household.Household, owner *household.Member, email, passwordHash string) error
	// ProvisionMember adds the member and, when email is non-empty, stores
	// credentials in one transaction. An empty email means no credentials are
	// written. It surfaces household.ErrDuplicateMember and
	// authdomain.ErrEmailAlreadyInUse unchanged.
	ProvisionMember(ctx context.Context, m *household.Member, email, passwordHash string) error
}

// OnboardingHandlers contains the HTTP handler methods for first-run household
// setup (GET /onboarding, POST /onboarding) and authenticated member
// provisioning (GET /members/new, POST /members).
type OnboardingHandlers struct {
	households  household.HouseholdRepository
	creds       credentialStore
	provisioner Provisioner
	sm          *scs.SessionManager
	logger      *slog.Logger
}

// NewOnboardingHandlers constructs OnboardingHandlers. All dependencies are
// required; missing any panics at construction time (fail-fast, not at request
// time). households backs the read-side first-run guard and member listing,
// creds backs the email pre-check, and provisioner performs the atomic writes.
func NewOnboardingHandlers(
	households household.HouseholdRepository,
	creds credentialStore,
	provisioner Provisioner,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *OnboardingHandlers {
	if households == nil {
		panic("adapter: NewOnboardingHandlers requires a non-nil HouseholdRepository")
	}
	if creds == nil {
		panic("adapter: NewOnboardingHandlers requires a non-nil credentialStore")
	}
	if provisioner == nil {
		panic("adapter: NewOnboardingHandlers requires a non-nil Provisioner")
	}
	if sm == nil {
		panic("adapter: NewOnboardingHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("adapter: NewOnboardingHandlers requires a non-nil logger")
	}
	return &OnboardingHandlers{
		households:  households,
		creds:       creds,
		provisioner: provisioner,
		sm:          sm,
		logger:      logger,
	}
}

// OnboardingPage handles GET /onboarding. When a household already exists the
// setup is complete, so it redirects to /login. Otherwise it renders the
// first-run form with a CSRF token.
func (h *OnboardingHandlers) OnboardingPage(w http.ResponseWriter, r *http.Request) {
	exists, err := h.households.HasAnyHousehold(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "onboarding: check household", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if exists {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	token := GetCSRFToken(r.Context(), h.sm)
	h.renderOnboardingPage(w, r, http.StatusOK, components.OnboardingForm{CSRFToken: token})
}

// Onboard handles POST /onboarding. It enforces CSRF, re-checks the first-run
// guard to block a second household (open-registration guard), validates the
// form, creates the household and owner member, hashes the password, stores
// credentials, then signs the owner in and redirects to /.
func (h *OnboardingHandlers) Onboard(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	// Re-check the first-run guard: if a household was created between the GET
	// and this POST, refuse to create a second one via the public route.
	exists, err := h.households.HasAnyHousehold(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "onboarding: re-check household", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if exists {
		// 409 Conflict: setup already done; direct the user to login.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	householdName := strings.TrimSpace(r.FormValue("household_name"))
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	token := GetCSRFToken(r.Context(), h.sm)

	if validationErr := validateOnboardingForm(householdName, displayName, email, password); validationErr != "" {
		h.renderOnboardingPage(w, r, http.StatusUnprocessableEntity, components.OnboardingForm{
			CSRFToken:     token,
			HouseholdName: householdName,
			DisplayName:   displayName,
			Email:         email,
			Error:         validationErr,
		})
		return
	}

	hash, err := crypto.Hash(password)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "onboarding: hash password", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	hh := &household.Household{
		ID:   household.NewHouseholdID(),
		Name: householdName,
	}
	owner := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: hh.ID,
		DisplayName: displayName,
		Role:        household.RoleOwner,
		Color:       household.NextColor(nil),
	}

	// Single transaction: household + owner member + credentials. A partial
	// failure rolls back entirely, so onboarding never leaves an orphaned
	// household or a member without credentials.
	if err := h.provisioner.ProvisionHousehold(r.Context(), hh, owner, email, hash); err != nil {
		if errors.Is(err, household.ErrHouseholdExists) {
			// Lost the first-run onboarding race: a household now exists, so
			// setup is complete — send the user to login.
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if errors.Is(err, authdomain.ErrEmailAlreadyInUse) {
			h.renderOnboardingPage(w, r, http.StatusConflict, components.OnboardingForm{
				CSRFToken:     token,
				HouseholdName: householdName,
				DisplayName:   displayName,
				Email:         email,
				Error:         "That email address is already in use.",
			})
			return
		}
		h.logger.ErrorContext(r.Context(), "onboarding: provision household", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Renew the session token on privilege escalation to prevent session fixation.
	if err := h.sm.RenewToken(r.Context()); err != nil {
		h.logger.ErrorContext(r.Context(), "onboarding: renew session token", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.sm.Put(r.Context(), sessionKeyMemberID, owner.ID.String())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// NewMemberPage handles GET /members/new (RequireMember-gated). It renders the
// add-member form with a CSRF token inside the app shell.
func (h *OnboardingHandlers) NewMemberPage(
	w http.ResponseWriter,
	r *http.Request,
	props components.ShellProps,
	nav []components.NavItem,
) {
	token := GetCSRFToken(r.Context(), h.sm)
	form := components.AddMemberForm{CSRFToken: token}
	h.renderAddMemberPage(w, r, http.StatusOK, props, nav, form)
}

// AddMember handles POST /members (RequireMember-gated). It validates the form,
// creates the member within the current household, and optionally stores
// login credentials when email and password are both supplied. On success it
// redirects to /.
func (h *OnboardingHandlers) AddMember(
	w http.ResponseWriter,
	r *http.Request,
	props components.ShellProps,
	nav []components.NavItem,
) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	currentMember, ok := CurrentMember(r.Context())
	if !ok {
		// RequireMember should prevent reaching here, but guard defensively.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	displayName := strings.TrimSpace(r.FormValue("display_name"))
	roleStr := r.FormValue("role")
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	token := GetCSRFToken(r.Context(), h.sm)

	role, parseErr := household.ParseRole(roleStr)
	if parseErr != nil || (role != household.RoleAdult && role != household.RoleChild) {
		h.renderAddMemberPage(w, r, http.StatusUnprocessableEntity, props, nav, components.AddMemberForm{
			CSRFToken:   token,
			DisplayName: displayName,
			Role:        roleStr,
			Email:       email,
			Error:       "Please choose a valid role (adult or child).",
		})
		return
	}

	if validationErr := validateAddMemberForm(displayName, email, password); validationErr != "" {
		h.renderAddMemberPage(w, r, http.StatusUnprocessableEntity, props, nav, components.AddMemberForm{
			CSRFToken:   token,
			DisplayName: displayName,
			Role:        roleStr,
			Email:       email,
			Error:       validationErr,
		})
		return
	}

	// Pre-check the email before creating anything, so a duplicate email is
	// reported without inserting an orphan member that then has to be cleaned
	// up. The unique constraint inside ProvisionMember remains the authoritative
	// guard against the residual race between this check and the write.
	var hash string
	if email != "" {
		inUse, err := h.creds.EmailExists(r.Context(), email)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "add member: email exists check", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if inUse {
			h.renderAddMemberPage(w, r, http.StatusConflict, props, nav, components.AddMemberForm{
				CSRFToken:   token,
				DisplayName: displayName,
				Role:        roleStr,
				Email:       email,
				Error:       "That email address is already in use by another member.",
			})
			return
		}

		var hashErr error
		hash, hashErr = crypto.Hash(password)
		if hashErr != nil {
			h.logger.ErrorContext(r.Context(), "add member: hash password", "error", hashErr)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	// Gather existing colors to assign the next one deterministically.
	existing, err := h.households.ListMembers(r.Context(), currentMember.HouseholdID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "add member: list existing members", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	colors := make([]household.MemberColor, 0, len(existing))
	for _, m := range existing {
		colors = append(colors, m.Color)
	}

	member := &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: currentMember.HouseholdID,
		DisplayName: displayName,
		Role:        role,
		Color:       household.NextColor(colors),
	}

	// Single transaction: member insert + optional credentials. A failure rolls
	// back entirely, so a member is never persisted without its credentials.
	if err := h.provisioner.ProvisionMember(r.Context(), member, email, hash); err != nil {
		switch {
		case errors.Is(err, household.ErrDuplicateMember):
			h.renderAddMemberPage(w, r, http.StatusConflict, props, nav, components.AddMemberForm{
				CSRFToken:   token,
				DisplayName: displayName,
				Role:        roleStr,
				Email:       email,
				Error:       "A member with that name already exists in your household.",
			})
			return
		case errors.Is(err, authdomain.ErrEmailAlreadyInUse):
			h.renderAddMemberPage(w, r, http.StatusConflict, props, nav, components.AddMemberForm{
				CSRFToken:   token,
				DisplayName: displayName,
				Role:        roleStr,
				Email:       email,
				Error:       "That email address is already in use by another member.",
			})
			return
		default:
			h.logger.ErrorContext(r.Context(), "add member: provision member", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderOnboardingPage renders the onboarding page at the given status code.
func (h *OnboardingHandlers) renderOnboardingPage(w http.ResponseWriter, r *http.Request, status int, form components.OnboardingForm) {
	if err := render.Render(r.Context(), w, status, components.OnboardingPage(form)); err != nil {
		h.logger.ErrorContext(r.Context(), "render onboarding page", "error", err)
	}
}

// renderAddMemberPage renders the add-member page at the given status code.
func (h *OnboardingHandlers) renderAddMemberPage(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	props components.ShellProps,
	nav []components.NavItem,
	form components.AddMemberForm,
) {
	if err := render.Render(r.Context(), w, status, components.AddMemberPage(props, nav, form)); err != nil {
		h.logger.ErrorContext(r.Context(), "render add member page", "error", err)
	}
}

// validateOnboardingForm returns a human-readable error message for the first
// validation failure found, or an empty string when the form is valid.
func validateOnboardingForm(householdName, displayName, email, password string) string {
	switch {
	case householdName == "":
		return "Household name is required."
	case displayName == "":
		return "Your name is required."
	case email == "":
		return "Email is required."
	case !strings.Contains(email, "@"):
		return "Please enter a valid email address."
	case len(password) < minPasswordLen:
		return "Password must be at least 8 characters."
	default:
		return ""
	}
}

// validateAddMemberForm returns a human-readable error message for the first
// validation failure found, or an empty string when the form is valid.
// Email and password are both optional, but must be supplied together.
func validateAddMemberForm(displayName, email, password string) string {
	switch {
	case displayName == "":
		return "Display name is required."
	case (email == "") != (password == ""):
		return "Provide both email and password, or leave both blank."
	case email != "" && !strings.Contains(email, "@"):
		return "Please enter a valid email address."
	case email != "" && len(password) < minPasswordLen:
		return "Password must be at least 8 characters."
	default:
		return ""
	}
}
