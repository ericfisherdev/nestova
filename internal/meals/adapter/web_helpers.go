package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// dateLayout is the YYYY-MM-DD form used for plan dates and date inputs.
const dateLayout = "2006-01-02"

// mealSlot pairs a Meal with its posted value and display label, in daily order.
type mealSlot struct {
	meal  domain.Meal
	value string
	label string
}

var mealSlots = []mealSlot{
	{domain.MealBreakfast, "breakfast", "Breakfast"},
	{domain.MealLunch, "lunch", "Lunch"},
	{domain.MealDinner, "dinner", "Dinner"},
	{domain.MealSnack, "snack", "Snack"},
}

// buildView loads the recipe box, the current week's plan, and (when finder query
// params are present) the finder results, resolving every ingredient id to its
// catalogue name for display.
func (h *WebHandlers) buildView(r *http.Request, member *household.Member) (components.MealsView, error) {
	ctx := r.Context()
	hh := member.HouseholdID

	recipes, err := h.recipes.ListRecipeBox(ctx, hh)
	if err != nil {
		return components.MealsView{}, err
	}

	source := r.URL.Query().Get("source")
	rawIngredients := r.URL.Query().Get("ingredients")
	matches, ran, err := h.runFinder(ctx, hh, source, rawIngredients)
	if err != nil {
		// The finder is optional: a failure must not take down the recipe box and
		// planner, so log it and render the page without finder results.
		h.logger.WarnContext(ctx, "meals: finder failed; rendering page without results", "error", err)
		matches, ran = nil, false
	}

	// Resolve all ingredient ids (recipe lines + finder "missing") to names once.
	idSet := make(map[tracking.IngredientID]struct{})
	for _, recipe := range recipes {
		for _, line := range recipe.Ingredients {
			idSet[line.IngredientID] = struct{}{}
		}
	}
	for _, match := range matches {
		for _, id := range match.Missing {
			idSet[id] = struct{}{}
		}
	}
	names, err := h.resolveNames(ctx, idSet)
	if err != nil {
		return components.MealsView{}, err
	}

	recipeViews := make([]components.MealRecipeView, 0, len(recipes))
	options := make([]components.MealRecipeOption, 0, len(recipes))
	titleByID := make(map[domain.RecipeID]string, len(recipes))
	for _, recipe := range recipes {
		titleByID[recipe.ID] = recipe.Title
		options = append(options, components.MealRecipeOption{ID: recipe.ID.String(), Title: recipe.Title})
		recipeViews = append(recipeViews, buildRecipeView(recipe, names))
	}

	weekStart := weekStartFor(time.Now().UTC())
	plan, err := h.planner.PlanForWeek(ctx, hh, weekStart)
	if err != nil {
		return components.MealsView{}, err
	}

	var finder *components.MealFinderView
	if ran {
		finder = &components.MealFinderView{
			Source:  source,
			Query:   rawIngredients,
			Matches: buildMatchViews(matches, names),
		}
	}

	return components.MealsView{
		CSRFToken:     authadapter.GetCSRFToken(ctx, h.sm),
		Units:         unitOptions(),
		Recipes:       recipeViews,
		RecipeOptions: options,
		Week:          buildWeekView(weekStart, plan, titleByID),
		Finder:        finder,
	}, nil
}

// runFinder runs the requested finder search, returning the matches and whether a
// search ran at all.
func (h *WebHandlers) runFinder(ctx context.Context, hh household.HouseholdID, source, rawIngredients string) ([]domain.RecipeMatch, bool, error) {
	switch source {
	case "pantry":
		matches, err := h.finder.FindFromPantry(ctx, hh)
		return matches, true, err
	case "ingredients":
		names := splitIngredients(rawIngredients)
		if len(names) == 0 {
			return nil, true, nil
		}
		matches, err := h.finder.FindFromIngredients(ctx, hh, names)
		return matches, true, err
	default:
		return nil, false, nil
	}
}

// resolveNames batch-maps ingredient ids to canonical names.
func (h *WebHandlers) resolveNames(ctx context.Context, idSet map[tracking.IngredientID]struct{}) (map[tracking.IngredientID]string, error) {
	if len(idSet) == 0 {
		return map[tracking.IngredientID]string{}, nil
	}
	ids := make([]tracking.IngredientID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	return h.namer.NamesByIDs(ctx, ids)
}

func buildRecipeView(recipe *domain.Recipe, names map[tracking.IngredientID]string) components.MealRecipeView {
	ingredients := make([]components.MealIngredientView, 0, len(recipe.Ingredients))
	editLines := make([]components.MealEditLineView, 0, len(recipe.Ingredients))
	for _, line := range recipe.Ingredients {
		name := ingredientName(line.IngredientID, names)
		ingredients = append(ingredients, components.MealIngredientView{
			Name: name, Quantity: formatQuantity(line.Quantity), Optional: line.Optional,
		})
		editLines = append(editLines, components.MealEditLineView{
			Name: name, Amount: formatAmount(line.Quantity.Amount), Unit: line.Quantity.Unit.String(), Optional: line.Optional,
		})
	}
	return components.MealRecipeView{
		ID: recipe.ID.String(), Title: recipe.Title, Instructions: recipe.Instructions,
		Servings: recipe.Servings, ServingsLabel: fmt.Sprintf("Serves %d", recipe.Servings),
		Ingredients: ingredients, EditLines: editLines,
	}
}

func buildWeekView(weekStart time.Time, plan []*domain.MealPlanEntry, titleByID map[domain.RecipeID]string) components.MealWeekView {
	type slotKey struct {
		date string
		meal domain.Meal
	}
	bySlot := make(map[slotKey]*domain.MealPlanEntry, len(plan))
	for _, entry := range plan {
		bySlot[slotKey{entry.Date.UTC().Format(dateLayout), entry.Meal}] = entry
	}

	days := make([]components.MealDayView, 0, 7)
	for i := 0; i < 7; i++ {
		day := weekStart.AddDate(0, 0, i)
		dateStr := day.Format(dateLayout)
		slots := make([]components.MealSlotView, 0, len(mealSlots))
		for _, ms := range mealSlots {
			slot := components.MealSlotView{Meal: ms.value, MealLabel: ms.label}
			if entry, ok := bySlot[slotKey{dateStr, ms.meal}]; ok {
				slot.Filled = true
				if title, ok := titleByID[entry.RecipeID]; ok {
					slot.RecipeTitle = title
				} else {
					slot.RecipeTitle = "Recipe"
				}
				slot.ServingsLabel = fmt.Sprintf("%d servings", entry.Servings)
			}
			slots = append(slots, slot)
		}
		days = append(days, components.MealDayView{Date: dateStr, Label: day.Format("Mon 2"), Slots: slots})
	}

	weekEnd := weekStart.AddDate(0, 0, 6)
	return components.MealWeekView{
		WeekStart:  weekStart.Format(dateLayout),
		RangeLabel: weekStart.Format("Jan 2") + " – " + weekEnd.Format("Jan 2"),
		Days:       days,
	}
}

func buildMatchViews(matches []domain.RecipeMatch, names map[tracking.IngredientID]string) []components.MealMatchView {
	out := make([]components.MealMatchView, 0, len(matches))
	for _, match := range matches {
		missing := make([]string, 0, len(match.Missing))
		for _, id := range match.Missing {
			missing = append(missing, ingredientName(id, names))
		}
		out = append(out, components.MealMatchView{
			Title:      match.Recipe.Title,
			MatchLabel: strconv.Itoa(int(match.MatchPct*100+0.5)) + "% match",
			Missing:    missing,
		})
	}
	return out
}

// parseRecipeInput zips the repeatable ingredient-line form arrays into a
// RecipeInput, skipping blank-name rows (a trailing empty line).
func parseRecipeInput(r *http.Request) (app.RecipeInput, error) {
	names := r.Form["ingredient_name"]
	amounts := r.Form["ingredient_amount"]
	units := r.Form["ingredient_unit"]
	optionals := r.Form["ingredient_optional"]

	lines := make([]app.IngredientLine, 0, len(names))
	for i, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		amount, _ := strconv.ParseFloat(strings.TrimSpace(at(amounts, i)), 64)
		unit, err := household.ParseUnit(strings.TrimSpace(at(units, i)))
		if err != nil {
			return app.RecipeInput{}, err
		}
		lines = append(lines, app.IngredientLine{
			Name: name, Amount: amount, Unit: unit, Optional: at(optionals, i) == "true",
		})
	}
	return app.RecipeInput{
		Title:        r.FormValue("title"),
		Servings:     parseServings(r.FormValue("servings")),
		Instructions: r.FormValue("instructions"),
		Ingredients:  lines,
	}, nil
}

// at returns the i-th element of s, or "" when out of range.
func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

// parseServings parses a servings count; a non-numeric value yields 0 so domain
// validation rejects it.
func parseServings(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return n
}

// parseDate parses a YYYY-MM-DD date as a UTC calendar date.
func parseDate(raw string) (time.Time, error) {
	return time.Parse(dateLayout, strings.TrimSpace(raw))
}

// splitIngredients splits a comma-separated ingredient list, dropping blanks.
func splitIngredients(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// weekStartFor returns the Sunday-at-midnight-UTC that starts t's week.
func weekStartFor(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -int(day.Weekday()))
}

func ingredientName(id tracking.IngredientID, names map[tracking.IngredientID]string) string {
	if name, ok := names[id]; ok && name != "" {
		return name
	}
	return "ingredient"
}

func formatQuantity(q household.Quantity) string {
	return formatAmount(q.Amount) + " " + q.Unit.String()
}

func formatAmount(amount float64) string {
	return strconv.FormatFloat(amount, 'f', -1, 64)
}

func unitOptions() []components.UnitOption {
	units := household.Units()
	opts := make([]components.UnitOption, 0, len(units))
	for _, unit := range units {
		opts = append(opts, components.UnitOption{Value: unit.String(), Label: unit.String()})
	}
	return opts
}

// beginMutation parses the form, verifies CSRF, and resolves the current member,
// shared by every POST handler.
func (h *WebHandlers) beginMutation(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, false
	}
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// respondAfterMutation refreshes the page: HX-Redirect for HTMX, a 303 otherwise.
func respondAfterMutation(w http.ResponseWriter, r *http.Request, target string) {
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleMutationError maps domain errors to HTTP status codes.
func (h *WebHandlers) handleMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrRecipeNotFound),
		errors.Is(err, domain.ErrMealPlanEntryNotFound),
		errors.Is(err, household.ErrHouseholdNotFound),
		errors.Is(err, tracking.ErrIngredientNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrInvalidRecipe),
		errors.Is(err, domain.ErrInvalidMealPlanEntry),
		errors.Is(err, household.ErrInvalidQuantity),
		errors.Is(err, household.ErrUnitMismatch),
		errors.Is(err, tracking.ErrInvalidIngredient):
		http.Error(w, "invalid request", http.StatusBadRequest)
	default:
		h.logger.ErrorContext(r.Context(), "meals: mutation error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
