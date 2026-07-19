package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	"github.com/ericfisherdev/nestova/internal/platform/cache"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// ExternalRequestTimeout bounds a single provider request; the composition root
// builds the injected http.Client with this timeout so a slow provider cannot
// stall the finder.
const ExternalRequestTimeout = 10 * time.Second

// maxResponseBytes caps the provider response we will read, so a misbehaving or
// compromised provider cannot exhaust memory with an unbounded payload.
const maxResponseBytes = 2 << 20 // 2 MiB

// externalFindCacheTTL is how long a find-by-ingredients query's raw
// provider results are cached (NES-140) before an identical subsequent
// query re-queries the provider.
const externalFindCacheTTL = 24 * time.Hour

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
	cache   cache.Cache
	logger  *slog.Logger
	// sf collapses concurrent identical cache-miss queries into one
	// upstream provider call (NES-140 round 2) — the provider is a
	// metered, paid API, exactly what singleflight exists to protect. Its
	// zero value is ready to use; ExternalRecipeSource is only ever used
	// through the pointer NewExternalRecipeSource returns, so embedding it
	// by value here is safe (it is never copied after construction).
	sf singleflight.Group
}

// Compile-time assurance the source satisfies the port.
var _ domain.RecipeSource = (*ExternalRecipeSource)(nil)

// NewExternalRecipeSource constructs the source. client should carry a bounded
// Timeout (see ExternalRequestTimeout); recipes caches results, ensurer normalizes
// returned ingredient names, and namer resolves the on-hand ingredient ids to the
// names the provider query expects. cache holds a cache-aside of raw provider
// results (NES-140), keyed by the resolved ingredient set — see
// externalFindCacheKey; logger records cache-write failures, which are
// swallowed rather than surfaced (a cache write is never load-bearing for
// this source's own correctness).
func NewExternalRecipeSource(client *http.Client, baseURL, apiKey string, recipes domain.RecipeRepository, ensurer tracking.IngredientEnsurer, namer tracking.IngredientNamer, cache cache.Cache, logger *slog.Logger) (*ExternalRecipeSource, error) {
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
	case cache == nil:
		return nil, errors.New("adapter: NewExternalRecipeSource requires a non-nil cache")
	case logger == nil:
		return nil, errors.New("adapter: NewExternalRecipeSource requires a non-nil logger")
	}
	return &ExternalRecipeSource{
		client: client, baseURL: baseURL, apiKey: apiKey,
		recipes: recipes, ensurer: ensurer, namer: namer,
		cache: cache, logger: logger,
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

	results, err := s.findResults(ctx, query)
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

// findResults returns the provider's find-by-ingredients results for query,
// cache-aside around the metered provider call ONLY (NES-140): a cache hit
// within externalFindCacheTTL skips queryProvider entirely; a miss (absent
// or expired — a real cache read error is logged and ALSO treated as a
// miss, since falling through to a fresh provider query is always safe)
// queries the provider and caches the raw results for next time.
// cacheAndMap's own per-result DB upsert is NOT part of this cache-aside —
// FindByIngredients runs it on every call, cache hit or not, so each
// household's local recipe-box copy of an external recipe stays current
// regardless of the query-level cache.
//
// Concurrent identical misses are collapsed into one upstream call via
// s.sf, keyed on the same cache key: the provider is a metered, paid API,
// so N goroutines racing on the same just-expired or never-cached query
// must not turn into N upstream requests.
func (s *ExternalRecipeSource) findResults(ctx context.Context, query []string) ([]providerRecipe, error) {
	key := externalFindCacheKey(query)
	if cached, ok, err := s.cache.Get(ctx, key); err != nil {
		s.logger.Warn("external recipe source: failed to read find-by-ingredients cache",
			"error", err)
	} else if ok {
		var results []providerRecipe
		if jsonErr := json.Unmarshal(cached, &results); jsonErr == nil {
			return results, nil
		}
		// A corrupt or incompatible cached payload is treated as a miss, not
		// an error: falling through to a fresh provider query is always safe.
	}

	v, err, _ := s.sf.Do(key, func() (any, error) {
		results, err := s.queryProvider(ctx, query)
		if err != nil {
			return nil, err
		}
		if payload, err := json.Marshal(results); err == nil {
			if err := s.cache.Set(ctx, key, payload, externalFindCacheTTL); err != nil {
				s.logger.Warn("external recipe source: failed to cache find-by-ingredients results",
					"error", err)
			}
		}
		return results, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]providerRecipe), nil
}

// externalFindCacheKey namespaces a find-by-ingredients cache key
// "recipes:externalfind:<id>" (see cache.Cache's own key-namespace
// convention doc), where id is the SHA-256 hex digest of a SORTED COPY of
// the resolved ingredient names. Sorting means two calls that resolve the
// same ingredient set in a different order (e.g. a different iteration
// order over the household's on-hand ingredients) hit the same cache entry;
// hashing keeps the key a bounded, filesystem/log-safe length regardless of
// how many ingredients or how long their names are.
func externalFindCacheKey(ingredients []string) string {
	sorted := make([]string, len(ingredients))
	copy(sorted, ingredients)
	sort.Strings(sorted)
	// A NUL separator, not a comma: an ingredient name could itself contain
	// a comma, which would let two different ingredient sets hash to the
	// same key.
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return "recipes:externalfind:" + hex.EncodeToString(sum[:])
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
			// Skip only an invalid provider value (e.g. a blank name); a real failure
			// (a catalogue/DB error) must surface rather than silently dropping a
			// missing ingredient and returning understated results.
			if errors.Is(err, tracking.ErrInvalidIngredient) {
				continue
			}
			return domain.RecipeMatch{}, fmt.Errorf("external recipe source: normalize missing ingredient %q: %w", ingredient.Name, err)
		}
		missing = append(missing, normalized.ID)
	}
	return domain.RecipeMatch{Recipe: *stored, MatchPct: pct, Missing: missing}, nil
}
