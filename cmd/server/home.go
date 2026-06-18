package main

import (
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// primaryNav returns the fixed sidebar navigation, marking the item whose href
// equals active (empty selects none).
func primaryNav(active string) []components.NavItem {
	defs := []components.NavItem{
		{Label: "Calendar", Href: "/calendar"},
		{Label: "Chores", Href: "/chores"},
		{Label: "Meals & Recipes", Href: "/meals"},
		{Label: "Groceries", Href: "/groceries"},
		{Label: "Photos", Href: "/photos"},
	}
	for i := range defs {
		defs[i].Active = defs[i].Href == active
	}
	return defs
}

// toMemberViews maps a slice of domain Members to the MemberView view model
// used by the app shell sidebar.
func toMemberViews(members []*household.Member) []components.MemberView {
	views := make([]components.MemberView, 0, len(members))
	for _, m := range members {
		views = append(views, components.MemberView{
			Name:     m.DisplayName,
			Initials: m.Initials(),
			Color:    m.Color.String(),
		})
	}
	return views
}

// registerWebRoutes wires the user-facing pages. The dashboard requires
// authentication (RequireMember); auth routes (login, logout, onboarding) are
// public. The household repository is required so the dashboard can load real
// members from the database.
func registerWebRoutes(
	mux *http.ServeMux,
	logger *slog.Logger,
	sm *scs.SessionManager,
	authHandlers *authadapter.Handlers,
	onboardingHandlers *authadapter.OnboardingHandlers,
	households household.HouseholdRepository,
) {
	// Auth routes — public.
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	mux.HandleFunc("POST /logout", authHandlers.Logout)

	// Onboarding routes — public (first-run guard enforced inside the handlers).
	mux.HandleFunc("GET /onboarding", onboardingHandlers.OnboardingPage)
	mux.HandleFunc("POST /onboarding", onboardingHandlers.Onboard)

	requireMember := authadapter.RequireMember(sm)

	// Add-member routes — RequireMember-gated.
	mux.Handle("GET /members/new", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, _ := authadapter.CurrentMember(r.Context())
		props, nav := dashboardShell(r, sm, member, households, logger, "")
		onboardingHandlers.NewMemberPage(w, r, props, nav)
	})))
	mux.Handle("POST /members", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, _ := authadapter.CurrentMember(r.Context())
		props, nav := dashboardShell(r, sm, member, households, logger, "")
		onboardingHandlers.AddMember(w, r, props, nav)
	})))

	// Dashboard — protected: RequireMember redirects unauthenticated visitors
	// to /login?next=/ before the handler runs.
	mux.Handle("GET /{$}", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, _ := authadapter.CurrentMember(r.Context())
		props, nav := dashboardShell(r, sm, member, households, logger, "")
		layout := func(c templ.Component) templ.Component {
			return components.Layout(props, nav, c)
		}
		if err := render.Page(r.Context(), w, r, layout, components.Dashboard()); err != nil {
			logger.ErrorContext(r.Context(), "render dashboard", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	})))
}

// dashboardShell builds the ShellProps and nav slice for a given protected
// page. It loads the household member list from the database so the sidebar
// Family section reflects real persisted members. On error it falls back to an
// empty member list rather than failing the entire request.
func dashboardShell(
	r *http.Request,
	sm *scs.SessionManager,
	currentMember *household.Member,
	households household.HouseholdRepository,
	logger *slog.Logger,
	activeNav string,
) (components.ShellProps, []components.NavItem) {
	var memberViews []components.MemberView
	if currentMember != nil {
		members, err := households.ListMembers(r.Context(), currentMember.HouseholdID)
		if err != nil {
			logger.ErrorContext(r.Context(), "dashboard: list members", "error", err)
		} else {
			memberViews = toMemberViews(members)
		}
	}
	props := components.ShellProps{
		Members:   memberViews,
		CSRFToken: authadapter.GetCSRFToken(r.Context(), sm),
	}
	return props, primaryNav(activeNav)
}
