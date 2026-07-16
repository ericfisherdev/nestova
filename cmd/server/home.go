package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	calendaradapter "github.com/ericfisherdev/nestova/internal/calendar/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	mealsadapter "github.com/ericfisherdev/nestova/internal/meals/adapter"
	mediaadapter "github.com/ericfisherdev/nestova/internal/media/adapter"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	subscriptionsadapter "github.com/ericfisherdev/nestova/internal/subscriptions/adapter"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	trackingadapter "github.com/ericfisherdev/nestova/internal/tracking/adapter"
	"github.com/ericfisherdev/nestova/web/components"
)

// groceriesNavHref is the canonical href for the groceries nav item. Defined as a
// constant so home.go and tests reference the same value.
const groceriesNavHref = "/groceries"

// rewardsNavHref is the canonical href for the rewards / scoreboard nav item.
// Defined as a constant so home.go and tests reference the same value.
const rewardsNavHref = "/rewards"

// mealsNavHref is the canonical href for the meals & recipes nav item.
const mealsNavHref = "/meals"

// primaryNav returns the fixed sidebar navigation, marking the item whose href
// equals active (empty selects none).
func primaryNav(active string) []components.NavItem {
	defs := []components.NavItem{
		{Label: "Calendar", Href: "/calendar"},
		{Label: "Chores", Href: "/tasks"},
		{Label: "Rewards", Href: rewardsNavHref},
		{Label: "Meals & Recipes", Href: mealsNavHref},
		{Label: "Groceries", Href: groceriesNavHref},
		{Label: "Subscriptions", Href: "/subscriptions"},
		{Label: "Photos", Href: "/photos"},
	}
	for i := range defs {
		defs[i].Active = defs[i].Href == active
	}
	return defs
}

// toMemberViews maps a slice of domain Members to the MemberView view model
// used by the app shell sidebar.
func toMemberViews(members []*household.Member) []components.MemberView {
	views := make([]components.MemberView, 0, len(members))
	for _, m := range members {
		views = append(views, components.MemberView{
			Name:     m.DisplayName,
			Initials: m.Initials(),
			Color:    m.Color.String(),
		})
	}
	return views
}

// registerWebRoutes wires the user-facing pages. The dashboard requires
// authentication (RequireMember); auth routes (login, logout, onboarding) are
// public. The household repository is required so the dashboard can load real
// members from the database.
func registerWebRoutes(
	mux *http.ServeMux,
	logger *slog.Logger,
	sm *scs.SessionManager,
	authHandlers *authadapter.Handlers,
	onboardingHandlers *authadapter.OnboardingHandlers,
	households household.HouseholdRepository,
	taskHandlers *tasksadapter.WebHandlers,
	tradeHandlers *tasksadapter.TradeWebHandlers,
	gamificationHandlers *tasksadapter.GamificationWebHandlers,
	groceryHandlers *trackingadapter.WebHandlers,
	mealsHandlers *mealsadapter.WebHandlers,
	calendarHandlers *calendaradapter.WebHandlers,
) {
	// Auth routes — public.
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	mux.HandleFunc("POST /logout", authHandlers.Logout)

	// Onboarding routes — public (first-run guard enforced inside the handlers).
	mux.HandleFunc("GET /onboarding", onboardingHandlers.OnboardingPage)
	mux.HandleFunc("POST /onboarding", onboardingHandlers.Onboard)

	requireMember := authadapter.RequireMember(sm)

	// Add-member routes — RequireMember-gated.
	mux.Handle("GET /members/new", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, _ := authadapter.CurrentMember(r.Context())
		props, nav := dashboardShell(r, sm, member, households, logger, "")
		onboardingHandlers.NewMemberPage(w, r, props, nav)
	})))
	mux.Handle("POST /members", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, _ := authadapter.CurrentMember(r.Context())
		props, nav := dashboardShell(r, sm, member, households, logger, "")
		onboardingHandlers.AddMember(w, r, props, nav)
	})))

	// Dashboard — protected: RequireMember redirects unauthenticated visitors
	// to /login?next=/ before the handler runs.
	mux.Handle("GET /{$}", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, _ := authadapter.CurrentMember(r.Context())
		props, nav := dashboardShell(r, sm, member, households, logger, "")
		layout := func(c templ.Component) templ.Component {
			return components.Layout(props, nav, c)
		}
		// NES-122: pending chore-trade cards are cross-cutting dashboard
		// content (tasks bounded context), composed here rather than inside
		// components.Dashboard itself, matching how this route already
		// composes ShellProps/nav from the household repository.
		trades := tradeHandlers.DashboardSections(r, member)
		if err := render.Page(r.Context(), w, r, layout, components.Dashboard(trades)); err != nil {
			logger.ErrorContext(r.Context(), "render dashboard", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	})))

	// Tasks routes — RequireMember-gated.
	// GET /tasks           renders the chores & maintenance list.
	// GET /tasks/new       renders the create-recurring-task form.
	// POST /tasks          creates a new recurring task.
	// POST /tasks/{id}/complete|skip|claim are the three HTMX action endpoints.
	// GET /tasks/groups    re-renders the grouped task list fragment for the
	//                      claim countdown's passive-expiry refresh (NES-118).
	//
	// The layout callback is constructed per-request so the request context
	// (for CSRF token generation and member list loading) is always available.
	// Go's ServeMux distinguishes POST /tasks from POST /tasks/{id}/complete
	// because the latter's pattern has a path segment after the prefix.
	mux.Handle("GET /tasks", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "/tasks")
				return components.Layout(props, nav, c)
			}
		}
		taskHandlers.List(layoutFn)(w, r)
	})))
	mux.Handle("GET /tasks/new", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "/tasks")
				return components.Layout(props, nav, c)
			}
		}
		taskHandlers.NewTaskPage(layoutFn)(w, r)
	})))
	mux.Handle("POST /tasks", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "/tasks")
				return components.Layout(props, nav, c)
			}
		}
		taskHandlers.CreateTask(layoutFn)(w, r)
	})))
	mux.Handle("POST /tasks/{id}/complete", requireMember(http.HandlerFunc(taskHandlers.Complete)))
	mux.Handle("POST /tasks/{id}/skip", requireMember(http.HandlerFunc(taskHandlers.Skip)))
	mux.Handle("POST /tasks/{id}/claim", requireMember(http.HandlerFunc(taskHandlers.Claim)))
	// GET /tasks/groups re-renders the #task-groups container (NES-118): the
	// claim countdown badge's client-side timer calls this once a claim's
	// expiry passes, so the reverted claim's row is re-grouped under its
	// correct heading (not just updated in place under the wrong one)
	// without a full page reload.
	mux.Handle("GET /tasks/groups", requireMember(http.HandlerFunc(taskHandlers.Groups)))

	// Chore trade routes (NES-122) — RequireMember-gated.
	// GET  /tasks/{id}/propose-trade   renders the propose-trade picker for
	//                                  one of the viewing member's own chores.
	// POST /trades                     creates a new trade proposal.
	// POST /trades/{id}/accept         responder accepts a pending proposal.
	// POST /trades/{id}/decline        responder declines a pending proposal.
	// POST /trades/{id}/cancel         proposer withdraws a pending proposal.
	// GET  /trades/history             parent-only (owner/adult) trade history.
	mux.Handle("GET /tasks/{id}/propose-trade", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "/tasks")
				return components.Layout(props, nav, c)
			}
		}
		tradeHandlers.ProposePickerPage(layoutFn)(w, r)
	})))
	mux.Handle("POST /trades", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "/tasks")
				return components.Layout(props, nav, c)
			}
		}
		tradeHandlers.ProposeTrade(layoutFn)(w, r)
	})))
	mux.Handle("POST /trades/{id}/accept", requireMember(http.HandlerFunc(tradeHandlers.Accept)))
	mux.Handle("POST /trades/{id}/decline", requireMember(http.HandlerFunc(tradeHandlers.Decline)))
	mux.Handle("POST /trades/{id}/cancel", requireMember(http.HandlerFunc(tradeHandlers.Cancel)))
	mux.Handle("GET /trades/history", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "")
				return components.Layout(props, nav, c)
			}
		}
		tradeHandlers.HistoryPage(layoutFn)(w, r)
	})))

	// Rewards / scoreboard routes — RequireMember-gated.
	// GET /rewards            renders the scoreboard + rewards catalog.
	// POST /rewards/{id}/redeem exchanges the current member's points for a reward.
	mux.Handle("GET /rewards", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, rewardsNavHref)
				return components.Layout(props, nav, c)
			}
		}
		gamificationHandlers.RewardsPage(layoutFn)(w, r)
	})))
	mux.Handle("POST /rewards/{id}/redeem", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, rewardsNavHref)
				return components.Layout(props, nav, c)
			}
		}
		gamificationHandlers.Redeem(layoutFn)(w, r)
	})))

	// Groceries routes — RequireMember-gated (NES-45).
	// GET  /groceries                          renders the usage tracker, pantry,
	//                                          and shopping-list sections.
	// POST /groceries/items                    registers a new tracked item.
	// POST /groceries/items/{id}/usage         logs a usage event for an item.
	// POST /groceries/pantry                   adds an on-hand pantry item.
	// POST /groceries/pantry/{id}/consume      decreases a pantry item's quantity.
	// POST /groceries/pantry/{id}/adjust       increases a pantry item's quantity.
	// POST /groceries/shopping                 adds an ad-hoc manual shopping item.
	// POST /groceries/shopping/{id}/status     transitions a shopping item's status.
	//
	// The layout callback is constructed per-request so the request context (CSRF
	// token, member list) is always available, mirroring the tasks routes.
	mux.Handle("GET /groceries", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, groceriesNavHref)
				return components.Layout(props, nav, c)
			}
		}
		groceryHandlers.Page(layoutFn)(w, r)
	})))
	mux.Handle("POST /groceries/items", requireMember(http.HandlerFunc(groceryHandlers.RegisterItem)))
	mux.Handle("POST /groceries/items/{id}/usage", requireMember(http.HandlerFunc(groceryHandlers.LogUsage)))
	mux.Handle("POST /groceries/pantry", requireMember(http.HandlerFunc(groceryHandlers.PantryAdd)))
	mux.Handle("POST /groceries/pantry/{id}/consume", requireMember(http.HandlerFunc(groceryHandlers.PantryConsume)))
	mux.Handle("POST /groceries/pantry/{id}/adjust", requireMember(http.HandlerFunc(groceryHandlers.PantryAdjust)))
	mux.Handle("POST /groceries/shopping", requireMember(http.HandlerFunc(groceryHandlers.ShoppingAdd)))
	mux.Handle("POST /groceries/shopping/{id}/status", requireMember(http.HandlerFunc(groceryHandlers.ShoppingTransition)))

	// Meals routes — RequireMember-gated (NES-62).
	// GET  /meals                       renders the recipe box, planner, and finder.
	// POST /meals/finder                runs the finder (CSRF-verified because the
	//                                   ad-hoc path can create catalogue ingredients).
	// POST /meals/recipes               creates a box recipe.
	// POST /meals/recipes/{id}          edits a box recipe.
	// POST /meals/recipes/{id}/delete   deletes a box recipe.
	// POST /meals/plan                  assigns a recipe to a (date, meal) slot.
	// POST /meals/plan/clear            clears a slot.
	// POST /meals/plan/generate         generates the shopping list from the week.
	//
	// The layout callback is constructed per-request so the request context (CSRF
	// token, member list) is always available, mirroring the groceries routes.
	mux.Handle("GET /meals", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, mealsNavHref)
				return components.Layout(props, nav, c)
			}
		}
		mealsHandlers.Page(layoutFn)(w, r)
	})))
	mux.Handle("POST /meals/finder", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		layoutFn := func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, mealsNavHref)
				return components.Layout(props, nav, c)
			}
		}
		mealsHandlers.Finder(layoutFn)(w, r)
	})))
	mux.Handle("POST /meals/recipes", requireMember(http.HandlerFunc(mealsHandlers.CreateRecipe)))
	mux.Handle("POST /meals/recipes/{id}", requireMember(http.HandlerFunc(mealsHandlers.EditRecipe)))
	mux.Handle("POST /meals/recipes/{id}/delete", requireMember(http.HandlerFunc(mealsHandlers.DeleteRecipe)))
	mux.Handle("POST /meals/plan", requireMember(http.HandlerFunc(mealsHandlers.AssignMeal)))
	mux.Handle("POST /meals/plan/clear", requireMember(http.HandlerFunc(mealsHandlers.ClearMeal)))
	mux.Handle("POST /meals/plan/generate", requireMember(http.HandlerFunc(mealsHandlers.GenerateGroceries)))

	// Calendar Google-account connection (NES-67) — RequireMember-gated. Connect
	// starts the OAuth flow (CSRF-verified POST); the callback completes it. The
	// callback path must match GOOGLE_REDIRECT_URL.
	mux.Handle("POST /calendar/google/connect", requireMember(http.HandlerFunc(calendarHandlers.Connect)))
	mux.Handle("GET /calendar/google/callback", requireMember(http.HandlerFunc(calendarHandlers.Callback)))
}

// registerCalendarSubscriptionPages wires the NES-70 calendar view and
// subscriptions UI. It is separate from registerWebRoutes so those routes can be
// added without changing the shared route builder's signature.
func registerCalendarSubscriptionPages(
	mux *http.ServeMux,
	logger *slog.Logger,
	sm *scs.SessionManager,
	households household.HouseholdRepository,
	calendarView *calendaradapter.ViewHandlers,
	subscriptionHandlers *subscriptionsadapter.WebHandlers,
) {
	requireMember := authadapter.RequireMember(sm)
	layoutFor := func(r *http.Request, active string) func(member *household.Member) func(templ.Component) templ.Component {
		return func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, active)
				return components.Layout(props, nav, c)
			}
		}
	}

	// Unified calendar page (NES-69/70).
	mux.Handle("GET /calendar", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calendarView.Page(layoutFor(r, "/calendar"), time.Now)(w, r)
	})))

	// Subscriptions UI (NES-70) — list + rollup and the add/edit/deactivate actions.
	mux.Handle("GET /subscriptions", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subscriptionHandlers.Page(layoutFor(r, "/subscriptions"))(w, r)
	})))
	mux.Handle("POST /subscriptions", requireMember(http.HandlerFunc(subscriptionHandlers.Add)))
	mux.Handle("POST /subscriptions/{id}", requireMember(http.HandlerFunc(subscriptionHandlers.Edit)))
	mux.Handle("POST /subscriptions/{id}/deactivate", requireMember(http.HandlerFunc(subscriptionHandlers.Deactivate)))
}

// registerMediaPages wires the NES-75 photo management UI: the /photos page, the
// upload and album-management actions, the #photo-grid refresh fragment the
// upload queue drives after a batch drains (NES-124), and the tenant-checked
// raw-bytes endpoint.
func registerMediaPages(
	mux *http.ServeMux,
	logger *slog.Logger,
	sm *scs.SessionManager,
	households household.HouseholdRepository,
	mediaHandlers *mediaadapter.WebHandlers,
) {
	requireMember := authadapter.RequireMember(sm)
	layoutFor := func(r *http.Request) func(member *household.Member) func(templ.Component) templ.Component {
		return func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "/photos")
				return components.Layout(props, nav, c)
			}
		}
	}

	mux.Handle("GET /photos", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaHandlers.Page(layoutFor(r))(w, r)
	})))
	mux.Handle("POST /photos", requireMember(http.HandlerFunc(mediaHandlers.Upload)))
	// GET /photos/grid re-renders the #photo-grid fragment (NES-124): the
	// client-side upload queue triggers this once after a whole drag-and-drop
	// batch drains, so the grid refreshes once regardless of batch size.
	mux.Handle("GET /photos/grid", requireMember(http.HandlerFunc(mediaHandlers.Grid)))
	mux.Handle("GET /photos/{id}/raw", requireMember(http.HandlerFunc(mediaHandlers.Raw)))
	mux.Handle("POST /photos/{id}/delete", requireMember(http.HandlerFunc(mediaHandlers.DeletePhoto)))
	mux.Handle("POST /photos/{id}/add-to-album", requireMember(http.HandlerFunc(mediaHandlers.AddPhoto)))
	mux.Handle("POST /albums", requireMember(http.HandlerFunc(mediaHandlers.CreateAlbum)))
	mux.Handle("POST /albums/{id}", requireMember(http.HandlerFunc(mediaHandlers.ConfigureAlbum)))
	mux.Handle("POST /albums/{id}/photos/{photoID}/remove", requireMember(http.HandlerFunc(mediaHandlers.RemovePhoto)))
	mux.Handle("POST /albums/{id}/photos/{photoID}/move", requireMember(http.HandlerFunc(mediaHandlers.MovePhoto)))
	// Full-screen rotating slideshow (NES-76) — a standalone page, not the shell.
	mux.Handle("GET /album/{id}", requireMember(http.HandlerFunc(mediaHandlers.AlbumViewer)))
}

// dashboardShell builds the ShellProps and nav slice for a given protected
// page. It loads the household member list from the database so the sidebar
// Family section reflects real persisted members. On error it falls back to an
// empty member list rather than failing the entire request.
func dashboardShell(
	r *http.Request,
	sm *scs.SessionManager,
	currentMember *household.Member,
	households household.HouseholdRepository,
	logger *slog.Logger,
	activeNav string,
) (components.ShellProps, []components.NavItem) {
	var memberViews []components.MemberView
	if currentMember != nil {
		members, err := households.ListMembers(r.Context(), currentMember.HouseholdID)
		if err != nil {
			logger.ErrorContext(r.Context(), "dashboard: list members", "error", err)
		} else {
			memberViews = toMemberViews(members)
		}
	}
	props := components.ShellProps{
		Members:   memberViews,
		CSRFToken: authadapter.GetCSRFToken(r.Context(), sm),
	}
	return props, primaryNav(activeNav)
}
