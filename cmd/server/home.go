package main

import (
	"log/slog"
	"net/http"

	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// exampleMembers is placeholder seed data demonstrating the member avatars. It
// is replaced by real household members in NES-22.
var exampleMembers = []components.MemberView{
	{Name: "Maya", Initials: "M", Color: "sage"},
	{Name: "Daniel", Initials: "D", Color: "clay"},
	{Name: "Ivy", Initials: "I", Color: "ochre"},
	{Name: "Leo", Initials: "L", Color: "blue"},
	{Name: "Family", Initials: "F", Color: "plum"},
}

// registerExampleRoutes wires the NES-19 proof routes — a full page and an HTMX
// fragment, both rendered through the render seam. NES-21 replaces the page route
// with the real dashboard shell.
func registerExampleRoutes(mux *http.ServeMux, logger *slog.Logger) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if err := render.Page(r.Context(), w, r, components.ExamplePage, components.ExampleContent(exampleMembers)); err != nil {
			logger.ErrorContext(r.Context(), "render home page", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /fragment", func(w http.ResponseWriter, r *http.Request) {
		if err := render.Render(r.Context(), w, http.StatusOK, components.ExampleFragment()); err != nil {
			logger.ErrorContext(r.Context(), "render fragment", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	})
}
