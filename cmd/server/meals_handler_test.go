package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	mealsapp "github.com/ericfisherdev/nestova/internal/meals/app"
	mealsdomain "github.com/ericfisherdev/nestova/internal/meals/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	trackingdomain "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// mealsShoppingRecorder is a tracking.ShoppingListRepository that records the
// meal-plan items a generation adds, so a test can assert the side effect.
type mealsShoppingRecorder struct {
	mealPlanItems []*trackingdomain.ShoppingListItem
}

func (r *mealsShoppingRecorder) Add(context.Context, *trackingdomain.ShoppingListItem) error {
	return nil
}

func (r *mealsShoppingRecorder) AddRestockIfAbsent(context.Context, *trackingdomain.ShoppingListItem) (bool, error) {
	return true, nil
}

func (r *mealsShoppingRecorder) AddMealPlanIfAbsent(_ context.Context, item *trackingdomain.ShoppingListItem) (bool, error) {
	r.mealPlanItems = append(r.mealPlanItems, item)
	return true, nil
}

func (r *mealsShoppingRecorder) UpdateStatus(context.Context, household.HouseholdID, trackingdomain.ShoppingListItemID, trackingdomain.ItemStatus) (*trackingdomain.ShoppingListItem, error) {
	return nil, trackingdomain.ErrShoppingListItemNotFound
}

func (r *mealsShoppingRecorder) ListByStatus(context.Context, household.HouseholdID, trackingdomain.ItemStatus) ([]*trackingdomain.ShoppingListItem, error) {
	return nil, nil
}

// mealsRecipeRepo is an in-memory mealsdomain.RecipeRepository scoped by household.
type mealsRecipeRepo struct {
	recipes map[mealsdomain.RecipeID]*mealsdomain.Recipe
}

func newMealsRecipeRepo() *mealsRecipeRepo {
	return &mealsRecipeRepo{recipes: map[mealsdomain.RecipeID]*mealsdomain.Recipe{}}
}

func (r *mealsRecipeRepo) Create(_ context.Context, recipe *mealsdomain.Recipe) error {
	r.recipes[recipe.ID] = recipe
	return nil
}

func (r *mealsRecipeRepo) Update(_ context.Context, recipe *mealsdomain.Recipe) error {
	existing, ok := r.recipes[recipe.ID]
	if !ok || existing.HouseholdID == nil || recipe.HouseholdID == nil || *existing.HouseholdID != *recipe.HouseholdID {
		return mealsdomain.ErrRecipeNotFound
	}
	r.recipes[recipe.ID] = recipe
	return nil
}

func (r *mealsRecipeRepo) UpsertExternal(_ context.Context, recipe *mealsdomain.Recipe) (*mealsdomain.Recipe, error) {
	r.recipes[recipe.ID] = recipe
	return recipe, nil
}

func (r *mealsRecipeRepo) Get(_ context.Context, hh household.HouseholdID, id mealsdomain.RecipeID) (*mealsdomain.Recipe, error) {
	recipe, ok := r.recipes[id]
	if !ok || recipe.HouseholdID == nil || *recipe.HouseholdID != hh {
		return nil, mealsdomain.ErrRecipeNotFound
	}
	return recipe, nil
}

func (r *mealsRecipeRepo) Delete(_ context.Context, hh household.HouseholdID, id mealsdomain.RecipeID) error {
	recipe, ok := r.recipes[id]
	if !ok || recipe.HouseholdID == nil || *recipe.HouseholdID != hh {
		return mealsdomain.ErrRecipeNotFound
	}
	delete(r.recipes, id)
	return nil
}

func (r *mealsRecipeRepo) ListByHousehold(_ context.Context, hh household.HouseholdID) ([]*mealsdomain.Recipe, error) {
	var out []*mealsdomain.Recipe
	for _, recipe := range r.recipes {
		if recipe.HouseholdID != nil && *recipe.HouseholdID == hh {
			out = append(out, recipe)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

// mealsPlanRepo is an in-memory mealsdomain.MealPlanRepository keyed by slot.
type mealsPlanRepo struct {
	entries map[string]*mealsdomain.MealPlanEntry
}

func newMealsPlanRepo() *mealsPlanRepo {
	return &mealsPlanRepo{entries: map[string]*mealsdomain.MealPlanEntry{}}
}

func mealsSlotKey(hh household.HouseholdID, date time.Time, meal mealsdomain.Meal) string {
	return hh.String() + "|" + date.UTC().Format("2006-01-02") + "|" + meal.String()
}

func (r *mealsPlanRepo) Upsert(_ context.Context, entry *mealsdomain.MealPlanEntry) error {
	r.entries[mealsSlotKey(entry.HouseholdID, entry.Date, entry.Meal)] = entry
	return nil
}

func (r *mealsPlanRepo) Delete(_ context.Context, hh household.HouseholdID, date time.Time, meal mealsdomain.Meal) error {
	key := mealsSlotKey(hh, date, meal)
	if _, ok := r.entries[key]; !ok {
		return mealsdomain.ErrMealPlanEntryNotFound
	}
	delete(r.entries, key)
	return nil
}

func (r *mealsPlanRepo) ListByDateRange(_ context.Context, hh household.HouseholdID, start, end time.Time) ([]*mealsdomain.MealPlanEntry, error) {
	var out []*mealsdomain.MealPlanEntry
	for _, entry := range r.entries {
		if entry.HouseholdID == hh && !entry.Date.Before(start) && !entry.Date.After(end) {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out, nil
}

// mealsFakes bundles the in-memory repositories a meals test seeds and asserts on.
type mealsFakes struct {
	recipes  *mealsRecipeRepo
	plans    *mealsPlanRepo
	pantry   *fakeGroceryPantryRepo
	shopping *mealsShoppingRecorder
	catalog  *fakeIngredientCatalog
}

func newMealsFakes() *mealsFakes {
	return &mealsFakes{
		recipes:  newMealsRecipeRepo(),
		plans:    newMealsPlanRepo(),
		pantry:   newFakeGroceryPantryRepo(),
		shopping: &mealsShoppingRecorder{},
		catalog:  newFakeIngredientCatalog(),
	}
}

func buildMealsHandlers(fakes *mealsFakes, sm *scs.SessionManager, logger *slog.Logger) *mealsadapter.WebHandlers {
	recipeSvc, err := mealsapp.NewRecipeService(fakes.recipes, fakes.catalog)
	if err != nil {
		panic("buildMealsHandlers: " + err.Error())
	}
	plannerSvc, err := mealsapp.NewPlannerService(fakes.plans, fakes.recipes)
	if err != nil {
		panic("buildMealsHandlers: " + err.Error())
	}
	grocerySvc, err := mealsapp.NewGroceryFromPlanService(fakes.plans, fakes.recipes, fakes.shopping)
	if err != nil {
		panic("buildMealsHandlers: " + err.Error())
	}
	localSrc, err := mealsadapter.NewLocalRecipeSource(fakes.recipes)
	if err != nil {
		panic("buildMealsHandlers: " + err.Error())
	}
	finderSvc, err := mealsapp.NewFinderService(localSrc, fakes.pantry, fakes.catalog)
	if err != nil {
		panic("buildMealsHandlers: " + err.Error())
	}
	return mealsadapter.NewWebHandlers(recipeSvc, plannerSvc, finderSvc, grocerySvc, fakes.catalog, sm, logger)
}

// newTestMealsHandlers builds meals WebHandlers with fresh no-op fakes, used by the
// other route builders that need registerWebRoutes to compile but do not exercise
// /meals routes.
func newTestMealsHandlers(sm *scs.SessionManager, logger *slog.Logger) *mealsadapter.WebHandlers {
	return buildMealsHandlers(newMealsFakes(), sm, logger)
}

// buildMealsTestHandler wires a full http.Handler exercising the /meals routes
// under an authenticated session backed by the supplied fakes.
func buildMealsTestHandler(fakes *mealsFakes, member *household.Member) (http.Handler, *scs.SessionManager) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(
		householdRepo, testCredStore{}, testProvisioner{}, sm, logger,
	)

	recurringRepo := fakeRecurringTaskRepo{}
	instanceRepo := &fakeTaskInstanceRepo{}
	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo)
	if err != nil {
		panic("buildMealsTestHandler: " + err.Error())
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(
		taskService, recurringRepo, instanceRepo, householdRepo, sm, logger,
	)
	gamificationHandlers := newTestGamificationHandlers(instanceRepo, householdRepo, sm, logger)
	groceryHandlers := newTestGroceryHandlers(householdRepo, sm, logger)
	mealsHandlers := buildMealsHandlers(fakes, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers, gamificationHandlers, groceryHandlers, mealsHandlers)

	return sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	), sm
}

func seedMealsRecipe(fakes *mealsFakes, hh household.HouseholdID, title string) mealsdomain.RecipeID {
	id := mealsdomain.NewRecipeID()
	fakes.recipes.recipes[id] = &mealsdomain.Recipe{
		ID: id, HouseholdID: &hh, Title: title, Source: mealsdomain.SourceLocal, Servings: 2,
	}
	return id
}

func TestMealsPageRequiresAuth(t *testing.T) {
	handler, _ := buildMealsTestHandler(newMealsFakes(), testMember())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/meals", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /meals: status = %d, want 303", rec.Code)
	}
}

func TestMealsPageRendersForAuthedMember(t *testing.T) {
	member := testMember()
	fakes := newMealsFakes()
	seedMealsRecipe(fakes, member.HouseholdID, "Pancakes")
	handler, sm := buildMealsTestHandler(fakes, member)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/meals", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /meals: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Recipe box", "Pancakes", "Weekly planner", "What can I make?"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /meals body missing %q", want)
		}
	}
}

func TestMealsFinderRejectsMissingCSRF(t *testing.T) {
	member := testMember()
	handler, sm := buildMealsTestHandler(newMealsFakes(), member)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/meals/finder", strings.NewReader("source=ingredients&ingredients=flour"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /meals/finder without CSRF: status = %d, want 403", rec.Code)
	}
}

func TestMealsFinderPostRendersResults(t *testing.T) {
	member := testMember()
	fakes := newMealsFakes()
	// A recipe that needs only flour, so a flour-only search fully matches it.
	flour, _ := fakes.catalog.EnsureIngredient(context.Background(), "flour")
	recipeID := mealsdomain.NewRecipeID()
	fakes.recipes.recipes[recipeID] = &mealsdomain.Recipe{
		ID: recipeID, HouseholdID: &member.HouseholdID, Title: "Flatbread", Source: mealsdomain.SourceLocal, Servings: 2,
		Ingredients: []mealsdomain.RecipeIngredient{{IngredientID: flour.ID, Quantity: household.Quantity{Amount: 200, Unit: household.UnitGram}}},
	}
	handler, sm := buildMealsTestHandler(fakes, member)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	body := "csrf_token=" + csrf + "&source=ingredients&ingredients=flour"
	req := httptest.NewRequest(http.MethodPost, "/meals/finder", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /meals/finder: status = %d, want 200", rec.Code)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "Flatbread") || !strings.Contains(out, "100% match") {
		t.Errorf("finder result missing matched recipe; body has Flatbread=%v match=%v",
			strings.Contains(out, "Flatbread"), strings.Contains(out, "100% match"))
	}
}

func TestMealsAssignRejectsMissingCSRF(t *testing.T) {
	member := testMember()
	fakes := newMealsFakes()
	handler, sm := buildMealsTestHandler(fakes, member)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/meals/plan", strings.NewReader("date=2026-06-21&meal=dinner&servings=2"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /meals/plan without CSRF: status = %d, want 403", rec.Code)
	}
}

func TestMealsAssignSucceedsAndPersists(t *testing.T) {
	member := testMember()
	fakes := newMealsFakes()
	recipeID := seedMealsRecipe(fakes, member.HouseholdID, "Pancakes")
	handler, sm := buildMealsTestHandler(fakes, member)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	body := "csrf_token=" + csrf + "&date=2026-06-21&meal=dinner&recipe_id=" + recipeID.String() + "&servings=4"
	req := httptest.NewRequest(http.MethodPost, "/meals/plan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("assign: status = %d, want 200", rec.Code)
	}
	if loc := rec.Header().Get("HX-Redirect"); loc != "/meals" {
		t.Errorf("HX-Redirect = %q, want /meals", loc)
	}
	if len(fakes.plans.entries) != 1 {
		t.Errorf("stored plan entries = %d, want 1", len(fakes.plans.entries))
	}
}

func TestMealsGenerateGroceriesRedirectsToGroceries(t *testing.T) {
	member := testMember()
	fakes := newMealsFakes()
	ingredient, _ := fakes.catalog.EnsureIngredient(context.Background(), "flour")
	recipeID := mealsdomain.NewRecipeID()
	fakes.recipes.recipes[recipeID] = &mealsdomain.Recipe{
		ID: recipeID, HouseholdID: &member.HouseholdID, Title: "Bread", Source: mealsdomain.SourceLocal, Servings: 2,
		Ingredients: []mealsdomain.RecipeIngredient{{IngredientID: ingredient.ID, Quantity: household.Quantity{Amount: 100, Unit: household.UnitGram}}},
	}
	weekStart := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	day := time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC)
	fakes.plans.entries[mealsSlotKey(member.HouseholdID, day, mealsdomain.MealDinner)] = &mealsdomain.MealPlanEntry{
		ID: mealsdomain.NewMealPlanEntryID(), HouseholdID: member.HouseholdID, Date: day, Meal: mealsdomain.MealDinner, RecipeID: recipeID, Servings: 4,
	}

	handler, sm := buildMealsTestHandler(fakes, member)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	body := "csrf_token=" + csrf + "&week_start=" + weekStart.Format("2006-01-02")
	req := httptest.NewRequest(http.MethodPost, "/meals/plan/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("generate: status = %d, want 200", rec.Code)
	}
	if loc := rec.Header().Get("HX-Redirect"); loc != "/groceries" {
		t.Errorf("HX-Redirect = %q, want /groceries", loc)
	}
	if len(fakes.shopping.mealPlanItems) == 0 {
		t.Errorf("expected a meal_plan shopping item to be added, got none")
	}
}
