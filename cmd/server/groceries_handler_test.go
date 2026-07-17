package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
	trackingapp "github.com/ericfisherdev/nestova/internal/tracking/app"
	trackingdomain "github.com/ericfisherdev/nestova/internal/tracking/domain"
)

// ---------------------------------------------------------------------------
// Tracking domain fakes (hermetic, no DB)
// ---------------------------------------------------------------------------

// fakeTrackedItemRepo is an in-memory TrackedItemRepository recording appends.
type fakeTrackedItemRepo struct {
	created []*trackingdomain.TrackedItem
	active  []*trackingdomain.TrackedItem
}

func (f *fakeTrackedItemRepo) Create(_ context.Context, item *trackingdomain.TrackedItem) error {
	f.created = append(f.created, item)
	f.active = append(f.active, item)
	return nil
}

func (f *fakeTrackedItemRepo) Get(_ context.Context, _ trackingdomain.TrackedItemID) (*trackingdomain.TrackedItem, error) {
	return nil, trackingdomain.ErrTrackedItemNotFound
}

func (f *fakeTrackedItemRepo) Update(_ context.Context, _ *trackingdomain.TrackedItem) error {
	return nil
}

func (f *fakeTrackedItemRepo) ListActiveByHousehold(_ context.Context, _ household.HouseholdID) ([]*trackingdomain.TrackedItem, error) {
	return f.active, nil
}

func (f *fakeTrackedItemRepo) ListAllActive(_ context.Context) ([]*trackingdomain.TrackedItem, error) {
	return f.active, nil
}

func (f *fakeTrackedItemRepo) ListDueForRestock(_ context.Context, _ household.HouseholdID, _ time.Time) ([]*trackingdomain.TrackedItem, error) {
	return nil, nil
}

var _ trackingdomain.TrackedItemRepository = (*fakeTrackedItemRepo)(nil)

// fakeUsageEventRepo is an in-memory UsageEventRepository recording appends and
// serving a fixed depletion history so a depletion log drives a recompute.
type fakeUsageEventRepo struct {
	appended   []*trackingdomain.UsageEvent
	depletions []*trackingdomain.UsageEvent
}

func (f *fakeUsageEventRepo) Append(_ context.Context, event *trackingdomain.UsageEvent) error {
	f.appended = append(f.appended, event)
	return nil
}

func (f *fakeUsageEventRepo) ListDepletionEvents(_ context.Context, _ trackingdomain.TrackedItemID) ([]*trackingdomain.UsageEvent, error) {
	return f.depletions, nil
}

var _ trackingdomain.UsageEventRepository = (*fakeUsageEventRepo)(nil)

// fakePredictionRepo is an in-memory RestockPredictionRepository.
type fakePredictionRepo struct {
	upserts int
	preds   map[trackingdomain.TrackedItemID]*trackingdomain.RestockPrediction
}

func newFakePredictionRepo() *fakePredictionRepo {
	return &fakePredictionRepo{preds: map[trackingdomain.TrackedItemID]*trackingdomain.RestockPrediction{}}
}

func (f *fakePredictionRepo) Upsert(_ context.Context, p *trackingdomain.RestockPrediction) error {
	f.upserts++
	f.preds[p.TrackedItemID] = p
	return nil
}

func (f *fakePredictionRepo) Get(_ context.Context, id trackingdomain.TrackedItemID) (*trackingdomain.RestockPrediction, error) {
	p, ok := f.preds[id]
	if !ok {
		return nil, trackingdomain.ErrPredictionNotFound
	}
	return p, nil
}

var _ trackingdomain.RestockPredictionRepository = (*fakePredictionRepo)(nil)

// fakeGroceryPantryRepo is an in-memory PantryRepository.
type fakeGroceryPantryRepo struct {
	items map[trackingdomain.PantryItemID]*trackingdomain.PantryItem
}

func newFakeGroceryPantryRepo() *fakeGroceryPantryRepo {
	return &fakeGroceryPantryRepo{items: map[trackingdomain.PantryItemID]*trackingdomain.PantryItem{}}
}

func (f *fakeGroceryPantryRepo) Create(_ context.Context, item *trackingdomain.PantryItem) error {
	f.items[item.ID] = item
	return nil
}

func (f *fakeGroceryPantryRepo) Get(_ context.Context, id trackingdomain.PantryItemID) (*trackingdomain.PantryItem, error) {
	item, ok := f.items[id]
	if !ok {
		return nil, trackingdomain.ErrPantryItemNotFound
	}
	return item, nil
}

func (f *fakeGroceryPantryRepo) Adjust(_ context.Context, householdID household.HouseholdID, id trackingdomain.PantryItemID, delta household.Quantity) (*trackingdomain.PantryItem, error) {
	item, ok := f.items[id]
	if !ok || item.HouseholdID != householdID {
		return nil, trackingdomain.ErrPantryItemNotFound
	}
	updated, err := item.Quantity.Add(delta)
	if err != nil {
		return nil, err
	}
	item.Quantity = updated
	return item, nil
}

func (f *fakeGroceryPantryRepo) Consume(_ context.Context, householdID household.HouseholdID, id trackingdomain.PantryItemID, amount household.Quantity) (*trackingdomain.PantryItem, error) {
	item, ok := f.items[id]
	if !ok || item.HouseholdID != householdID {
		return nil, trackingdomain.ErrPantryItemNotFound
	}
	updated, err := item.Quantity.Subtract(amount)
	if err != nil {
		return nil, err
	}
	item.Quantity = updated
	return item, nil
}

func (f *fakeGroceryPantryRepo) ListByHousehold(_ context.Context, _ household.HouseholdID) ([]*trackingdomain.PantryItem, error) {
	out := make([]*trackingdomain.PantryItem, 0, len(f.items))
	for _, item := range f.items {
		out = append(out, item)
	}
	return out, nil
}

func (f *fakeGroceryPantryRepo) ListExpiringWithin(_ context.Context, _ household.HouseholdID, _ time.Time, _ int) ([]*trackingdomain.PantryItem, error) {
	return nil, nil
}

var _ trackingdomain.PantryRepository = (*fakeGroceryPantryRepo)(nil)

// fakeShoppingRepo is an in-memory ShoppingListRepository.
type fakeShoppingRepo struct {
	items []*trackingdomain.ShoppingListItem
}

func (f *fakeShoppingRepo) Add(_ context.Context, item *trackingdomain.ShoppingListItem) error {
	f.items = append(f.items, item)
	return nil
}

func (f *fakeShoppingRepo) AddRestockIfAbsent(_ context.Context, _ *trackingdomain.ShoppingListItem) (bool, error) {
	return true, nil
}

func (f *fakeShoppingRepo) AddMealPlanIfAbsent(_ context.Context, _ *trackingdomain.ShoppingListItem) (bool, error) {
	return true, nil
}

func (f *fakeShoppingRepo) UpdateStatus(_ context.Context, householdID household.HouseholdID, id trackingdomain.ShoppingListItemID, status trackingdomain.ItemStatus) (*trackingdomain.ShoppingListItem, error) {
	for _, item := range f.items {
		if item.ID == id && item.HouseholdID == householdID {
			item.Status = status
			return item, nil
		}
	}
	return nil, trackingdomain.ErrShoppingListItemNotFound
}

func (f *fakeShoppingRepo) MarkInCart(_ context.Context, householdID household.HouseholdID, id trackingdomain.ShoppingListItemID) (*trackingdomain.ShoppingListItem, error) {
	for _, item := range f.items {
		if item.ID == id && item.HouseholdID == householdID {
			switch item.Status {
			case trackingdomain.StatusNeeded, trackingdomain.StatusInCart:
				item.Status = trackingdomain.StatusInCart
				return item, nil
			default:
				return nil, trackingdomain.ErrShoppingListItemNotInCartable
			}
		}
	}
	return nil, trackingdomain.ErrShoppingListItemNotFound
}

func (f *fakeShoppingRepo) ListByStatus(_ context.Context, householdID household.HouseholdID, status trackingdomain.ItemStatus) ([]*trackingdomain.ShoppingListItem, error) {
	out := make([]*trackingdomain.ShoppingListItem, 0)
	for _, item := range f.items {
		if item.HouseholdID == householdID && item.Status == status {
			out = append(out, item)
		}
	}
	return out, nil
}

var _ trackingdomain.ShoppingListRepository = (*fakeShoppingRepo)(nil)

// fakeIngredientCatalog is an in-memory IngredientEnsurer + IngredientNamer.
type fakeIngredientCatalog struct {
	byName map[string]trackingdomain.IngredientID
	byID   map[trackingdomain.IngredientID]string
}

func newFakeIngredientCatalog() *fakeIngredientCatalog {
	return &fakeIngredientCatalog{
		byName: map[string]trackingdomain.IngredientID{},
		byID:   map[trackingdomain.IngredientID]string{},
	}
}

func (f *fakeIngredientCatalog) EnsureIngredient(_ context.Context, name string) (*trackingdomain.Ingredient, error) {
	canonical := trackingdomain.NormalizeName(name)
	if canonical == "" {
		return nil, trackingdomain.ErrInvalidIngredient
	}
	id, ok := f.byName[canonical]
	if !ok {
		id = trackingdomain.NewIngredientID()
		f.byName[canonical] = id
		f.byID[id] = canonical
	}
	return &trackingdomain.Ingredient{ID: id, CanonicalName: canonical}, nil
}

func (f *fakeIngredientCatalog) NamesByIDs(_ context.Context, ids []trackingdomain.IngredientID) (map[trackingdomain.IngredientID]string, error) {
	out := make(map[trackingdomain.IngredientID]string, len(ids))
	for _, id := range ids {
		if name, ok := f.byID[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

var (
	_ trackingdomain.IngredientEnsurer = (*fakeIngredientCatalog)(nil)
	_ trackingdomain.IngredientNamer   = (*fakeIngredientCatalog)(nil)
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// groceryFakes bundles the configurable repositories so a test can seed data and
// assert side effects after issuing a request.
type groceryFakes struct {
	tracked     *fakeTrackedItemRepo
	events      *fakeUsageEventRepo
	predictions *fakePredictionRepo
	pantry      *fakeGroceryPantryRepo
	shopping    *fakeShoppingRepo
	ingredients *fakeIngredientCatalog
}

func newGroceryFakes() *groceryFakes {
	return &groceryFakes{
		tracked:     &fakeTrackedItemRepo{},
		events:      &fakeUsageEventRepo{},
		predictions: newFakePredictionRepo(),
		pantry:      newFakeGroceryPantryRepo(),
		shopping:    &fakeShoppingRepo{},
		ingredients: newFakeIngredientCatalog(),
	}
}

// buildGroceryHandlers constructs a trackingadapter.WebHandlers from the supplied
// fakes. Used by both the dedicated grocery tests and the shared route builders.
func buildGroceryHandlers(
	fakes *groceryFakes,
	householdRepo household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *trackingadapter.WebHandlers {
	predictor, err := trackingapp.NewPredictor(fakes.events, fakes.predictions)
	if err != nil {
		panic("buildGroceryHandlers: " + err.Error())
	}
	usageSvc, err := trackingapp.NewUsageService(fakes.tracked, fakes.events, predictor, logger)
	if err != nil {
		panic("buildGroceryHandlers: " + err.Error())
	}
	pantrySvc, err := trackingapp.NewPantryService(fakes.pantry)
	if err != nil {
		panic("buildGroceryHandlers: " + err.Error())
	}
	shoppingSvc, err := trackingapp.NewShoppingListService(fakes.shopping)
	if err != nil {
		panic("buildGroceryHandlers: " + err.Error())
	}
	return trackingadapter.NewWebHandlers(
		usageSvc, pantrySvc, shoppingSvc,
		fakes.tracked, fakes.predictions, fakes.ingredients, fakes.ingredients,
		householdRepo, sm, logger,
	)
}

// newTestGroceryHandlers builds grocery WebHandlers wired with fresh no-op fakes,
// used by the other route builders that need registerWebRoutes to compile but do
// not exercise /groceries routes.
func newTestGroceryHandlers(
	householdRepo household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *trackingadapter.WebHandlers {
	return buildGroceryHandlers(newGroceryFakes(), householdRepo, sm, logger)
}

// buildGroceriesTestHandler wires a full http.Handler exercising the /groceries
// routes under an authenticated session backed by the supplied fakes.
func buildGroceriesTestHandler(
	fakes *groceryFakes,
	member *household.Member,
) (http.Handler, *scs.SessionManager) {
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
	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo, nil)
	if err != nil {
		panic("buildGroceriesTestHandler: " + err.Error())
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(
		taskService, recurringRepo, instanceRepo, householdRepo, sm, logger, nil,
	)
	gamificationHandlers := newTestGamificationHandlers(instanceRepo, householdRepo, sm, logger)
	groceryHandlers := buildGroceryHandlers(fakes, householdRepo, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers, newTestTradeHandlers(taskWebHandlers, instanceRepo, householdRepo, sm, logger), gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger))

	return sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	), sm
}

// ---------------------------------------------------------------------------
// Tests: GET /groceries — auth guard
// ---------------------------------------------------------------------------

func TestGroceriesPageRequiresAuth(t *testing.T) {
	handler, _ := buildGroceriesTestHandler(newGroceryFakes(), nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/groceries", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /groceries: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want /login...", loc)
	}
}

func TestGroceriesPageHTMXRequiresAuth(t *testing.T) {
	handler, _ := buildGroceriesTestHandler(newGroceryFakes(), nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/groceries", nil)
	req.Header.Set("HX-Request", "true")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HX GET /groceries: status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /groceries — authenticated renders page with predictions
// ---------------------------------------------------------------------------

func TestGroceriesPageRendersForAuthedMember(t *testing.T) {
	member := testMember()
	fakes := newGroceryFakes()

	// Seed a tracked item with a cached restock prediction so the page shows it.
	itemID := trackingdomain.NewTrackedItemID()
	fakes.tracked.active = []*trackingdomain.TrackedItem{
		{ID: itemID, HouseholdID: member.HouseholdID, Name: "Coffee beans", Category: "pantry", Active: true},
	}
	fakes.predictions.preds[itemID] = &trackingdomain.RestockPrediction{
		TrackedItemID:        itemID,
		PredictedDepletionOn: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Confidence:           0.4,
	}
	// Seed a needed manual shopping item.
	fakes.shopping.items = []*trackingdomain.ShoppingListItem{
		{
			ID:          trackingdomain.NewShoppingListItemID(),
			HouseholdID: member.HouseholdID,
			Name:        "Paper towels",
			Quantity:    household.Quantity{Amount: 1, Unit: household.UnitCount},
			Source:      trackingdomain.SourceManual,
			Status:      trackingdomain.StatusNeeded,
		},
	}

	handler, sm := buildGroceriesTestHandler(fakes, member)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/groceries", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /groceries: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Coffee beans",   // tracked item name
		"Jul 1, 2026",    // predicted depletion date
		"40% confidence", // confidence label
		"Paper towels",   // shopping item name
		"Needed",         // status group heading
		"Manual",         // source badge
	} {
		if !strings.Contains(body, want) {
			t.Errorf("groceries page missing %q: %q", want, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: CSRF guard — POST without a valid token is rejected
// ---------------------------------------------------------------------------

func TestGroceriesRegisterItemRejectsBadCSRF(t *testing.T) {
	member := testMember()
	fakes := newGroceryFakes()
	handler, sm := buildGroceriesTestHandler(fakes, member)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(
		http.MethodPost,
		"/groceries/items",
		strings.NewReader("csrf_token=wrong-token&name=Coffee"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /groceries/items with bad CSRF: status = %d, want 403", rec.Code)
	}
	if len(fakes.tracked.created) != 0 {
		t.Errorf("bad-CSRF register must not persist, got %d items", len(fakes.tracked.created))
	}
}

func TestGroceriesLogUsageRejectsBadCSRF(t *testing.T) {
	member := testMember()
	fakes := newGroceryFakes()
	handler, sm := buildGroceriesTestHandler(fakes, member)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	itemID := trackingdomain.NewTrackedItemID().String()
	req := httptest.NewRequest(
		http.MethodPost,
		"/groceries/items/"+itemID+"/usage",
		strings.NewReader("csrf_token=wrong-token&type=depleted"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST usage with bad CSRF: status = %d, want 403", rec.Code)
	}
	if len(fakes.events.appended) != 0 {
		t.Errorf("bad-CSRF usage must not append, got %d events", len(fakes.events.appended))
	}
}

// ---------------------------------------------------------------------------
// Tests: LogUsage success — HX-Redirect + persisted depletion + recompute
// ---------------------------------------------------------------------------

func TestGroceriesLogUsageDepletedSucceedsAndRecomputes(t *testing.T) {
	member := testMember()
	fakes := newGroceryFakes()
	itemID := trackingdomain.NewTrackedItemID()
	fakes.tracked.active = []*trackingdomain.TrackedItem{
		{ID: itemID, HouseholdID: member.HouseholdID, Name: "Coffee beans", Active: true},
	}
	// Two prior depletions so the new one drives a prediction Upsert.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fakes.events.depletions = []*trackingdomain.UsageEvent{
		{ID: trackingdomain.NewUsageEventID(), Type: trackingdomain.UsageDepleted, OccurredAt: base},
		{ID: trackingdomain.NewUsageEventID(), Type: trackingdomain.UsageDepleted, OccurredAt: base.AddDate(0, 0, 10)},
	}

	handler, sm := buildGroceriesTestHandler(fakes, member)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(
		http.MethodPost,
		"/groceries/items/"+itemID.String()+"/usage",
		strings.NewReader("csrf_token="+csrfToken+"&type=depleted"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HTMX LogUsage: status = %d, want 200", rec.Code)
	}
	if loc := rec.Header().Get("HX-Redirect"); loc != "/groceries" {
		t.Errorf("HX-Redirect = %q, want /groceries", loc)
	}
	if len(fakes.events.appended) != 1 {
		t.Errorf("expected 1 appended event, got %d", len(fakes.events.appended))
	}
	if fakes.predictions.upserts != 1 {
		t.Errorf("depletion log should recompute the prediction (1 upsert), got %d", fakes.predictions.upserts)
	}
}

// ---------------------------------------------------------------------------
// Tests: ShoppingTransition success — HX-Redirect + persisted status
// ---------------------------------------------------------------------------

func TestGroceriesShoppingTransitionSucceeds(t *testing.T) {
	member := testMember()
	fakes := newGroceryFakes()
	itemID := trackingdomain.NewShoppingListItemID()
	fakes.shopping.items = []*trackingdomain.ShoppingListItem{
		{
			ID:          itemID,
			HouseholdID: member.HouseholdID,
			Name:        "Paper towels",
			Quantity:    household.Quantity{Amount: 1, Unit: household.UnitCount},
			Source:      trackingdomain.SourceManual,
			Status:      trackingdomain.StatusNeeded,
		},
	}

	handler, sm := buildGroceriesTestHandler(fakes, member)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(
		http.MethodPost,
		"/groceries/shopping/"+itemID.String()+"/status",
		strings.NewReader("csrf_token="+csrfToken+"&status=in_cart"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HTMX ShoppingTransition: status = %d, want 200", rec.Code)
	}
	if loc := rec.Header().Get("HX-Redirect"); loc != "/groceries" {
		t.Errorf("HX-Redirect = %q, want /groceries", loc)
	}
	if got := fakes.shopping.items[0].Status; got != trackingdomain.StatusInCart {
		t.Errorf("item status = %q, want in_cart (side effect persisted)", got)
	}
}

// ---------------------------------------------------------------------------
// Tests: nav — Groceries item exists and is active on /groceries
// ---------------------------------------------------------------------------

func TestGroceriesNavItemExists(t *testing.T) {
	nav := primaryNav("")
	var found bool
	for _, item := range nav {
		if item.Href == groceriesNavHref {
			found = true
			if item.Label != "Groceries" {
				t.Errorf("Groceries nav item label = %q, want Groceries", item.Label)
			}
		}
	}
	if !found {
		t.Errorf("primary nav has no item with href %q", groceriesNavHref)
	}
}

func TestGroceriesNavActiveWhenOnGroceries(t *testing.T) {
	nav := primaryNav(groceriesNavHref)
	var activeCount int
	for _, item := range nav {
		if item.Active {
			activeCount++
			if item.Href != groceriesNavHref {
				t.Errorf("unexpected active item: %q", item.Href)
			}
		}
	}
	if activeCount != 1 {
		t.Errorf("active nav items = %d, want 1", activeCount)
	}
}
