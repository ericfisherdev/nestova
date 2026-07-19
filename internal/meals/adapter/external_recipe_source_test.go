package adapter_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/meals/adapter"
	"github.com/ericfisherdev/nestova/internal/meals/domain"
	"github.com/ericfisherdev/nestova/internal/platform/cache"
	tracking "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// discardLogger is a *slog.Logger that writes nowhere, for tests that must
// supply a non-nil logger but do not assert on its output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// capturingRecipeRepo records UpsertExternal calls and echoes the recipe back (as
// the real adapter does, with the id preserved). mu guards upserted so this
// double is safe under the concurrent-callers test below, matching a real
// pool-backed repository's own concurrency safety — tests that read
// upserted only do so after every writer goroutine has finished (via
// wg.Wait()), so no lock is needed on the read side.
type capturingRecipeRepo struct {
	mu       sync.Mutex
	upserted []*domain.Recipe
}

func (r *capturingRecipeRepo) Create(context.Context, *domain.Recipe) error { return nil }
func (r *capturingRecipeRepo) Update(context.Context, *domain.Recipe) error { return nil }
func (r *capturingRecipeRepo) UpsertExternal(_ context.Context, recipe *domain.Recipe) (*domain.Recipe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// fakeEnsurer's mu guards byName for the same reason as
// capturingRecipeRepo's mu above: safe under concurrent callers.
type fakeEnsurer struct {
	mu     sync.Mutex
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
	f.mu.Lock()
	defer f.mu.Unlock()
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

func newExternalSource(t *testing.T, baseURL string, repo domain.RecipeRepository, ensurer tracking.IngredientEnsurer, namer tracking.IngredientNamer, c cache.Cache) *adapter.ExternalRecipeSource {
	t.Helper()
	src, err := adapter.NewExternalRecipeSource(&http.Client{Timeout: 5 * time.Second}, baseURL, "test-key", repo, ensurer, namer, c, discardLogger())
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
	src := newExternalSource(t, server.URL, repo, ensurer, namer, cache.NewMemoryCache())

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

	src := newExternalSource(t, server.URL, &capturingRecipeRepo{}, newFakeEnsurer(), fakeNamer{}, cache.NewMemoryCache())
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
		fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}}, cache.NewMemoryCache())
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
		fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}}, cache.NewMemoryCache())
	if _, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour}); err == nil {
		t.Error("FindByIngredients with malformed JSON = nil error, want decode error")
	}
}

// erroringEnsurer fails every normalization with a non-validation error.
type erroringEnsurer struct{ err error }

func (e erroringEnsurer) EnsureIngredient(context.Context, string) (*tracking.Ingredient, error) {
	return nil, e.err
}

func TestExternalRecipeSourcePropagatesNormalizationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"title":"R","usedIngredients":[{"name":"flour"}],"missedIngredients":[{"name":"eggs"}]}]`))
	}))
	defer server.Close()

	flour := tracking.NewIngredientID()
	sentinel := errors.New("catalogue unavailable")
	src := newExternalSource(t, server.URL, &capturingRecipeRepo{}, erroringEnsurer{err: sentinel},
		fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}}, cache.NewMemoryCache())
	if _, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour}); !errors.Is(err, sentinel) {
		t.Errorf("FindByIngredients = %v, want wrapped %v (real normalization errors must surface)", err, sentinel)
	}
}

func TestNewExternalRecipeSourceGuards(t *testing.T) {
	client := &http.Client{}
	repo := &capturingRecipeRepo{}
	ensurer := newFakeEnsurer()
	namer := fakeNamer{}
	memCache := cache.NewMemoryCache()
	logger := discardLogger()
	cases := []struct {
		name            string
		client          *http.Client
		baseURL, apiKey string
		repo            domain.RecipeRepository
		ensurer         tracking.IngredientEnsurer
		namer           tracking.IngredientNamer
		cache           cache.Cache
		logger          *slog.Logger
	}{
		{"nil client", nil, "http://x", "k", repo, ensurer, namer, memCache, logger},
		{"empty base url", client, "", "k", repo, ensurer, namer, memCache, logger},
		{"empty api key", client, "http://x", "", repo, ensurer, namer, memCache, logger},
		{"nil repo", client, "http://x", "k", nil, ensurer, namer, memCache, logger},
		{"nil ensurer", client, "http://x", "k", repo, nil, namer, memCache, logger},
		{"nil namer", client, "http://x", "k", repo, ensurer, nil, memCache, logger},
		{"nil cache", client, "http://x", "k", repo, ensurer, namer, nil, logger},
		{"nil logger", client, "http://x", "k", repo, ensurer, namer, memCache, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.NewExternalRecipeSource(tc.client, tc.baseURL, tc.apiKey, tc.repo, tc.ensurer, tc.namer, tc.cache, tc.logger); err == nil {
				t.Errorf("NewExternalRecipeSource(%s) = nil error, want error", tc.name)
			}
		})
	}
}

// countingHTTPClient wraps an http.RoundTripper to count how many requests
// pass through it — the hit-counter used by
// TestExternalRecipeSourceCacheAvoidsRepeatedProviderCalls to assert the
// provider is queried once, not once per identical FindByIngredients call.
type countingRoundTripper struct {
	inner http.RoundTripper
	hits  atomic.Int64
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return c.inner.RoundTrip(req)
}

// TestExternalRecipeSourceCacheAvoidsRepeatedProviderCalls is the NES-140
// regression test for the cache-aside AC: two identical FindByIngredients
// calls against a MemoryCache-backed source must reach the provider exactly
// once, not twice — the second call is served entirely from the cache.
func TestExternalRecipeSourceCacheAvoidsRepeatedProviderCalls(t *testing.T) {
	flour := tracking.NewIngredientID()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":42,"title":"Discovered Pasta",
			"usedIngredients":[{"name":"flour"}],
			"missedIngredients":[{"name":"eggs"}]}]`))
	}))
	defer server.Close()

	counter := &countingRoundTripper{inner: http.DefaultTransport}
	client := &http.Client{Timeout: 5 * time.Second, Transport: counter}
	repo := &capturingRecipeRepo{}
	ensurer := newFakeEnsurer()
	namer := fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}}
	src, err := adapter.NewExternalRecipeSource(client, server.URL, "test-key", repo, ensurer, namer, cache.NewMemoryCache(), discardLogger())
	if err != nil {
		t.Fatalf("NewExternalRecipeSource: %v", err)
	}

	ctx := context.Background()
	hh := household.NewHouseholdID()
	if _, err := src.FindByIngredients(ctx, hh, []tracking.IngredientID{flour}); err != nil {
		t.Fatalf("FindByIngredients (first call): %v", err)
	}
	if _, err := src.FindByIngredients(ctx, hh, []tracking.IngredientID{flour}); err != nil {
		t.Fatalf("FindByIngredients (second call): %v", err)
	}

	if got := counter.hits.Load(); got != 1 {
		t.Errorf("upstream provider hits = %d, want 1 (second call should be served from the cache)", got)
	}
	// cacheAndMap's own per-result DB upsert still runs on every call,
	// cache hit or not (NES-140's explicit requirement) — so it must show
	// two upserts even though the provider was queried only once.
	if len(repo.upserted) != 2 {
		t.Errorf("upserted = %d, want 2 (DB caching runs on every call, cache hit or not)", len(repo.upserted))
	}
}

// TestExternalRecipeSourceConcurrentIdenticalQueriesCollapseViaSingleflight
// is the NES-140 round-2 regression test for stampede protection: N
// concurrent FindByIngredients calls with the same resolved ingredient set
// must reach the metered provider exactly once. The handler blocks on
// release until every goroutine has had a chance to reach (or join) the
// in-flight call, so the assertion proves collapsing, not lucky timing.
func TestExternalRecipeSourceConcurrentIdenticalQueriesCollapseViaSingleflight(t *testing.T) {
	flour := tracking.NewIngredientID()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":42,"title":"Discovered Pasta",
			"usedIngredients":[{"name":"flour"}],
			"missedIngredients":[{"name":"eggs"}]}]`))
	}))
	defer server.Close()

	counter := &countingRoundTripper{inner: http.DefaultTransport}
	client := &http.Client{Timeout: 5 * time.Second, Transport: counter}
	repo := &capturingRecipeRepo{}
	ensurer := newFakeEnsurer()
	namer := fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}}
	src, err := adapter.NewExternalRecipeSource(client, server.URL, "test-key", repo, ensurer, namer, cache.NewMemoryCache(), discardLogger())
	if err != nil {
		t.Fatalf("NewExternalRecipeSource: %v", err)
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour})
			errs[i] = err
		}(i)
	}

	// Give every goroutine time to reach the blocked provider call, or join
	// the in-flight singleflight group, before releasing the response.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: FindByIngredients: %v", i, err)
		}
	}
	if got := counter.hits.Load(); got != 1 {
		t.Errorf("upstream provider hits = %d, want 1 (concurrent identical queries must collapse via singleflight)", got)
	}
}

// erroringGetCache wraps a real MemoryCache but always fails Get with err,
// so Set/Delete still behave normally — isolating a test to Get's own
// error-handling branch in findResults.
type erroringGetCache struct {
	*cache.MemoryCache
	err error
}

func (c *erroringGetCache) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, c.err
}

// TestExternalRecipeSourceGetCacheErrorIsLoggedAndTreatedAsMiss is the
// NES-140 round-2 regression test for finding #4: a real cache Get error
// (not just a miss) must be logged AND still fall through to the provider,
// so FindByIngredients still succeeds.
func TestExternalRecipeSourceGetCacheErrorIsLoggedAndTreatedAsMiss(t *testing.T) {
	flour := tracking.NewIngredientID()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"title":"R","usedIngredients":[{"name":"flour"}],"missedIngredients":[{"name":"eggs"}]}]`))
	}))
	defer server.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	sentinel := errors.New("disk read failure")
	erroringCache := &erroringGetCache{MemoryCache: cache.NewMemoryCache(), err: sentinel}

	src, err := adapter.NewExternalRecipeSource(&http.Client{Timeout: 5 * time.Second}, server.URL, "test-key",
		&capturingRecipeRepo{}, newFakeEnsurer(), fakeNamer{names: map[tracking.IngredientID]string{flour: "flour"}},
		erroringCache, logger)
	if err != nil {
		t.Fatalf("NewExternalRecipeSource: %v", err)
	}

	matches, err := src.FindByIngredients(context.Background(), household.NewHouseholdID(), []tracking.IngredientID{flour})
	if err != nil {
		t.Fatalf("FindByIngredients: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d, want 1 (a cache Get error must fall through to the provider, not fail the call)", len(matches))
	}
	if !strings.Contains(logBuf.String(), "disk read failure") {
		t.Errorf("log output = %q, want it to mention the cache Get error", logBuf.String())
	}
}
