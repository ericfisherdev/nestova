package adapter_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// capturingRecipeRepo records UpsertExternal calls and echoes the recipe back (as
// the real adapter does, with the id preserved).
type capturingRecipeRepo struct {
	upserted []*domain.Recipe
}

func (r *capturingRecipeRepo) Create(context.Context, *domain.Recipe) error { return nil }
func (r *capturingRecipeRepo) Update(context.Context, *domain.Recipe) error { return nil }
func (r *capturingRecipeRepo) UpsertExternal(_ context.Context, recipe *domain.Recipe) (*domain.Recipe, error) {
	r.upserted = append(r.upserted, recipe)
	return recipe, nil
}

func (r *capturingRecipeRepo) Get(context.Context, household.HouseholdID, domain.RecipeID) (*domain.Recipe, error) {
	return nil, domain.ErrRecipeNotFound
}

func (r *capturingRecipeRepo) Delete(context.Context, household.HouseholdID, domain.RecipeID) error {
	return nil
}

func (r *capturingRecipeRepo) ListByHousehold(context.Context, household.HouseholdID) ([]*domain.Recipe, error) {
	return nil, nil
}

type fakeEnsurer struct {
	byName map[string]tracking.IngredientID
}

func newFakeEnsurer() *fakeEnsurer {
	return &fakeEnsurer{byName: map[string]tracking.IngredientID{}}
}

func (f *fakeEnsurer) EnsureIngredient(_ context.Context, name string) (*tracking.Ingredient, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return nil, tracking.ErrInvalidIngredient
	}
	id, ok := f.byName[n]
	if !ok {
		id = tracking.NewIngredientID()
		f.byName[n] = id
	}
	return &tracking.Ingredient{ID: id, CanonicalName: n}, nil
}

type fakeNamer struct {
	names map[tracking.IngredientID]string
}

func (f fakeNamer) NamesByIDs(_ context.Context, ids []tracking.IngredientID) (map[tracking.IngredientID]string, error) {
	out := make(map[tracking.IngredientID]string, len(ids))
	for _, id := range ids {
		if n, ok := f.names[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

func newExternalSource(t *testing.T, baseURL string, repo domain.RecipeRepository, ensurer tracking.IngredientEnsurer, namer tracking.IngredientNamer) *adapter.ExternalRecipeSource {
	t.Helper()
	src, err := adapter.NewExternalRecipeSource(&http.Client{Timeout: 5 * time.Second}, baseURL, "test-key", repo, ensurer, namer)
	if err != nil {
		t.Fatalf("NewExternalRecipeSource: %v", err)
	}
	return src
}

func TestExternalRecipeSourceMapsAndCaches(t *testing.T) {
	flour := tracking.NewIngredientID()
	var gotIngredients, gotAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIngredients = r.URL.Query().Get("ingredients")
		gotAPIKey = r.URL.Query().Get("apiKey")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":42,"title":"Discovered Pasta",
			"usedIngredients":[{"name":"flour"}],
			"missedIngredients":[{"name":"Eggs"},{"name":"Tomato"}]}]`))
	}))
	defer server.Close()

	repo := &capturingRecipeRepo{}
	ensurer := newFakeEnsurer()
	namer := fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}}
	src := newExternalSource(t, server.URL, repo, ensurer, namer)

	matches, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour})
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}

	if gotIngredients != "flour" || gotAPIKey != "test-key" {
		t.Errorf("request ingredients=%q apiKey=%q, want flour/test-key", gotIngredients, gotAPIKey)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(matches))
	}
	m := matches[0]
	// 1 used, 2 missed -> 1/3.
	if m.MatchPct < 0.33 || m.MatchPct > 0.34 {
		t.Errorf("MatchPct = %v, want ~0.333 (1 used / 3 total)", m.MatchPct)
	}
	if len(m.Missing) != 2 {
		t.Errorf("Missing = %d, want 2 (eggs, tomato)", len(m.Missing))
	}
	if m.Recipe.Source != domain.SourceExternal || m.Recipe.ExternalRef == nil || *m.Recipe.ExternalRef != "spoonacular-42" {
		t.Errorf("recipe = %+v, want external with ref spoonacular-42", m.Recipe)
	}
	// The result was cached as an external recipe.
	if len(repo.upserted) != 1 || repo.upserted[0].Title != "Discovered Pasta" {
		t.Errorf("upserted = %+v, want one cached Discovered Pasta", repo.upserted)
	}
}

func TestExternalRecipeSourceEmptyHaveReturnsNil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("provider should not be called with no ingredients")
	}))
	defer server.Close()

	src := newExternalSource(t, server.URL, &capturingRecipeRepo{}, newFakeEnsurer(), fakeNamer{})
	matches, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), nil)
	if err != nil || matches != nil {
		t.Errorf("FindByIngredients(empty) = (%v, %v), want (nil, nil)", matches, err)
	}
}

func TestExternalRecipeSourceProviderErrorPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	flour := tracking.NewIngredientID()
	src := newExternalSource(t, server.URL, &capturingRecipeRepo{}, newFakeEnsurer(),
		fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}})
	if _, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour}); err == nil {
		t.Error("FindByIngredients with 500 = nil error, want error")
	}
}

func TestExternalRecipeSourceMalformedJSONPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer server.Close()

	flour := tracking.NewIngredientID()
	src := newExternalSource(t, server.URL, &capturingRecipeRepo{}, newFakeEnsurer(),
		fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}})
	if _, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour}); err == nil {
		t.Error("FindByIngredients with malformed JSON = nil error, want decode error")
	}
}

func TestNewExternalRecipeSourceGuards(t *testing.T) {
	client := &http.Client{}
	repo := &capturingRecipeRepo{}
	ensurer := newFakeEnsurer()
	namer := fakeNamer{}
	cases := []struct {
		name            string
		client          *http.Client
		baseURL, apiKey string
		repo            domain.RecipeRepository
		ensurer         tracking.IngredientEnsurer
		namer           tracking.IngredientNamer
	}{
		{"nil client", nil, "http://x", "k", repo, ensurer, namer},
		{"empty base url", client, "", "k", repo, ensurer, namer},
		{"empty api key", client, "http://x", "", repo, ensurer, namer},
		{"nil repo", client, "http://x", "k", nil, ensurer, namer},
		{"nil ensurer", client, "http://x", "k", repo, nil, namer},
		{"nil namer", client, "http://x", "k", repo, ensurer, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.NewExternalRecipeSource(tc.client, tc.baseURL, tc.apiKey, tc.repo, tc.ensurer, tc.namer); err == nil {
				t.Errorf("NewExternalRecipeSource(%s) = nil error, want error", tc.name)
			}
		})
	}
}
