package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	calendaradapter "github.com/ericfisherdev/nestova/internal/calendar/adapter"
	deeplinkadapter "github.com/ericfisherdev/nestova/internal/deeplink/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
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
	// POST /rewards/redemptions/{id}/cancel — member self-cancel of their own
	// still-pending redemption (NES-127); refunds the debited points.
	mux.Handle("POST /rewards/redemptions/{id}/cancel", requireMember(http.HandlerFunc(gamificationHandlers.CancelRedemption)))

	// Reward catalogue admin routes (NES-126) — RequireMember-gated at the
	// router; each handler additionally checks isParent (owner/adult) and
	// returns 403 for a child member, mirroring the /trades/history gate.
	// GET  /admin/rewards            parent-only catalogue list (active + archived).
	// GET  /admin/rewards/new        create-reward form.
	// POST /admin/rewards            create-reward submit.
	// GET  /admin/rewards/{id}/edit  edit-reward form, pre-filled.
	// POST /admin/rewards/{id}       edit-reward submit.
	// POST /admin/rewards/{id}/archive retires a reward from the storefront.
	rewardAdminLayoutFn := func(r *http.Request) func(member *household.Member) func(templ.Component) templ.Component {
		return func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, rewardsNavHref)
				return components.Layout(props, nav, c)
			}
		}
	}
	mux.Handle("GET /admin/rewards", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gamificationHandlers.RewardsAdminPage(rewardAdminLayoutFn(r))(w, r)
	})))
	mux.Handle("GET /admin/rewards/new", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gamificationHandlers.NewRewardPage(rewardAdminLayoutFn(r))(w, r)
	})))
	mux.Handle("POST /admin/rewards", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gamificationHandlers.CreateReward(rewardAdminLayoutFn(r))(w, r)
	})))
	mux.Handle("GET /admin/rewards/{id}/edit", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gamificationHandlers.EditRewardPage(rewardAdminLayoutFn(r))(w, r)
	})))
	mux.Handle("POST /admin/rewards/{id}", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gamificationHandlers.UpdateReward(rewardAdminLayoutFn(r))(w, r)
	})))
	mux.Handle("POST /admin/rewards/{id}/archive", requireMember(http.HandlerFunc(gamificationHandlers.ArchiveReward)))

	// Redemption fulfillment inbox actions (NES-127) — parent-only (owner/
	// adult); each handler re-checks the role itself, mirroring the reward
	// admin routes' gate immediately above.
	// POST /admin/rewards/redemptions/{id}/fulfill approves a pending redemption.
	// POST /admin/rewards/redemptions/{id}/deny     rejects it and refunds the points.
	mux.Handle("POST /admin/rewards/redemptions/{id}/fulfill", requireMember(http.HandlerFunc(gamificationHandlers.FulfillRedemption)))
	mux.Handle("POST /admin/rewards/redemptions/{id}/deny", requireMember(http.HandlerFunc(gamificationHandlers.DenyRedemption)))

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

// registerChoreProofPhotoRoutes wires the NES-119 chore-proof photo upload
// endpoint and its NES-120 raw-bytes serving route. It is deliberately its
// own registration function rather than a case added to registerMediaPages:
// the routes live under the tasks-owned /tasks/... prefix even though their
// handlers are implemented in the media bounded context — the composition
// root (this file, mirroring the pattern NES-26's onboarding provisioner
// established) is the one place allowed to know about both, so neither
// tasks/adapter nor media/adapter imports the other.
func registerChoreProofPhotoRoutes(
	mux *http.ServeMux,
	sm *scs.SessionManager,
	choreProofHandlers *mediaadapter.ChoreProofWebHandlers,
) {
	requireMember := authadapter.RequireMember(sm)
	mux.Handle("POST /tasks/{id}/photos", requireMember(http.HandlerFunc(choreProofHandlers.Upload)))
	// NES-120: the /tasks chore row's capture/review section loads before/
	// after images from this route.
	mux.Handle("GET /tasks/photos/{id}/raw", requireMember(http.HandlerFunc(choreProofHandlers.Raw)))
}

// registerSettingsPage wires the shared /settings page: the auth context's
// per-member MFA section (NES-134), rendered for EVERY member, and the kiosk
// context's device section (NES-128), rendered only for a parent
// (owner/adult) member. RequireMember-gated at the router — the page itself
// is no longer parent-only (NES-134 moved that gate to the kiosk section
// specifically; the parent-only role check for kiosk mutations still happens
// inside kioskadapter.SettingsWebHandlers, mirroring /admin/rewards and
// /trades/history's per-handler role checks).
//
// Neither adapter package imports the other (kioskadapter knows nothing of
// MFA; authadapter knows nothing of kiosk devices) — this function is the
// one place allowed to know about both, composing their independently built
// section views into one components.SettingsPage, mirroring
// registerChoreProofPhotoRoutes' established composition-root pattern.
func registerSettingsPage(
	mux *http.ServeMux,
	logger *slog.Logger,
	sm *scs.SessionManager,
	households household.HouseholdRepository,
	settingsHandlers *kioskadapter.SettingsWebHandlers,
	mfaHandlers *authadapter.MFAWebHandlers,
) {
	const settingsPath = "/settings"
	requireMember := authadapter.RequireMember(sm)
	layoutFor := func(r *http.Request) func(member *household.Member) func(templ.Component) templ.Component {
		return func(member *household.Member) func(templ.Component) templ.Component {
			return func(c templ.Component) templ.Component {
				props, nav := dashboardShell(r, sm, member, households, logger, "")
				return components.Layout(props, nav, c)
			}
		}
	}

	// composePage builds the combined SettingsView from both contexts'
	// independently built section views and renders the full page at
	// status. It is used by every mutating action across both sections
	// (not just GET), since a one-time reveal produced by one section's
	// mutation must render alongside the OTHER section's current state, and
	// a redirect would lose that reveal. kioskReveal/mfaEnrollReveal/
	// mfaRecoveryReveal/mfaErr are non-nil/non-empty only in the one
	// response produced by the mutation that generated them.
	composePage := func(
		w http.ResponseWriter, r *http.Request, member *household.Member, status int,
		kioskReveal *components.KioskActivationReveal,
		mfaEnrollReveal *components.MFAEnrollReveal,
		mfaRecoveryReveal *components.MFARecoveryCodesReveal,
		mfaErr string,
	) {
		kioskView, showKiosk, err := settingsHandlers.SectionView(r.Context(), member, kioskReveal)
		if err != nil {
			logger.ErrorContext(r.Context(), "settings: build kiosk section", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		mfaView, err := mfaHandlers.SectionView(r.Context(), member, mfaEnrollReveal, mfaRecoveryReveal, mfaErr)
		if err != nil {
			logger.ErrorContext(r.Context(), "settings: build mfa section", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		view := components.SettingsView{
			ShowKioskSection: showKiosk,
			Kiosk:            kioskView,
			MFA:              mfaView,
			CSRFToken:        authadapter.GetCSRFToken(r.Context(), sm),
		}
		if err := render.Render(r.Context(), w, status, layoutFor(r)(member)(components.SettingsPage(view))); err != nil {
			logger.ErrorContext(r.Context(), "settings: render page", "error", err)
		}
	}

	mux.Handle("GET /settings", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		composePage(w, r, member, http.StatusOK, nil, nil, nil, "")
	})))

	// Kiosk section mutations (parent-only, enforced inside settingsHandlers).
	mux.Handle("POST /settings/kiosk/generate", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, reveal, ok := settingsHandlers.CreateActivationCode(w, r)
		if !ok {
			return
		}
		// This response reveals a live credential (even though it is
		// short-lived and single-use): it must never be cached by a shared
		// proxy or stored in the browser's disk cache.
		w.Header().Set("Cache-Control", "no-store")
		composePage(w, r, member, http.StatusOK, reveal, nil, nil, "")
	})))
	mux.Handle("POST /settings/kiosk/{id}/revoke", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := settingsHandlers.RevokeKioskToken(w, r)
		if !ok {
			return
		}
		if render.IsHTMX(r) {
			w.Header().Set("HX-Redirect", settingsPath)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, settingsPath, http.StatusSeeOther)
	})))

	// MFA section mutations (any member acts on their own enrollment; the
	// admin reset is owner-only, enforced inside mfaHandlers).
	mux.Handle("POST /settings/mfa/enroll", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, reveal, ok := mfaHandlers.Enroll(w, r)
		if !ok {
			return
		}
		composePage(w, r, member, http.StatusOK, nil, reveal, nil, "")
	})))
	mux.Handle("POST /settings/mfa/confirm", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, reveal, errMsg, status, ok := mfaHandlers.Confirm(w, r)
		if !ok {
			return
		}
		// A successful confirm reveals the member's recovery codes exactly
		// once: this response must never be cached.
		if reveal != nil {
			w.Header().Set("Cache-Control", "no-store")
		}
		composePage(w, r, member, status, nil, nil, reveal, errMsg)
	})))
	mux.Handle("POST /settings/mfa/recovery-codes/regenerate", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, reveal, errMsg, status, ok := mfaHandlers.RegenerateRecoveryCodes(w, r)
		if !ok {
			return
		}
		if reveal != nil {
			w.Header().Set("Cache-Control", "no-store")
		}
		composePage(w, r, member, status, nil, nil, reveal, errMsg)
	})))
	mux.Handle("POST /settings/mfa/disenroll", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, errMsg, status, ok := mfaHandlers.Disenroll(w, r, settingsPath)
		if !ok {
			return
		}
		composePage(w, r, member, status, nil, nil, nil, errMsg)
	})))
	mux.Handle("POST /settings/mfa/reset", requireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		member, errMsg, status, ok := mfaHandlers.AdminReset(w, r, settingsPath)
		if !ok {
			return
		}
		composePage(w, r, member, status, nil, nil, nil, errMsg)
	})))
}

// registerKioskPages wires the /kiosk/* touch-first kiosk shell (NES-128).
// GET/POST /kiosk/activate is deliberately public — the kiosk device has no
// identity until that request succeeds (see KioskWebHandlers.Activate: GET
// carries the one-click link's ?code=, POST carries the manual entry form) —
// every other route is gated by RequireKioskOrMember so a browser with
// neither a kiosk cookie nor a member session gets a bare 401 (AC1), never a
// peek at household data.
//
// Every tab route (chores/calendar/meals/shopping/photos) has a matching
// GET /kiosk/{tab}/content route (NES-130): the tab's own content fragment
// self-polls that endpoint every 15s (kioskadapter.kioskContentPollInterval)
// so a change made elsewhere — a chore claimed from the QR deep-link flow,
// or any other household data change — shows up on an untouched display
// without manual interaction. Since RequireKioskOrMember still gates these
// like every other kiosk route, a revoked device's poll gets a bare 401;
// htmx does not swap a non-2xx response by default, so a failed poll simply
// leaves the fragment as it was and the next interval retries.
func registerKioskPages(mux *http.ServeMux, kioskHandlers *kioskadapter.KioskWebHandlers) {
	mux.HandleFunc("GET /kiosk/activate", kioskHandlers.Activate)
	mux.HandleFunc("POST /kiosk/activate", kioskHandlers.Activate)

	requireKioskOrMember := kioskadapter.RequireKioskOrMember()
	mux.Handle("GET /kiosk/{$}", requireKioskOrMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/kiosk/chores", http.StatusSeeOther)
	})))
	mux.Handle("GET /kiosk/chores", requireKioskOrMember(http.HandlerFunc(kioskHandlers.Chores)))
	mux.Handle("GET /kiosk/chores/content", requireKioskOrMember(http.HandlerFunc(kioskHandlers.ChoresContent)))
	mux.Handle("GET /kiosk/calendar", requireKioskOrMember(http.HandlerFunc(kioskHandlers.Calendar)))
	mux.Handle("GET /kiosk/calendar/content", requireKioskOrMember(http.HandlerFunc(kioskHandlers.CalendarContent)))
	mux.Handle("GET /kiosk/meals", requireKioskOrMember(http.HandlerFunc(kioskHandlers.Meals)))
	mux.Handle("GET /kiosk/meals/content", requireKioskOrMember(http.HandlerFunc(kioskHandlers.MealsContent)))
	mux.Handle("GET /kiosk/shopping", requireKioskOrMember(http.HandlerFunc(kioskHandlers.Shopping)))
	mux.Handle("GET /kiosk/shopping/content", requireKioskOrMember(http.HandlerFunc(kioskHandlers.ShoppingContent)))
	// POST /kiosk/shopping/{id}/in-cart is the one member-free mutation the
	// kiosk exposes (AC5): marking a needed item in-cart.
	mux.Handle("POST /kiosk/shopping/{id}/in-cart", requireKioskOrMember(http.HandlerFunc(kioskHandlers.MarkInCart)))
	mux.Handle("GET /kiosk/photos", requireKioskOrMember(http.HandlerFunc(kioskHandlers.Photos)))
	mux.Handle("GET /kiosk/photos/content", requireKioskOrMember(http.HandlerFunc(kioskHandlers.PhotosContent)))
	mux.Handle("GET /kiosk/photos/{id}/raw", requireKioskOrMember(http.HandlerFunc(kioskHandlers.Raw)))
}

// registerDeepLinkPages wires the /go/{action}/{id} kiosk QR deep links
// (NES-129). Every route — including the id-less /go/add-chore — requires a
// MEMBER session (never a kiosk device identity): the whole point of a deep
// link is to bridge the wall-mounted kiosk to the scanning member's OWN
// phone, so the kiosk device itself must never be able to satisfy this gate.
// RequireMember's existing redirect-to-/login?next=... behavior is exactly
// the login-continuation flow NES-129 needs — no additional code is required
// for it here; see internal/auth/adapter's sanitizeNext.
func registerDeepLinkPages(mux *http.ServeMux, sm *scs.SessionManager, deepLinkHandlers *deeplinkadapter.WebHandlers) {
	requireMember := authadapter.RequireMember(sm)

	mux.Handle("GET /go/add-chore", requireMember(http.HandlerFunc(deepLinkHandlers.ShowAddChore)))
	mux.Handle("POST /go/add-chore", requireMember(http.HandlerFunc(deepLinkHandlers.ConfirmAddChore)))
	// GET /go/{action}/done is the PRG (Post-Redirect-Get) landing page every
	// successful confirm POST redirects to (NES-129) — registered before the
	// id-shaped pattern only for readability; net/http's ServeMux resolves
	// the literal "done" path segment to this handler regardless of
	// registration order (a literal is more specific than a wildcard at the
	// same position — see WebHandlers.Done's doc comment).
	mux.Handle("GET /go/{action}/done", requireMember(http.HandlerFunc(deepLinkHandlers.Done)))
	mux.Handle("GET /go/{action}/{id}", requireMember(http.HandlerFunc(deepLinkHandlers.Show)))
	mux.Handle("POST /go/{action}/{id}", requireMember(http.HandlerFunc(deepLinkHandlers.Confirm)))
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
