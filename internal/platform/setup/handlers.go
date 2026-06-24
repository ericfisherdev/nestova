package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"

	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

const (
	// sessionKeyCSRF stores the per-session CSRF token. A minimal local CSRF
	// implementation (rather than reusing the auth adapter's) keeps this platform
	// package independent of the auth bounded context.
	sessionKeyCSRF = "csrf_token"
	// csrfTokenLen is the CSRF token length in bytes (64-char hex string).
	csrfTokenLen = 32
	// SetupTokenEnv optionally gates the wizard behind a shared secret. When set,
	// the value must be supplied in the form; when unset, the setup screen is open
	// (the composition root logs a warning). First-run setup is expected on a
	// trusted network.
	SetupTokenEnv = "NESTOVA_SETUP_TOKEN"
)

// Applier runs the first-run setup action. *Service satisfies it; the indirection
// keeps the handlers testable with a fake and depending on a behaviour, not a
// concrete type (DIP).
type Applier interface {
	Apply(ctx context.Context, in Input) error
}

// Handlers serve the first-run setup wizard over plain HTTP, before any database
// or authenticated identity exists.
type Handlers struct {
	service    Applier
	sm         *scs.SessionManager
	logger     *slog.Logger
	onComplete func()
	setupToken string
}

// NewHandlers constructs the wizard handlers. All dependencies are required.
// onComplete is invoked once, after a successful setup, so the composition root
// can shut the setup server down and restart in normal mode; it must not block
// (the handler calls it after the response is written).
func NewHandlers(service Applier, sm *scs.SessionManager, logger *slog.Logger, onComplete func(), setupToken string) *Handlers {
	if service == nil {
		panic("setup: NewHandlers requires a non-nil Service")
	}
	if sm == nil {
		panic("setup: NewHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("setup: NewHandlers requires a non-nil logger")
	}
	if onComplete == nil {
		panic("setup: NewHandlers requires a non-nil onComplete callback")
	}
	return &Handlers{
		service:    service,
		sm:         sm,
		logger:     logger,
		onComplete: onComplete,
		setupToken: strings.TrimSpace(setupToken),
	}
}

// Register mounts the wizard routes on mux: the form, its submission, and a
// catch-all that redirects every other path to /setup so an unconfigured app
// only ever shows the wizard.
func (h *Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup", h.Page)
	mux.HandleFunc("POST /setup", h.Submit)
	mux.HandleFunc("/", h.redirectToSetup)
}

// redirectToSetup sends any non-setup, non-platform path to the wizard.
func (h *Handlers) redirectToSetup(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/setup", http.StatusSeeOther)
}

// Page handles GET /setup, rendering the connection form pre-filled with sensible
// local defaults and a CSRF token.
func (h *Handlers) Page(w http.ResponseWriter, r *http.Request) {
	form := components.SetupForm{
		CSRFToken:     h.csrfToken(r.Context()),
		Host:          "localhost",
		Port:          "5432",
		SSLMode:       "disable",
		TokenRequired: h.setupToken != "",
	}
	h.renderForm(w, r, http.StatusOK, form)
}

// Submit handles POST /setup: CSRF + optional setup-token checks, then runs the
// setup service. On success it renders the completion page and signals the
// composition root to restart in normal mode; on failure it re-renders the form
// with a stage-specific message (422) and never echoes the password back.
func (h *Handlers) Submit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !h.verifyCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	token := h.csrfToken(r.Context())
	formFromRequest := func(errMsg string) components.SetupForm {
		return components.SetupForm{
			CSRFToken: token,
			Host:      strings.TrimSpace(r.FormValue("host")),
			Port:      strings.TrimSpace(r.FormValue("port")),
			Database:  strings.TrimSpace(r.FormValue("database")),
			User:      strings.TrimSpace(r.FormValue("user")),
			SSLMode:   strings.TrimSpace(r.FormValue("sslmode")),
			// Never echo the raw DSN back: it may embed credentials, which would
			// then be exposed in the rendered HTML on a validation error.
			RawDSN:        "",
			TokenRequired: h.setupToken != "",
			Error:         errMsg,
		}
	}

	if h.setupToken != "" {
		supplied := strings.TrimSpace(r.FormValue("setup_token"))
		if subtle.ConstantTimeCompare([]byte(h.setupToken), []byte(supplied)) != 1 {
			h.renderForm(w, r, http.StatusForbidden, formFromRequest("Incorrect setup token."))
			return
		}
	}

	in := Input{
		Host:     r.FormValue("host"),
		Port:     r.FormValue("port"),
		Database: r.FormValue("database"),
		User:     r.FormValue("user"),
		Password: r.FormValue("password"),
		SSLMode:  r.FormValue("sslmode"),
		RawDSN:   r.FormValue("raw_dsn"),
	}
	if err := h.service.Apply(r.Context(), in); err != nil {
		status, msg := classifyApplyError(err)
		h.logger.WarnContext(r.Context(), "setup attempt failed", "error", err)
		h.renderForm(w, r, status, formFromRequest(msg))
		return
	}

	// render buffers the whole page and writes it before returning, so the client
	// has the completion page (with its meta-refresh to /onboarding) before
	// onComplete shuts the setup server down. onComplete is non-blocking.
	if err := render.Render(r.Context(), w, http.StatusOK, components.SetupCompletePage()); err != nil {
		h.logger.ErrorContext(r.Context(), "render setup complete page", "error", err)
	}
	h.logger.InfoContext(r.Context(), "first-run setup complete; restarting in normal mode")
	h.onComplete()
}

// classifyApplyError maps a Service.Apply error to an HTTP status and a
// user-facing message. All stages re-render the form (422) so the operator can
// correct and retry; the underlying error is logged, not shown.
func classifyApplyError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusUnprocessableEntity, "Please check the connection details and try again."
	case errors.Is(err, ErrConnect):
		return http.StatusUnprocessableEntity, "Could not connect to the database with those details. Check the host, port, and credentials."
	case errors.Is(err, ErrMigrate):
		return http.StatusUnprocessableEntity, "Connected, but could not initialize the database schema. See the server logs for details."
	default:
		return http.StatusInternalServerError, "Something went wrong completing setup. See the server logs for details."
	}
}

// renderForm renders the setup form component at the given status code.
func (h *Handlers) renderForm(w http.ResponseWriter, r *http.Request, status int, form components.SetupForm) {
	if err := render.Render(r.Context(), w, status, components.SetupPage(form)); err != nil {
		h.logger.ErrorContext(r.Context(), "render setup page", "error", err)
	}
}

// csrfToken returns the per-session CSRF token, generating and storing a new one
// when absent. A crypto/rand failure returns "" so the subsequent check fails
// closed (the safe outcome).
func (h *Handlers) csrfToken(ctx context.Context) string {
	if token := h.sm.GetString(ctx, sessionKeyCSRF); token != "" {
		return token
	}
	b := make([]byte, csrfTokenLen)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	token := hex.EncodeToString(b)
	h.sm.Put(ctx, sessionKeyCSRF, token)
	return token
}

// verifyCSRF constant-time compares the form token against the session token,
// returning false when either is absent.
func (h *Handlers) verifyCSRF(r *http.Request) bool {
	sessionToken := h.sm.GetString(r.Context(), sessionKeyCSRF)
	formToken := r.FormValue("csrf_token")
	if sessionToken == "" || formToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sessionToken), []byte(formToken)) == 1
}
