package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// ExternalRequestTimeout bounds a single provider request; the composition root
// builds the injected http.Client with this timeout so a slow provider cannot
// stall the finder.
const ExternalRequestTimeout = 10 * time.Second

// maxResponseBytes caps the provider response we will read, so a misbehaving or
// compromised provider cannot exhaust memory with an unbounded payload.
const maxResponseBytes = 2 << 20 // 2 MiB

// ExternalRecipeSource is the "discover more" implementation of domain.RecipeSource
// backed by an external provider's find-by-ingredients endpoint (Spoonacular-shaped:
// each result reports the ingredients of the query the recipe uses and the ones it
// still needs). It caches each result as an external recipe (keyed by provider id)
// so repeated lookups are cheap, and normalizes the provider's ingredient names to
// the shared catalogue. The API key and base URL come from configuration, never code.
type ExternalRecipeSource struct {
	client  *http.Client
	baseURL string
	apiKey  string
	recipes domain.RecipeRepository
	ensurer tracking.IngredientEnsurer
	namer   tracking.IngredientNamer
}

// Compile-time assurance the source satisfies the port.
var _ domain.RecipeSource = (*ExternalRecipeSource)(nil)

// NewExternalRecipeSource constructs the source. client should carry a bounded
// Timeout (see ExternalRequestTimeout); recipes caches results, ensurer normalizes
// returned ingredient names, and namer resolves the on-hand ingredient ids to the
// names the provider query expects.
func NewExternalRecipeSource(client *http.Client, baseURL, apiKey string, recipes domain.RecipeRepository, ensurer tracking.IngredientEnsurer, namer tracking.IngredientNamer) (*ExternalRecipeSource, error) {
	switch {
	case client == nil:
		return nil, errors.New("adapter: NewExternalRecipeSource requires a non-nil http client")
	case strings.TrimSpace(baseURL) == "":
		return nil, errors.New("adapter: NewExternalRecipeSource requires a base url")
	case strings.TrimSpace(apiKey) == "":
		return nil, errors.New("adapter: NewExternalRecipeSource requires an api key")
	case recipes == nil:
		return nil, errors.New("adapter: NewExternalRecipeSource requires a non-nil recipe repository")
	case ensurer == nil:
		return nil, errors.New("adapter: NewExternalRecipeSource requires a non-nil ingredient ensurer")
	case namer == nil:
		return nil, errors.New("adapter: NewExternalRecipeSource requires a non-nil ingredient namer")
	}
	return &ExternalRecipeSource{
		client: client, baseURL: baseURL, apiKey: apiKey,
		recipes: recipes, ensurer: ensurer, namer: namer,
	}, nil
}

// providerRecipe is one find-by-ingredients result; only the fields the finder
// needs are decoded.
type providerRecipe struct {
	ID                int                  `json:"id"`
	Title             string               `json:"title"`
	UsedIngredients   []providerIngredient `json:"usedIngredients"`
	MissedIngredients []providerIngredient `json:"missedIngredients"`
}

type providerIngredient struct {
	Name string `json:"name"`
}

// FindByIngredients queries the provider with the on-hand ingredient names, caches
// each returned recipe, and maps results to RecipeMatch (MatchPct = used / (used +
// missed); Missing = the recipe's still-needed ingredients). It returns no matches
// when there are no ingredients to search, and a wrapped error on provider failure
// (the hybrid degrades on that error).
// (householdID is unused: external/cached recipes are household-agnostic.)
func (s *ExternalRecipeSource) FindByIngredients(ctx context.Context, _ household.HouseholdID, have []tracking.IngredientID) ([]domain.RecipeMatch, error) {
	if len(have) == 0 {
		return nil, nil
	}
	names, err := s.namer.NamesByIDs(ctx, have)
	if err != nil {
		return nil, fmt.Errorf("external recipe source: resolve ingredient names: %w", err)
	}
	query := make([]string, 0, len(have))
	for _, id := range have {
		if name, ok := names[id]; ok {
			query = append(query, name)
		}
	}
	if len(query) == 0 {
		return nil, nil
	}

	results, err := s.queryProvider(ctx, query)
	if err != nil {
		return nil, err
	}

	matches := make([]domain.RecipeMatch, 0, len(results))
	for _, result := range results {
		match, err := s.cacheAndMap(ctx, result)
		if err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, nil
}

// queryProvider performs the find-by-ingredients GET and decodes the results.
func (s *ExternalRecipeSource) queryProvider(ctx context.Context, ingredients []string) ([]providerRecipe, error) {
	endpoint, err := url.Parse(s.baseURL)
	if err != nil {
		return nil, fmt.Errorf("external recipe source: parse base url: %w", err)
	}
	endpoint = endpoint.JoinPath("recipes", "findByIngredients")
	q := endpoint.Query()
	q.Set("ingredients", strings.Join(ingredients, ","))
	q.Set("number", "10")
	q.Set("apiKey", s.apiKey)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("external recipe source: build request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("external recipe source: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("external recipe source: provider returned status %d", resp.StatusCode)
	}

	var results []providerRecipe
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&results); err != nil {
		return nil, fmt.Errorf("external recipe source: decode response: %w", err)
	}
	return results, nil
}

// cacheAndMap caches the provider result as an external recipe and maps it to a
// RecipeMatch.
func (s *ExternalRecipeSource) cacheAndMap(ctx context.Context, result providerRecipe) (domain.RecipeMatch, error) {
	ref := "spoonacular-" + strconv.Itoa(result.ID)
	recipe := &domain.Recipe{
		ID:          domain.NewRecipeID(),
		Title:       result.Title,
		Source:      domain.SourceExternal,
		ExternalRef: &ref,
		// find-by-ingredients omits the yield; 1 keeps the row valid and is refined
		// when (if) the full recipe is fetched.
		Servings: 1,
	}
	stored, err := s.recipes.UpsertExternal(ctx, recipe)
	if err != nil {
		return domain.RecipeMatch{}, fmt.Errorf("external recipe source: cache recipe: %w", err)
	}

	used := len(result.UsedIngredients)
	missed := len(result.MissedIngredients)
	pct := 1.0
	if used+missed > 0 {
		pct = float64(used) / float64(used+missed)
	}

	missing := make([]tracking.IngredientID, 0, missed)
	for _, ingredient := range result.MissedIngredients {
		normalized, err := s.ensurer.EnsureIngredient(ctx, ingredient.Name)
		if err != nil {
			// A provider ingredient that does not normalize (e.g. blank) is skipped
			// rather than failing the whole discovery result.
			continue
		}
		missing = append(missing, normalized.ID)
	}
	return domain.RecipeMatch{Recipe: *stored, MatchPct: pct, Missing: missing}, nil
}
