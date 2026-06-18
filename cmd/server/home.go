package main

import (
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// seedMembers is the mock household (placeholder until NES-22 supplies real
// household members from the database).
var seedMembers = []components.MemberView{
	{Name: "Maya", Initials: "M", Color: "sage"},
	{Name: "Daniel", Initials: "D", Color: "clay"},
	{Name: "Ivy", Initials: "I", Color: "ochre"},
	{Name: "Leo", Initials: "L", Color: "blue"},
	{Name: "Family", Initials: "F", Color: "plum"},
}

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

// registerWebRoutes wires the user-facing pages. The dashboard requires
// authentication (RequireMember); auth routes (login, logout) are public.
func registerWebRoutes(mux *http.ServeMux, logger *slog.Logger, sm *scs.SessionManager, authHandlers *authadapter.Handlers) {
	// Auth routes — public.
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	mux.HandleFunc("POST /logout", authHandlers.Logout)

	// Dashboard — protected: RequireMember redirects unauthenticated visitors
	// to /login?next=/ before the handler runs.
	requireMember := authadapter.RequireMember(sm)
	mux.Handle("GET /{$}", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		props := components.ShellProps{
			Members:   seedMembers,
			CSRFToken: authadapter.GetCSRFToken(r.Context(), sm),
		}
		nav := primaryNav("") // dashboard home: no feature item active
		layout := func(c templ.Component) templ.Component {
			return components.Layout(props, nav, c)
		}
		if err := render.Page(r.Context(), w, r, layout, components.Dashboard()); err != nil {
			logger.ErrorContext(r.Context(), "render dashboard", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	})))
}
