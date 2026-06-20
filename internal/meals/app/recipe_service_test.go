package app_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/app"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// fakeIngredientEnsurer is an in-memory tracking.IngredientEnsurer that normalizes
// names (trim + lower), assigns a stable id per canonical name, rejects blanks,
// and records the canonical names it was asked to ensure.
type fakeIngredientEnsurer struct {
	byName map[string]tracking.IngredientID
	calls  []string
}

func newFakeEnsurer() *fakeIngredientEnsurer {
	return &fakeIngredientEnsurer{byName: map[string]tracking.IngredientID{}}
}

func (f *fakeIngredientEnsurer) EnsureIngredient(_ context.Context, name string) (*tracking.Ingredient, error) {
	canonical := strings.ToLower(strings.TrimSpace(name))
	if canonical == "" {
		return nil, tracking.ErrInvalidIngredient
	}
	f.calls = append(f.calls, canonical)
	id, ok := f.byName[canonical]
	if !ok {
		id = tracking.NewIngredientID()
		f.byName[canonical] = id
	}
	return &tracking.Ingredient{ID: id, CanonicalName: canonical}, nil
}

// fakeRecipeRepo is an in-memory domain.RecipeRepository scoped by household.
type fakeRecipeRepo struct {
	recipes map[domain.RecipeID]*domain.Recipe
}

func newFakeRecipeRepo() *fakeRecipeRepo {
	return &fakeRecipeRepo{recipes: map[domain.RecipeID]*domain.Recipe{}}
}

func (f *fakeRecipeRepo) Create(_ context.Context, recipe *domain.Recipe) error {
	f.recipes[recipe.ID] = recipe
	return nil
}

func (f *fakeRecipeRepo) Update(_ context.Context, recipe *domain.Recipe) error {
	existing, ok := f.recipes[recipe.ID]
	if !ok || !sameHousehold(existing, recipe) {
		return domain.ErrRecipeNotFound
	}
	f.recipes[recipe.ID] = recipe
	return nil
}

func (f *fakeRecipeRepo) UpsertExternal(_ context.Context, recipe *domain.Recipe) (*domain.Recipe, error) {
	f.recipes[recipe.ID] = recipe
	return recipe, nil
}

func (f *fakeRecipeRepo) Get(_ context.Context, householdID household.HouseholdID, id domain.RecipeID) (*domain.Recipe, error) {
	recipe, ok := f.recipes[id]
	if !ok || recipe.HouseholdID == nil || *recipe.HouseholdID != householdID {
		return nil, domain.ErrRecipeNotFound
	}
	return recipe, nil
}

func (f *fakeRecipeRepo) Delete(_ context.Context, householdID household.HouseholdID, id domain.RecipeID) error {
	recipe, ok := f.recipes[id]
	if !ok || recipe.HouseholdID == nil || *recipe.HouseholdID != householdID {
		return domain.ErrRecipeNotFound
	}
	delete(f.recipes, id)
	return nil
}

func (f *fakeRecipeRepo) ListByHousehold(_ context.Context, householdID household.HouseholdID) ([]*domain.Recipe, error) {
	var out []*domain.Recipe
	for _, recipe := range f.recipes {
		if recipe.HouseholdID != nil && *recipe.HouseholdID == householdID {
			out = append(out, recipe)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func sameHousehold(a, b *domain.Recipe) bool {
	if a.HouseholdID == nil || b.HouseholdID == nil {
		return false
	}
	return *a.HouseholdID == *b.HouseholdID
}

func mustService(t *testing.T, repo domain.RecipeRepository, ensurer tracking.IngredientEnsurer) *app.RecipeService {
	t.Helper()
	svc, err := app.NewRecipeService(repo, ensurer)
	if err != nil {
		t.Fatalf("NewRecipeService: %v", err)
	}
	return svc
}

func line(name string, amount float64, unit household.Unit, optional bool) app.IngredientLine {
	return app.IngredientLine{Name: name, Amount: amount, Unit: unit, Optional: optional}
}

func TestNewRecipeServiceRejectsNilDeps(t *testing.T) {
	if _, err := app.NewRecipeService(nil, newFakeEnsurer()); err == nil {
		t.Error("NewRecipeService(nil repo) = nil error, want error")
	}
	if _, err := app.NewRecipeService(newFakeRecipeRepo(), nil); err == nil {
		t.Error("NewRecipeService(nil ensurer) = nil error, want error")
	}
}

func TestCreateRecipeNormalizesEachLine(t *testing.T) {
	repo := newFakeRecipeRepo()
	ensurer := newFakeEnsurer()
	svc := mustService(t, repo, ensurer)
	hh := household.NewHouseholdID()

	in := app.RecipeInput{
		Title: "Bread", Servings: 2,
		Ingredients: []app.IngredientLine{
			line("  Flour ", 500, household.UnitGram, false),
			line("Salt", 2, household.UnitGram, true),
		},
	}
	recipe, err := svc.CreateRecipe(context.Background(), hh, in)
	if err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}

	// EnsureIngredient is invoked once per line, with normalized names.
	if len(ensurer.calls) != 2 || ensurer.calls[0] != "flour" || ensurer.calls[1] != "salt" {
		t.Errorf("ensure calls = %v, want [flour salt]", ensurer.calls)
	}
	if len(recipe.Ingredients) != 2 {
		t.Fatalf("recipe ingredients = %d, want 2", len(recipe.Ingredients))
	}
	if recipe.Source != domain.SourceLocal || recipe.HouseholdID == nil || *recipe.HouseholdID != hh {
		t.Errorf("recipe = %+v, want local recipe owned by %v", recipe, hh)
	}
	// Each line carries the catalogue id the ensurer resolved.
	if recipe.Ingredients[0].IngredientID != ensurer.byName["flour"] {
		t.Errorf("line[0] ingredient id = %v, want flour id", recipe.Ingredients[0].IngredientID)
	}
	if stored := repo.recipes[recipe.ID]; stored == nil {
		t.Error("recipe was not persisted")
	}
}

func TestCreateRecipeValidationErrors(t *testing.T) {
	svc := mustService(t, newFakeRecipeRepo(), newFakeEnsurer())
	ctx := context.Background()
	hh := household.NewHouseholdID()
	validLine := []app.IngredientLine{line("flour", 1, household.UnitCount, false)}

	tests := []struct {
		name string
		in   app.RecipeInput
		want error
	}{
		{"blank title", app.RecipeInput{Title: "   ", Servings: 2, Ingredients: validLine}, domain.ErrInvalidRecipe},
		{"zero servings", app.RecipeInput{Title: "X", Servings: 0, Ingredients: validLine}, domain.ErrInvalidRecipe},
		{"blank ingredient", app.RecipeInput{Title: "X", Servings: 2, Ingredients: []app.IngredientLine{line("  ", 1, household.UnitCount, false)}}, tracking.ErrInvalidIngredient},
		{"bad quantity", app.RecipeInput{Title: "X", Servings: 2, Ingredients: []app.IngredientLine{line("flour", -1, household.UnitGram, false)}}, household.ErrInvalidQuantity},
		{"zero quantity", app.RecipeInput{Title: "X", Servings: 2, Ingredients: []app.IngredientLine{line("flour", 0, household.UnitGram, false)}}, household.ErrInvalidQuantity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateRecipe(ctx, hh, tc.in); !errors.Is(err, tc.want) {
				t.Errorf("CreateRecipe = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEditRecipeReplacesIngredients(t *testing.T) {
	repo := newFakeRecipeRepo()
	svc := mustService(t, repo, newFakeEnsurer())
	ctx := context.Background()
	hh := household.NewHouseholdID()

	created, err := svc.CreateRecipe(ctx, hh, app.RecipeInput{
		Title: "Cake", Servings: 2, Ingredients: []app.IngredientLine{line("flour", 200, household.UnitGram, false)},
	})
	if err != nil {
		t.Fatalf("CreateRecipe: %v", err)
	}

	edited, err := svc.EditRecipe(ctx, hh, created.ID, app.RecipeInput{
		Title: "Sweet Cake", Servings: 8, Ingredients: []app.IngredientLine{line("sugar", 100, household.UnitGram, false)},
	})
	if err != nil {
		t.Fatalf("EditRecipe: %v", err)
	}
	if edited.Title != "Sweet Cake" || edited.Servings != 8 {
		t.Errorf("edited = %q/%d, want Sweet Cake/8", edited.Title, edited.Servings)
	}
	if len(edited.Ingredients) != 1 {
		t.Fatalf("edited ingredients = %d, want 1 (replaced)", len(edited.Ingredients))
	}
	if stored := repo.recipes[created.ID]; stored.Title != "Sweet Cake" || len(stored.Ingredients) != 1 {
		t.Errorf("stored recipe not updated: %+v", stored)
	}
}

func TestEditRecipeUnknownReturnsNotFound(t *testing.T) {
	svc := mustService(t, newFakeRecipeRepo(), newFakeEnsurer())
	hh := household.NewHouseholdID()
	_, err := svc.EditRecipe(context.Background(), hh, domain.NewRecipeID(), app.RecipeInput{
		Title: "Ghost", Servings: 1, Ingredients: []app.IngredientLine{line("flour", 1, household.UnitCount, false)},
	})
	if !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("EditRecipe(unknown) = %v, want ErrRecipeNotFound", err)
	}
}

func TestDeleteAndListDelegate(t *testing.T) {
	repo := newFakeRecipeRepo()
	svc := mustService(t, repo, newFakeEnsurer())
	ctx := context.Background()
	hh := household.NewHouseholdID()

	a, err := svc.CreateRecipe(ctx, hh, app.RecipeInput{Title: "Apple Pie", Servings: 2, Ingredients: []app.IngredientLine{line("apple", 3, household.UnitCount, false)}})
	if err != nil {
		t.Fatalf("CreateRecipe(Apple Pie): %v", err)
	}
	if _, err := svc.CreateRecipe(ctx, hh, app.RecipeInput{Title: "Soup", Servings: 2, Ingredients: []app.IngredientLine{line("water", 1, household.UnitLiter, false)}}); err != nil {
		t.Fatalf("CreateRecipe(Soup): %v", err)
	}

	box, err := svc.ListRecipeBox(ctx, hh)
	if err != nil {
		t.Fatalf("ListRecipeBox: %v", err)
	}
	if len(box) != 2 || box[0].Title != "Apple Pie" || box[1].Title != "Soup" {
		t.Errorf("box = %v, want [Apple Pie, Soup]", titles(box))
	}

	if err := svc.DeleteRecipe(ctx, hh, a.ID); err != nil {
		t.Fatalf("DeleteRecipe: %v", err)
	}
	box, err = svc.ListRecipeBox(ctx, hh)
	if err != nil {
		t.Fatalf("ListRecipeBox after delete: %v", err)
	}
	if len(box) != 1 || box[0].Title != "Soup" {
		t.Errorf("box after delete = %v, want [Soup]", titles(box))
	}
	if err := svc.DeleteRecipe(ctx, hh, a.ID); !errors.Is(err, domain.ErrRecipeNotFound) {
		t.Errorf("DeleteRecipe(already gone) = %v, want ErrRecipeNotFound", err)
	}
}

func titles(recipes []*domain.Recipe) []string {
	out := make([]string, len(recipes))
	for i, r := range recipes {
		out[i] = r.Title
	}
	return out
}
