package adapter

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// mealsPath is the canonical page path; most mutations redirect back here.
const mealsPath = "/meals"

// groceriesPath is where grocery generation redirects so the member sees the
// newly added items.
const groceriesPath = "/groceries"

// LayoutFunc is the callback home.go passes to Page so the handler wraps its
// content in the app shell without knowing how the ShellProps/nav are built.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// WebHandlers holds the HTTP handler methods for the /meals UI: the recipe box,
// the weekly planner, and the ingredient-driven finder, plus their HTMX actions.
type WebHandlers struct {
	recipes *app.RecipeService
	planner *app.PlannerService
	finder  *app.FinderService
	grocery *app.GroceryFromPlanService
	namer   tracking.IngredientNamer
	sm      *scs.SessionManager
	logger  *slog.Logger
}

// NewWebHandlers constructs a WebHandlers with all required dependencies, panicking
// on a nil one so a misconfigured composition root fails at startup.
func NewWebHandlers(recipes *app.RecipeService, planner *app.PlannerService, finder *app.FinderService, grocery *app.GroceryFromPlanService, namer tracking.IngredientNamer, sm *scs.SessionManager, logger *slog.Logger) *WebHandlers {
	switch {
	case recipes == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil RecipeService")
	case planner == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil PlannerService")
	case finder == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil FinderService")
	case grocery == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil GroceryFromPlanService")
	case namer == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil IngredientNamer")
	case sm == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil session manager")
	case logger == nil:
		panic("meals/adapter: NewWebHandlers requires a non-nil logger")
	}
	return &WebHandlers{recipes: recipes, planner: planner, finder: finder, grocery: grocery, namer: namer, sm: sm, logger: logger}
}

// Page handles GET /meals. It builds the recipe box and the current week's plan and
// renders MealsPage into the app shell. The finder is never run here: the ad-hoc
// search normalizes names via EnsureIngredient (a write), so it must go through the
// CSRF-verified Finder POST rather than a state-changing GET.
func (h *WebHandlers) Page(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.renderPage(w, r, layoutFn, "", "")
	}
}

// Finder handles POST /meals/finder. CSRF-verified because the ad-hoc path can
// create catalogue ingredients; it renders the meals page with the ranked results.
func (h *WebHandlers) Finder(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := h.beginMutation(w, r)
		if !ok {
			return
		}
		h.renderPageForMember(w, r, layoutFn, member, r.FormValue("source"), r.FormValue("ingredients"))
	}
}

// renderPage resolves the current member, then renders the meals page (optionally
// with finder results).
func (h *WebHandlers) renderPage(w http.ResponseWriter, r *http.Request, layoutFn LayoutFunc, finderSource, finderIngredients string) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.renderPageForMember(w, r, layoutFn, member, finderSource, finderIngredients)
}

func (h *WebHandlers) renderPageForMember(w http.ResponseWriter, r *http.Request, layoutFn LayoutFunc, member *household.Member, finderSource, finderIngredients string) {
	view, err := h.buildView(r, member, finderSource, finderIngredients)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "meals: build view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	content := components.MealsPage(view)
	if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
		h.logger.ErrorContext(r.Context(), "meals: render page", "error", err)
	}
}

// CreateRecipe handles POST /meals/recipes: it normalizes the submitted ingredient
// lines and adds a box recipe.
func (h *WebHandlers) CreateRecipe(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	input, err := parseRecipeInput(r)
	if err != nil {
		http.Error(w, "invalid ingredient", http.StatusBadRequest)
		return
	}
	if _, err := h.recipes.CreateRecipe(r.Context(), member.HouseholdID, input); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, mealsPath)
}

// EditRecipe handles POST /meals/recipes/{id}: it rewrites a box recipe and
// replaces its ingredient set.
func (h *WebHandlers) EditRecipe(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	recipeID, err := domain.ParseRecipeID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid recipe id", http.StatusBadRequest)
		return
	}
	input, err := parseRecipeInput(r)
	if err != nil {
		http.Error(w, "invalid ingredient", http.StatusBadRequest)
		return
	}
	if _, err := h.recipes.EditRecipe(r.Context(), member.HouseholdID, recipeID, input); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, mealsPath)
}

// DeleteRecipe handles POST /meals/recipes/{id}/delete.
func (h *WebHandlers) DeleteRecipe(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	recipeID, err := domain.ParseRecipeID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid recipe id", http.StatusBadRequest)
		return
	}
	if err := h.recipes.DeleteRecipe(r.Context(), member.HouseholdID, recipeID); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, mealsPath)
}

// AssignMeal handles POST /meals/plan: it assigns a recipe to a (date, meal) slot.
func (h *WebHandlers) AssignMeal(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	date, err := parseDate(r.FormValue("date"))
	if err != nil {
		http.Error(w, "invalid date", http.StatusBadRequest)
		return
	}
	meal, err := domain.ParseMeal(r.FormValue("meal"))
	if err != nil {
		http.Error(w, "invalid meal", http.StatusBadRequest)
		return
	}
	recipeID, err := domain.ParseRecipeID(r.FormValue("recipe_id"))
	if err != nil {
		http.Error(w, "invalid recipe id", http.StatusBadRequest)
		return
	}
	if err := h.planner.AssignMeal(r.Context(), member.HouseholdID, date, meal, recipeID, parseServings(r.FormValue("servings"))); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, mealsPath)
}

// ClearMeal handles POST /meals/plan/clear: it removes the entry in a slot. An
// already-empty slot is treated as success (the goal is an empty slot).
func (h *WebHandlers) ClearMeal(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	date, err := parseDate(r.FormValue("date"))
	if err != nil {
		http.Error(w, "invalid date", http.StatusBadRequest)
		return
	}
	meal, err := domain.ParseMeal(r.FormValue("meal"))
	if err != nil {
		http.Error(w, "invalid meal", http.StatusBadRequest)
		return
	}
	if err := h.planner.ClearMeal(r.Context(), member.HouseholdID, date, meal); err != nil && !errors.Is(err, domain.ErrMealPlanEntryNotFound) {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, mealsPath)
}

// GenerateGroceries handles POST /meals/plan/generate: it adds the week's planned
// ingredients to the shopping list, then redirects to /groceries to show them.
func (h *WebHandlers) GenerateGroceries(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	weekStart, err := parseDate(r.FormValue("week_start"))
	if err != nil {
		http.Error(w, "invalid week", http.StatusBadRequest)
		return
	}
	if _, err := h.grocery.GenerateFromWeek(r.Context(), member.HouseholdID, weekStart); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}
