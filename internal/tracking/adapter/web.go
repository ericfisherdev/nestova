package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/internal/tracking/app"
	"github.com/ericfisherdev/nestova/internal/tracking/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// groceriesPath is the canonical path the page renders at and every mutation
// redirects back to after success.
const groceriesPath = "/groceries"

// expiringSoonDays is how many days ahead of today a pantry item is flagged as
// expiring soon (and highlighted). Matches the NES-45 brief's 7-day window.
const expiringSoonDays = 7

// dateLayout is the YYYY-MM-DD layout used for the optional pantry expiry input.
const dateLayout = "2006-01-02"

// displayDateLayout is the human-readable date layout shown in the UI for
// predicted depletion and expiry dates.
const displayDateLayout = "Jan 2, 2006"

// LayoutFunc is the callback home.go passes to Page so the handler can wrap its
// content in the full A·Hearth app shell. It mirrors the tasks adapter's type:
// build ShellProps + nav, return a templ layout func.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// WebHandlers holds the HTTP handler methods for the /groceries UI: the usage
// tracker, pantry, and shopping-list sections plus their HTMX mutation actions.
// All dependencies are injected via the constructor so the type is testable with
// fakes, matching the tasks WebHandlers pattern.
type WebHandlers struct {
	usage       *app.UsageService
	pantry      *app.PantryService
	shopping    *app.ShoppingListService
	items       domain.TrackedItemRepository
	predictions domain.RestockPredictionRepository
	ingredients domain.IngredientEnsurer
	namer       domain.IngredientNamer
	households  household.HouseholdRepository
	sm          *scs.SessionManager
	logger      *slog.Logger
}

// NewWebHandlers constructs a WebHandlers with all required dependencies. It
// panics when any dependency is nil so misconfigured composition roots are
// caught at startup rather than at the first HTTP request.
func NewWebHandlers(
	usage *app.UsageService,
	pantry *app.PantryService,
	shopping *app.ShoppingListService,
	items domain.TrackedItemRepository,
	predictions domain.RestockPredictionRepository,
	ingredients domain.IngredientEnsurer,
	namer domain.IngredientNamer,
	households household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *WebHandlers {
	if usage == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil UsageService")
	}
	if pantry == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil PantryService")
	}
	if shopping == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil ShoppingListService")
	}
	if items == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil TrackedItemRepository")
	}
	if predictions == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil RestockPredictionRepository")
	}
	if ingredients == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil IngredientEnsurer")
	}
	if namer == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil IngredientNamer")
	}
	if households == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil HouseholdRepository")
	}
	if sm == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("tracking/adapter: NewWebHandlers requires a non-nil logger")
	}
	return &WebHandlers{
		usage:       usage,
		pantry:      pantry,
		shopping:    shopping,
		items:       items,
		predictions: predictions,
		ingredients: ingredients,
		namer:       namer,
		households:  households,
		sm:          sm,
		logger:      logger,
	}
}

// Page handles GET /groceries. It loads the three sections — active tracked
// items with their cached restock predictions, the on-hand pantry with
// expiring-soon flags, and the shopping list grouped by status — builds the view
// model, and renders GroceriesPage into the app shell.
//
// The layout callback is supplied by the caller (home.go) so this handler stays
// decoupled from the ShellProps / nav construction that depends on the request
// and household repository.
func (h *WebHandlers) Page(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		view, err := h.buildView(r, member)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "groceries: build view", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		content := components.GroceriesPage(view)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "groceries: render page", "error", err)
		}
	}
}

// RegisterItem handles POST /groceries/items. It registers a new tracked item
// from the submitted name, category, and restock lead days, then refreshes the
// page via respond-after-mutation.
func (h *WebHandlers) RegisterItem(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}

	name := r.FormValue("name")
	category := r.FormValue("category")
	leadDays := parseLeadDays(r.FormValue("lead_days"))

	if _, err := h.usage.RegisterItem(r.Context(), member.HouseholdID, name, category, leadDays); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}

// LogUsage handles POST /groceries/items/{id}/usage. It logs the submitted usage
// type against the tracked item (recomputing the restock prediction on a
// depletion), then refreshes the page.
func (h *WebHandlers) LogUsage(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}

	itemID, err := domain.ParseTrackedItemID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid tracked item id", http.StatusBadRequest)
		return
	}
	usageType, err := domain.ParseUsageType(r.FormValue("type"))
	if err != nil {
		http.Error(w, "invalid usage type", http.StatusBadRequest)
		return
	}

	if _, err := h.usage.LogEvent(r.Context(), member.HouseholdID, itemID, usageType, &member.ID, time.Now()); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}

// PantryAdd handles POST /groceries/pantry. It resolves the submitted ingredient
// name to a canonical ingredient, then adds an on-hand pantry entry with the
// submitted quantity and optional expiry.
func (h *WebHandlers) PantryAdd(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}

	quantity, err := parseQuantity(r.FormValue("amount"), r.FormValue("unit"))
	if err != nil {
		http.Error(w, "invalid quantity", http.StatusBadRequest)
		return
	}
	expiresOn, err := parseOptionalDate(r.FormValue("expires_on"))
	if err != nil {
		http.Error(w, "invalid expiry date", http.StatusBadRequest)
		return
	}

	ingredient, err := h.ingredients.EnsureIngredient(r.Context(), r.FormValue("name"))
	if err != nil {
		h.handleMutationError(w, r, err)
		return
	}

	if _, err := h.pantry.Add(r.Context(), member.HouseholdID, ingredient.ID, quantity, expiresOn); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}

// pantryQuantityOp is the shared shape of PantryService.Consume and Adjust: it
// applies an amount+unit quantity change to a pantry item, scoped to the
// household.
type pantryQuantityOp func(ctx context.Context, householdID household.HouseholdID, itemID domain.PantryItemID, amount household.Quantity) (*domain.PantryItem, error)

// PantryConsume handles POST /groceries/pantry/{id}/consume. It decreases the
// item's on-hand quantity by the submitted amount.
func (h *WebHandlers) PantryConsume(w http.ResponseWriter, r *http.Request) {
	h.pantryQuantityMutation(w, r, h.pantry.Consume)
}

// PantryAdjust handles POST /groceries/pantry/{id}/adjust. It increases the
// item's on-hand quantity by the submitted amount.
func (h *WebHandlers) PantryAdjust(w http.ResponseWriter, r *http.Request) {
	h.pantryQuantityMutation(w, r, h.pantry.Adjust)
}

// pantryQuantityMutation is the shared body of Consume and Adjust: both parse the
// pantry item id and an amount+unit quantity, then invoke the supplied operation.
func (h *WebHandlers) pantryQuantityMutation(w http.ResponseWriter, r *http.Request, op pantryQuantityOp) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}

	itemID, err := domain.ParsePantryItemID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid pantry item id", http.StatusBadRequest)
		return
	}
	quantity, err := parseQuantity(r.FormValue("amount"), r.FormValue("unit"))
	if err != nil {
		http.Error(w, "invalid quantity", http.StatusBadRequest)
		return
	}

	if _, err := op(r.Context(), member.HouseholdID, itemID, quantity); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}

// ShoppingAdd handles POST /groceries/shopping. It adds an ad-hoc free-text
// manual item in the needed state, attributed to the current member.
func (h *WebHandlers) ShoppingAdd(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}

	quantity, err := parseQuantity(r.FormValue("amount"), r.FormValue("unit"))
	if err != nil {
		http.Error(w, "invalid quantity", http.StatusBadRequest)
		return
	}

	if _, err := h.shopping.AddManualItem(r.Context(), member.HouseholdID, nil, r.FormValue("name"), quantity, &member.ID); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}

// ShoppingTransition handles POST /groceries/shopping/{id}/status. It transitions
// a shopping item to the submitted lifecycle status.
func (h *WebHandlers) ShoppingTransition(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}

	itemID, err := domain.ParseShoppingListItemID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid shopping list item id", http.StatusBadRequest)
		return
	}
	status, err := domain.ParseItemStatus(r.FormValue("status"))
	if err != nil {
		http.Error(w, "invalid item status", http.StatusBadRequest)
		return
	}

	if _, err := h.shopping.TransitionStatus(r.Context(), member.HouseholdID, itemID, status); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, groceriesPath)
}

// beginMutation performs the shared mutation preamble: parse the form, verify the
// CSRF token, and resolve the current member. It writes the appropriate error
// response and returns ok=false when any step fails, so the caller returns
// immediately.
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

// buildView assembles the GroceriesView for the page. It makes a bounded number
// of queries: one tracked-items list (plus one prediction Get per item), one
// pantry list, one expiring-within list, three shopping-by-status lists, and one
// batch ingredient-name lookup — no per-row N+1 name resolution.
func (h *WebHandlers) buildView(r *http.Request, member *household.Member) (components.GroceriesView, error) {
	ctx := r.Context()
	hh := member.HouseholdID

	trackedItems, err := h.buildTrackedItemViews(r, member)
	if err != nil {
		return components.GroceriesView{}, err
	}

	pantryItems, err := h.pantry.List(ctx, hh)
	if err != nil {
		return components.GroceriesView{}, err
	}
	expiring, err := h.pantry.ListExpiringWithin(ctx, hh, time.Now(), expiringSoonDays)
	if err != nil {
		return components.GroceriesView{}, err
	}

	needed, err := h.shopping.ListByStatus(ctx, hh, domain.StatusNeeded)
	if err != nil {
		return components.GroceriesView{}, err
	}
	inCart, err := h.shopping.ListByStatus(ctx, hh, domain.StatusInCart)
	if err != nil {
		return components.GroceriesView{}, err
	}
	purchased, err := h.shopping.ListByStatus(ctx, hh, domain.StatusPurchased)
	if err != nil {
		return components.GroceriesView{}, err
	}

	names, err := h.resolveIngredientNames(ctx, pantryItems, needed, inCart, purchased)
	if err != nil {
		return components.GroceriesView{}, err
	}

	expiringIDs := make(map[domain.PantryItemID]bool, len(expiring))
	for _, item := range expiring {
		expiringIDs[item.ID] = true
	}

	return components.GroceriesView{
		TrackedItems: trackedItems,
		Pantry:       toPantryViews(pantryItems, expiringIDs, names),
		Needed:       toShoppingViews(needed, names),
		InCart:       toShoppingViews(inCart, names),
		Purchased:    toShoppingViews(purchased, names),
		Units:        unitOptions(),
		CSRFToken:    authadapter.GetCSRFToken(ctx, h.sm),
	}, nil
}

// buildTrackedItemViews lists the household's active tracked items and joins each
// with its cached restock prediction (tolerating ErrPredictionNotFound for items
// that have too few depletions to predict).
func (h *WebHandlers) buildTrackedItemViews(r *http.Request, member *household.Member) ([]components.TrackedItemView, error) {
	ctx := r.Context()
	items, err := h.usage.ListItems(ctx, member.HouseholdID)
	if err != nil {
		return nil, err
	}
	views := make([]components.TrackedItemView, 0, len(items))
	for _, item := range items {
		view := components.TrackedItemView{
			ID:       item.ID.String(),
			Name:     item.Name,
			Category: item.Category,
		}
		prediction, predErr := h.predictions.Get(ctx, item.ID)
		switch {
		case predErr == nil:
			view.HasPrediction = true
			view.PredictedDepletionLabel = prediction.PredictedDepletionOn.Format(displayDateLayout)
			view.ConfidenceLabel = formatConfidence(prediction.Confidence)
		case errors.Is(predErr, domain.ErrPredictionNotFound):
			// No prediction yet — leave HasPrediction false.
		default:
			return nil, predErr
		}
		views = append(views, view)
	}
	return views, nil
}

// resolveIngredientNames batch-resolves the canonical names for every distinct
// ingredient id referenced by the pantry and shopping lists, so the row builders
// can label ingredient-keyed entries without an N+1 lookup.
func (h *WebHandlers) resolveIngredientNames(
	ctx context.Context,
	pantryItems []*domain.PantryItem,
	shoppingGroups ...[]*domain.ShoppingListItem,
) (map[domain.IngredientID]string, error) {
	idSet := make(map[domain.IngredientID]struct{})
	for _, item := range pantryItems {
		idSet[item.IngredientID] = struct{}{}
	}
	for _, group := range shoppingGroups {
		for _, item := range group {
			if item.IngredientID != nil {
				idSet[*item.IngredientID] = struct{}{}
			}
		}
	}
	ids := make([]domain.IngredientID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	return h.namer.NamesByIDs(ctx, ids)
}

// handleMutationError maps domain errors to HTTP status codes and writes a
// plain-text error response. The caller must return immediately after this.
func (h *WebHandlers) handleMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrTrackedItemNotFound),
		errors.Is(err, domain.ErrPantryItemNotFound),
		errors.Is(err, domain.ErrShoppingListItemNotFound),
		errors.Is(err, domain.ErrIngredientNotFound),
		errors.Is(err, household.ErrHouseholdNotFound),
		errors.Is(err, household.ErrMemberNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, household.ErrInvalidQuantity),
		errors.Is(err, household.ErrUnitMismatch),
		errors.Is(err, domain.ErrInvalidShoppingListItem),
		errors.Is(err, domain.ErrInvalidIngredient):
		http.Error(w, "invalid request", http.StatusBadRequest)
	default:
		h.logger.ErrorContext(r.Context(), "groceries: mutation error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// respondAfterMutation responds after a successful mutation. HTMX requests
// receive an HX-Redirect so the whole page refreshes and reflects the new state;
// full navigations receive a 303 redirect to target.
func respondAfterMutation(w http.ResponseWriter, r *http.Request, target string) {
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
