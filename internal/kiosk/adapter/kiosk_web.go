package adapter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	calendarapp "github.com/ericfisherdev/nestova/internal/calendar/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	kioskdomain "github.com/ericfisherdev/nestova/internal/kiosk/domain"
	mealsapp "github.com/ericfisherdev/nestova/internal/meals/app"
	mealsdomain "github.com/ericfisherdev/nestova/internal/meals/domain"
	mediaapp "github.com/ericfisherdev/nestova/internal/media/app"
	mediadomain "github.com/ericfisherdev/nestova/internal/media/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
	trackingapp "github.com/ericfisherdev/nestova/internal/tracking/app"
	trackingdomain "github.com/ericfisherdev/nestova/internal/tracking/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// Kiosk read-window tuning. Chores mirrors the member-facing /tasks list's
// generation horizon (tasksadapter.listWindowDays); calendar and meals use a
// tighter week-ahead window appropriate for an at-a-glance wall display.
const (
	kioskChoreWindowDays    = 14
	kioskCalendarWindowDays = 7
	kioskRecentPhotoLimit   = 30
)

// kioskDisplayDateLayout / kioskDisplayDateTimeLayout format calendar and meal
// dates for the kiosk. Duplicated in miniature from calendaradapter's own
// unexported layout constants (web_view.go) rather than exported and shared,
// since kiosk's window and rendering are otherwise independent of that page.
const (
	kioskDisplayDateLayout     = "Jan 2, 2006"
	kioskDisplayDateTimeLayout = "Jan 2, 3:04 PM MST"
)

// kioskKindLabels mirrors calendaradapter's unexported kindLabels map for the
// same reason as the layout constants above.
var kioskKindLabels = map[calendarapp.CalendarItemKind]string{
	calendarapp.KindEvent:   "Event",
	calendarapp.KindTask:    "Chore",
	calendarapp.KindRenewal: "Renewal",
}

// kioskMealLabels renders a meals domain.Meal as its display label.
var kioskMealLabels = map[mealsdomain.Meal]string{
	mealsdomain.MealBreakfast: "Breakfast",
	mealsdomain.MealLunch:     "Lunch",
	mealsdomain.MealDinner:    "Dinner",
	mealsdomain.MealSnack:     "Snack",
}

// KioskWebHandlers serves the read-mostly /kiosk/* tabs, the device-activation
// link, and the single allowed mutation (marking a shopping item in-cart).
//
// It depends directly on each bounded context's APPLICATION-layer service or
// read repository — never on their adapter/WebHandlers types — because every
// existing WebHandlers read path (tasksadapter.WebHandlers.BuildGroups,
// trackingadapter.WebHandlers.Page, etc.) is built around a *household.Member
// and bakes in member-specific concerns this kiosk view must not carry (claim
// eligibility, "mine" highlighting, per-member action forms). Rebuilding
// read-only view models directly from the application services is the
// "extract shared builders" path the ticket calls for when a handler is
// member-coupled, without re-deriving each context's own join/query logic
// (which already lives in these services and repositories).
type KioskWebHandlers struct {
	kiosk          *kioskapp.KioskService
	taskInstances  tasksdomain.TaskInstanceRepository
	recurringTasks tasksdomain.RecurringTaskRepository
	calendar       *calendarapp.UnifiedCalendarService
	planner        *mealsapp.PlannerService
	recipes        mealsdomain.RecipeRepository
	shopping       *trackingapp.ShoppingListService
	ingredients    trackingdomain.IngredientNamer
	albums         *mediaapp.AlbumService
	photos         *mediaapp.PhotoService
	households     household.HouseholdRepository
	sm             *scs.SessionManager
	logger         *slog.Logger
	cookieSecure   bool
	now            func() time.Time
}

// NewKioskWebHandlers constructs KioskWebHandlers with all required
// dependencies. It panics when any dependency is nil so a misconfigured
// composition root is caught at startup. now defaults to time.Now.
func NewKioskWebHandlers(
	kiosk *kioskapp.KioskService,
	taskInstances tasksdomain.TaskInstanceRepository,
	recurringTasks tasksdomain.RecurringTaskRepository,
	calendar *calendarapp.UnifiedCalendarService,
	planner *mealsapp.PlannerService,
	recipes mealsdomain.RecipeRepository,
	shopping *trackingapp.ShoppingListService,
	ingredients trackingdomain.IngredientNamer,
	albums *mediaapp.AlbumService,
	photos *mediaapp.PhotoService,
	households household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
	cookieSecure bool,
	now func() time.Time,
) *KioskWebHandlers {
	switch {
	case kiosk == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil KioskService")
	case taskInstances == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil TaskInstanceRepository")
	case recurringTasks == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil RecurringTaskRepository")
	case calendar == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil UnifiedCalendarService")
	case planner == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil PlannerService")
	case recipes == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil RecipeRepository")
	case shopping == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil ShoppingListService")
	case ingredients == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil IngredientNamer")
	case albums == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil AlbumService")
	case photos == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil PhotoService")
	case households == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil HouseholdRepository")
	case sm == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil session manager")
	case logger == nil:
		panic("kiosk/adapter: NewKioskWebHandlers requires a non-nil logger")
	}
	if now == nil {
		now = time.Now
	}
	return &KioskWebHandlers{
		kiosk: kiosk, taskInstances: taskInstances, recurringTasks: recurringTasks,
		calendar: calendar, planner: planner, recipes: recipes, shopping: shopping,
		ingredients: ingredients, albums: albums, photos: photos, households: households,
		sm: sm, logger: logger, cookieSecure: cookieSecure, now: now,
	}
}

// ---------------------------------------------------------------------------
// Device activation
// ---------------------------------------------------------------------------

// Activate handles both GET /kiosk/activate?code=... (the one-click link a
// parent visits from the kiosk device's own browser, see the settings page's
// KioskActivationReveal) and POST /kiosk/activate (the manual entry form, for
// when scanning/following the link isn't convenient). It is deliberately
// public (not behind RequireKioskOrMember): the kiosk device has no identity
// yet until this request succeeds.
//
// A missing code (a bare GET /kiosk/activate, or the link's own retry) shows
// the manual entry form. A valid, unused, unexpired code redeems into a new
// device (see app.KioskService.Redeem's atomic contract), sets the device
// cookie, and redirects into the kiosk shell. An invalid/used/expired code
// re-shows the entry form with a generic error — never distinguishing which
// of the three applies, to avoid leaking whether a given code ever existed.
func (h *KioskWebHandlers) Activate(w http.ResponseWriter, r *http.Request) {
	var code string
	switch r.Method {
	case http.MethodGet:
		code = r.URL.Query().Get("code")
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !authadapter.VerifyCSRF(r, h.sm) {
			h.renderActivationForm(w, r, http.StatusForbidden, "Your session expired — please try again.")
			return
		}
		code = r.FormValue("code")
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if strings.TrimSpace(code) == "" {
		h.renderActivationForm(w, r, http.StatusOK, "")
		return
	}

	device, rawToken, err := h.kiosk.Redeem(r.Context(), code)
	if err != nil {
		switch {
		case errors.Is(err, kioskdomain.ErrActivationCodeNotFound),
			errors.Is(err, kioskdomain.ErrActivationCodeUsed),
			errors.Is(err, kioskdomain.ErrActivationCodeExpired):
			h.renderActivationForm(w, r, http.StatusUnauthorized, "That code is invalid, already used, or has expired.")
			return
		default:
			h.logger.ErrorContext(r.Context(), "kiosk: activate", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	SetCookie(w, rawToken, h.cookieSecure)
	h.logger.InfoContext(r.Context(), "kiosk device activated", "device_id", device.ID.String())
	http.Redirect(w, r, "/kiosk/chores", http.StatusSeeOther)
}

// renderActivationForm renders the manual activation-code entry page at
// status, with errMsg shown inline when non-empty.
func (h *KioskWebHandlers) renderActivationForm(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	view := components.KioskActivationFormView{
		Error:     errMsg,
		CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm),
	}
	if err := render.Render(r.Context(), w, status, components.KioskActivationPage(view)); err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: render activation form", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Chores tab
// ---------------------------------------------------------------------------

// Chores handles GET /kiosk/chores.
func (h *KioskWebHandlers) Chores(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := h.buildChoresView(r.Context(), householdID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: build chores view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, householdID, components.KioskTabChores, components.KioskChoresPage(view))
}

func (h *KioskWebHandlers) buildChoresView(ctx context.Context, householdID household.HouseholdID) (components.KioskChoresView, error) {
	today := tasksdomain.DateOf(h.now())

	activeTasks, err := h.recurringTasks.ListActive(ctx, householdID)
	if err != nil {
		return components.KioskChoresView{}, err
	}
	taskMeta := make(map[tasksdomain.RecurringTaskID]*tasksdomain.RecurringTask, len(activeTasks))
	for _, t := range activeTasks {
		taskMeta[t.ID] = t
	}

	members, err := h.households.ListMembers(ctx, householdID)
	if err != nil {
		return components.KioskChoresView{}, err
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}

	pending, err := h.taskInstances.ListByHousehold(ctx, householdID, tasksdomain.StatusPending, today, today.AddDate(0, 0, kioskChoreWindowDays))
	if err != nil {
		return components.KioskChoresView{}, err
	}
	overdue, err := h.taskInstances.ListByHousehold(ctx, householdID, tasksdomain.StatusOverdue, tasksdomain.DateOf(time.Time{}), today)
	if err != nil {
		return components.KioskChoresView{}, err
	}
	standing, err := h.taskInstances.ListStanding(ctx, householdID)
	if err != nil {
		return components.KioskChoresView{}, err
	}

	combined := make([]*tasksdomain.TaskInstance, 0, len(pending)+len(overdue)+len(standing))
	combined = append(combined, pending...)
	combined = append(combined, overdue...)
	combined = append(combined, standing...)

	sortable := make([]sortableChore, 0, len(combined))
	for _, inst := range combined {
		meta := taskMeta[inst.RecurringTaskID]
		title := "(archived)"
		category := "chore"
		if meta != nil {
			title = meta.Title
			category = meta.Category.String()
		}
		dueLabel := "Anytime"
		dueOn := today // standing instances sort alongside "today" — they have no due date of their own.
		if inst.Kind != tasksdomain.KindStanding && inst.DueOn != nil {
			dueOn = *inst.DueOn
			dueLabel = kioskDueLabel(dueOn, today)
		}
		row := components.KioskChoreView{Title: title, Category: category, DueLabel: dueLabel}
		if inst.AssigneeID != nil {
			if assignee, ok := memberByID[*inst.AssigneeID]; ok {
				row.AssigneeName = assignee.DisplayName
				row.AssigneeInitials = assignee.Initials()
				row.AssigneeColor = assignee.Color.String()
			}
		}
		sortable = append(sortable, sortableChore{row: row, dueOn: dueOn})
	}

	sort.Slice(sortable, func(i, j int) bool {
		if sortable[i].dueOn.Equal(sortable[j].dueOn) {
			return sortable[i].row.Title < sortable[j].row.Title
		}
		return sortable[i].dueOn.Before(sortable[j].dueOn)
	})
	rows := make([]components.KioskChoreView, len(sortable))
	for i, s := range sortable {
		rows[i] = s.row
	}
	return components.KioskChoresView{Chores: rows}, nil
}

// sortableChore pairs a chore row with its sort key (dueOn) so sort.Slice's
// swap moves both together. A previous version sorted the KioskChoreView rows
// via a separate, parallel []time.Time slice that sort.Slice's Swap never
// touched — after the first swap the two slices fell out of sync, so
// comparisons silently read the wrong due date for a given row. Keeping the
// key alongside the row in one struct makes that class of bug impossible.
type sortableChore struct {
	row   components.KioskChoreView
	dueOn time.Time
}

// kioskDueLabel renders a due date relative to today: "Today", "Tomorrow", or
// the short month-day form ("Jun 20"). A small, deliberate duplicate of
// tasksadapter's unexported dueLabel — see the KioskWebHandlers doc comment.
func kioskDueLabel(due, today time.Time) string {
	d := tasksdomain.DateOf(due)
	t := tasksdomain.DateOf(today)
	switch {
	case d.Equal(t):
		return "Today"
	case d.Equal(t.AddDate(0, 0, 1)):
		return "Tomorrow"
	default:
		return d.Format("Jan 2")
	}
}

// ---------------------------------------------------------------------------
// Calendar tab
// ---------------------------------------------------------------------------

// Calendar handles GET /kiosk/calendar.
func (h *KioskWebHandlers) Calendar(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := h.buildCalendarView(r.Context(), householdID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: build calendar view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, householdID, components.KioskTabCalendar, components.KioskCalendarPage(view))
}

func (h *KioskWebHandlers) buildCalendarView(ctx context.Context, householdID household.HouseholdID) (components.KioskCalendarView, error) {
	from := tasksdomain.DateOf(h.now())
	to := from.AddDate(0, 0, kioskCalendarWindowDays)

	items, err := h.calendar.List(ctx, householdID, from, to)
	if err != nil {
		return components.KioskCalendarView{}, err
	}
	views := make([]components.CalendarItemView, 0, len(items))
	for _, it := range items {
		when := it.Start.Format(kioskDisplayDateLayout)
		if !it.AllDay {
			when = it.Start.UTC().Format(kioskDisplayDateTimeLayout)
		}
		views = append(views, components.CalendarItemView{
			Kind:      string(it.Kind),
			KindLabel: kioskKindLabels[it.Kind],
			Title:     it.Title,
			When:      when,
			Color:     it.MemberColor,
		})
	}
	return components.KioskCalendarView{
		RangeLabel: from.Format("Jan 2") + " – " + to.Format("Jan 2, 2006"),
		Items:      views,
	}, nil
}

// ---------------------------------------------------------------------------
// Meals tab
// ---------------------------------------------------------------------------

// Meals handles GET /kiosk/meals.
func (h *KioskWebHandlers) Meals(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := h.buildMealsView(r.Context(), householdID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: build meals view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, householdID, components.KioskTabMeals, components.KioskMealsPage(view))
}

func (h *KioskWebHandlers) buildMealsView(ctx context.Context, householdID household.HouseholdID) (components.KioskMealsView, error) {
	weekStart := tasksdomain.DateOf(h.now())
	weekEnd := weekStart.AddDate(0, 0, 6)

	entries, err := h.planner.PlanForWeek(ctx, householdID, weekStart)
	if err != nil {
		return components.KioskMealsView{}, err
	}

	recipeTitles := make(map[mealsdomain.RecipeID]string, len(entries))
	for _, e := range entries {
		if _, cached := recipeTitles[e.RecipeID]; cached {
			continue
		}
		recipe, err := h.recipes.Get(ctx, householdID, e.RecipeID)
		if err != nil {
			return components.KioskMealsView{}, err
		}
		recipeTitles[e.RecipeID] = recipe.Title
	}

	dayByDate := make(map[time.Time]*components.KioskMealDayView)
	var order []time.Time
	for _, e := range entries {
		// MealPlanEntry.Date is already a UTC calendar date (midnight UTC) per
		// its own doc comment, so it is used directly as the grouping key
		// without a normalizing DateOf call (meals/domain has none).
		date := e.Date
		day, ok := dayByDate[date]
		if !ok {
			day = &components.KioskMealDayView{DateLabel: date.Format("Monday, Jan 2")}
			dayByDate[date] = day
			order = append(order, date)
		}
		day.Slots = append(day.Slots, components.KioskMealSlotView{
			MealLabel:   kioskMealLabels[e.Meal],
			RecipeTitle: recipeTitles[e.RecipeID],
		})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Before(order[j]) })

	days := make([]components.KioskMealDayView, 0, len(order))
	for _, date := range order {
		days = append(days, *dayByDate[date])
	}

	return components.KioskMealsView{
		WeekLabel: weekStart.Format("Jan 2") + " – " + weekEnd.Format("Jan 2, 2006"),
		Days:      days,
	}, nil
}

// ---------------------------------------------------------------------------
// Shopping tab
// ---------------------------------------------------------------------------

// Shopping handles GET /kiosk/shopping.
func (h *KioskWebHandlers) Shopping(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	view, err := h.buildShoppingView(r)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: build shopping view", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, householdID, components.KioskTabShopping, components.KioskShoppingPage(view))
}

func (h *KioskWebHandlers) buildShoppingView(r *http.Request) (components.KioskShoppingView, error) {
	householdID, _ := CurrentHouseholdID(r.Context())
	needed, err := h.shopping.ListByStatus(r.Context(), householdID, trackingdomain.StatusNeeded)
	if err != nil {
		return components.KioskShoppingView{}, err
	}
	inCart, err := h.shopping.ListByStatus(r.Context(), householdID, trackingdomain.StatusInCart)
	if err != nil {
		return components.KioskShoppingView{}, err
	}
	names, err := h.resolveIngredientNames(r.Context(), needed, inCart)
	if err != nil {
		return components.KioskShoppingView{}, err
	}
	return components.KioskShoppingView{
		Needed:    toKioskShoppingItemViews(needed, names),
		InCart:    toKioskShoppingItemViews(inCart, names),
		CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm),
	}, nil
}

// resolveIngredientNames batch-resolves the canonical names for every distinct
// catalogue ingredient referenced across groups, mirroring
// trackingadapter.WebHandlers.resolveIngredientNames so a kiosk shopping list
// never pays an N+1 lookup for catalogue-sourced (as opposed to free-text
// manual) items.
func (h *KioskWebHandlers) resolveIngredientNames(ctx context.Context, groups ...[]*trackingdomain.ShoppingListItem) (map[trackingdomain.IngredientID]string, error) {
	idSet := make(map[trackingdomain.IngredientID]struct{})
	for _, group := range groups {
		for _, item := range group {
			if item.IngredientID != nil {
				idSet[*item.IngredientID] = struct{}{}
			}
		}
	}
	ids := make([]trackingdomain.IngredientID, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	return h.ingredients.NamesByIDs(ctx, ids)
}

// toKioskShoppingItemViews maps domain shopping items to the same
// components.ShoppingItemView the member-facing /groceries page uses.
func toKioskShoppingItemViews(items []*trackingdomain.ShoppingListItem, names map[trackingdomain.IngredientID]string) []components.ShoppingItemView {
	views := make([]components.ShoppingItemView, 0, len(items))
	for _, item := range items {
		name := item.Name
		if name == "" && item.IngredientID != nil {
			name = names[*item.IngredientID]
		}
		views = append(views, components.ShoppingItemView{
			ID:            item.ID.String(),
			Name:          name,
			QuantityLabel: kioskFormatQuantity(item.Quantity),
			SourceLabel:   kioskSourceLabel(item.Source),
			Status:        item.Status.String(),
		})
	}
	return views
}

// kioskFormatQuantity renders a household.Quantity as "<amount> <unit>",
// mirroring trackingadapter's own (unexported) formatQuantity — household.
// Quantity carries no String() method of its own by design (see its doc
// comment), so every adapter that displays one formats it locally.
func kioskFormatQuantity(q household.Quantity) string {
	amount := strconv.FormatFloat(q.Amount, 'f', -1, 64)
	return amount + " " + q.Unit.String()
}

// kioskSourceLabel renders a shopping item's source as a display label,
// mirroring trackingadapter's own (unexported) label mapping.
func kioskSourceLabel(source trackingdomain.ItemSource) string {
	switch source {
	case trackingdomain.SourceRestock:
		return "Restock"
	case trackingdomain.SourceMealPlan:
		return "Meal plan"
	case trackingdomain.SourcePantryLow:
		return "Low pantry"
	default:
		return "Manual"
	}
}

// MarkInCart handles POST /kiosk/shopping/{id}/in-cart — the one member-free
// mutation the kiosk exposes (AC5). It always transitions to StatusInCart
// regardless of the item's current status: there is exactly one reachable
// target, so there is nothing to validate about the source state, unlike the
// member-facing /groceries/shopping/{id}/status endpoint, which accepts any of
// the three lifecycle statuses and is not exposed here.
func (h *KioskWebHandlers) MarkInCart(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	itemID, err := trackingdomain.ParseShoppingListItemID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid shopping list item id", http.StatusBadRequest)
		return
	}
	if _, err := h.shopping.MarkInCart(r.Context(), householdID, itemID); err != nil {
		switch {
		case errors.Is(err, trackingdomain.ErrShoppingListItemNotFound):
			http.NotFound(w, r)
			return
		case errors.Is(err, trackingdomain.ErrShoppingListItemNotInCartable):
			// The item exists but is past the point where "in cart" still
			// applies (already purchased) — a stale kiosk page replaying an
			// in-cart submit after the item was marked purchased elsewhere.
			http.Error(w, "item is no longer in a cartable state", http.StatusConflict)
			return
		default:
			h.logger.ErrorContext(r.Context(), "kiosk: mark shopping item in-cart", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	target := "/kiosk/shopping"
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Photos tab
// ---------------------------------------------------------------------------

// Photos handles GET /kiosk/photos.
func (h *KioskWebHandlers) Photos(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	photos, err := h.photos.List(r.Context(), householdID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: list photos", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	view := components.KioskPhotosView{Photos: toKioskPhotoViews(photos)}
	h.render(w, r, householdID, components.KioskTabPhotos, components.KioskPhotosPage(view))
}

// toKioskPhotoViews maps the household's photos to the read-only grid view,
// most-recent-first, bounded to kioskRecentPhotoLimit so the wall display never
// tries to render a household's entire multi-year photo history at once.
func toKioskPhotoViews(photos []*mediadomain.Photo) []components.PhotoView {
	if len(photos) > kioskRecentPhotoLimit {
		photos = photos[len(photos)-kioskRecentPhotoLimit:]
	}
	views := make([]components.PhotoView, 0, len(photos))
	for i := len(photos) - 1; i >= 0; i-- {
		p := photos[i]
		pv := components.PhotoView{
			ID:      p.ID.String(),
			RawURL:  "/kiosk/photos/" + p.ID.String() + "/raw",
			Caption: p.Caption,
		}
		if p.TakenAt != nil {
			pv.TakenOn = p.TakenAt.Format(kioskDisplayDateLayout)
		}
		views = append(views, pv)
	}
	return views
}

// Raw handles GET /kiosk/photos/{id}/raw: streams a photo's bytes to the
// current kiosk/member identity's own household only. It calls
// mediaapp.PhotoService.OpenBytes directly (the application-layer service,
// not mediaadapter.WebHandlers.Raw) so it never depends on the member-coupled
// media adapter package — see the KioskWebHandlers doc comment.
func (h *KioskWebHandlers) Raw(w http.ResponseWriter, r *http.Request) {
	householdID, ok := CurrentHouseholdID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := mediadomain.ParsePhotoID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid photo id", http.StatusBadRequest)
		return
	}
	rc, contentType, err := h.photos.OpenBytes(r.Context(), householdID, id)
	if err != nil {
		if errors.Is(err, mediadomain.ErrPhotoNotFound) {
			http.NotFound(w, r)
			return
		}
		h.logger.ErrorContext(r.Context(), "kiosk: open photo bytes", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if _, err := io.Copy(w, rc); err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: stream photo bytes", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Shared: shell + screensaver
// ---------------------------------------------------------------------------

// render wraps content in the kiosk shell (bottom tab bar + idle screensaver)
// and writes it. The kiosk shell is a standalone document (not the member
// dashboard shell — see KioskLayout's doc comment), so this always renders the
// full page rather than branching on an HTMX partial request.
func (h *KioskWebHandlers) render(w http.ResponseWriter, r *http.Request, householdID household.HouseholdID, active components.KioskTab, content templ.Component) {
	screensaver, err := h.buildScreensaver(r.Context(), householdID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: build screensaver", "error", err)
		// The screensaver is a non-critical enhancement (AC4's idle behavior);
		// a failure to load it must not block the tab the operator actually
		// asked for, so this degrades to the empty-slides placeholder instead
		// of a 500.
		screensaver = components.KioskScreensaverView{}
	}
	props := components.KioskShellProps{Active: active, Screensaver: screensaver}
	if err := render.Render(r.Context(), w, http.StatusOK, components.KioskLayout(props, content)); err != nil {
		h.logger.ErrorContext(r.Context(), "kiosk: render shell", "error", err)
	}
}

// buildScreensaver loads the household's earliest-created album (its de facto
// default, in the absence of an explicit "default album" flag — see the
// KioskWebHandlers doc comment) as the idle-timeout slideshow. A household
// with no album yet gets an empty (but still idle-triggered) screensaver.
func (h *KioskWebHandlers) buildScreensaver(ctx context.Context, householdID household.HouseholdID) (components.KioskScreensaverView, error) {
	albums, err := h.albums.List(ctx, householdID)
	if err != nil {
		return components.KioskScreensaverView{}, err
	}
	if len(albums) == 0 {
		return components.KioskScreensaverView{}, nil
	}
	album := albums[0]

	items, err := h.albums.Playlist(ctx, householdID, album.ID)
	if err != nil {
		return components.KioskScreensaverView{}, err
	}
	members, err := h.households.ListMembers(ctx, householdID)
	if err != nil {
		return components.KioskScreensaverView{}, err
	}
	colorByID := make(map[household.MemberID]string, len(members))
	for _, m := range members {
		colorByID[m.ID] = m.Color.String()
	}

	slides := make([]components.SlideView, 0, len(items))
	for _, it := range items {
		slide := components.SlideView{
			RawURL:  "/kiosk/photos/" + it.PhotoID.String() + "/raw",
			Caption: it.Caption,
		}
		if it.UploadedBy != nil {
			slide.UploaderColor = colorByID[*it.UploadedBy]
		}
		slides = append(slides, slide)
	}
	return components.KioskScreensaverView{
		RotationSeconds: album.Rotation.Seconds(),
		Slides:          slides,
	}, nil
}
