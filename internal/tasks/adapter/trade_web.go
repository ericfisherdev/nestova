package adapter

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// taskGroupsBuilder is the minimal capability TradeWebHandlers needs from the
// tasks list handler: rebuilding the #task-groups fragment's groups after an
// accept swaps two instances' assignees (NES-122). Defined here — narrower
// than WebHandlers' full read/write surface — so this cross-handler-type
// dependency stays minimal (ISP); *WebHandlers satisfies it via
// WebHandlers.BuildGroups.
type taskGroupsBuilder interface {
	BuildGroups(r *http.Request, member *household.Member) ([]components.TaskGroup, error)
}

// TradeWebHandlers holds the HTTP handler methods for the chore-trade UI
// (NES-122): the propose-trade picker, the propose/accept/decline/cancel
// mutation actions, and the parent-only trade history page. Mutations that
// need notifications (Propose, Accept, Decline) go through svc; every other
// method reads its repositories directly, mirroring WebHandlers' own
// split between h.svc (mutations) and h.taskRepo/h.instanceRepo (reads).
type TradeWebHandlers struct {
	svc           *app.TradeService
	tradeRepo     domain.ChoreTradeRepository
	instanceRepo  domain.TaskInstanceRepository
	taskRepo      domain.RecurringTaskRepository
	households    household.HouseholdRepository
	groupsBuilder taskGroupsBuilder
	sm            *scs.SessionManager
	logger        *slog.Logger
}

// NewTradeWebHandlers constructs a TradeWebHandlers with all required
// dependencies. It panics when any dependency is nil so misconfigured
// composition roots are caught at startup rather than at the first HTTP
// request, matching WebHandlers' and GamificationWebHandlers' precedent.
func NewTradeWebHandlers(
	svc *app.TradeService,
	tradeRepo domain.ChoreTradeRepository,
	instanceRepo domain.TaskInstanceRepository,
	taskRepo domain.RecurringTaskRepository,
	households household.HouseholdRepository,
	groupsBuilder taskGroupsBuilder,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *TradeWebHandlers {
	if svc == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil TradeService")
	}
	if tradeRepo == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil ChoreTradeRepository")
	}
	if instanceRepo == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil TaskInstanceRepository")
	}
	if taskRepo == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil RecurringTaskRepository")
	}
	if households == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil HouseholdRepository")
	}
	if groupsBuilder == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil taskGroupsBuilder")
	}
	if sm == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("tasks/adapter: NewTradeWebHandlers requires a non-nil logger")
	}
	return &TradeWebHandlers{
		svc:           svc,
		tradeRepo:     tradeRepo,
		instanceRepo:  instanceRepo,
		taskRepo:      taskRepo,
		households:    households,
		groupsBuilder: groupsBuilder,
		sm:            sm,
		logger:        logger,
	}
}

// isParent reports whether member has a parent role (owner or adult) —
// NES-122's role gate for the trade-history page and the dashboard's "View
// trade history" link. A child member never sees or can access history.
func isParent(member *household.Member) bool {
	return member.Role == household.RoleOwner || member.Role == household.RoleAdult
}

// ---------------------------------------------------------------------------
// Propose-trade picker
// ---------------------------------------------------------------------------

// ProposePickerPage handles GET /tasks/{id}/propose-trade. It loads the
// offered instance (must belong to and be currently assigned to the
// requesting member, and satisfy domain.IsInstanceTradeable), lists other
// members' tradeable chores as candidates, and renders the picker.
//
// Error mapping:
//   - ErrInstanceNotFound   → 404
//   - ErrNotYourChore       → 403
//   - ErrInstanceNotTradeable → 409
//   - other                 → 500
func (h *TradeWebHandlers) ProposePickerPage(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		offeredID, err := domain.ParseTaskInstanceID(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid instance id", http.StatusBadRequest)
			return
		}

		form, err := h.buildProposeTradeForm(r, member, offeredID, "")
		if err != nil {
			h.handlePickerBuildError(w, r, err)
			return
		}

		content := components.ProposeTradePage(form)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "propose trade: render picker", "error", err)
		}
	}
}

// handlePickerBuildError maps a buildProposeTradeForm error to an HTTP
// response and writes it. The caller must return immediately after this.
func (h *TradeWebHandlers) handlePickerBuildError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInstanceNotFound):
		http.Error(w, "task not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrNotYourChore):
		http.Error(w, "that chore is not assigned to you", http.StatusForbidden)
	case errors.Is(err, domain.ErrInstanceNotTradeable):
		http.Error(w, "that chore is not tradeable", http.StatusConflict)
	default:
		h.logger.ErrorContext(r.Context(), "propose trade: build picker", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// buildProposeTradeForm loads offeredID (tenant- and ownership-checked
// against member, and re-validated against domain.IsInstanceTradeable — the
// same rule ChoreTradeRepository.Propose enforces, checked here only to give
// a friendlier error before the picker renders) and every other member's
// tradeable chore, joined with titles/points (via one ListActive call) and
// assignee display names (via one ListMembers call), mirroring
// buildTaskRows' N+1-avoidance shape.
func (h *TradeWebHandlers) buildProposeTradeForm(
	r *http.Request,
	member *household.Member,
	offeredID domain.TaskInstanceID,
	errMsg string,
) (components.ProposeTradeForm, error) {
	ctx := r.Context()

	offered, err := h.instanceRepo.Get(ctx, member.HouseholdID, offeredID)
	if err != nil {
		return components.ProposeTradeForm{}, err
	}
	if offered.AssigneeID == nil || *offered.AssigneeID != member.ID {
		return components.ProposeTradeForm{}, domain.ErrNotYourChore
	}
	if !domain.IsInstanceTradeable(offered) {
		return components.ProposeTradeForm{}, domain.ErrInstanceNotTradeable
	}

	candidates, err := h.instanceRepo.ListTradeableAssignedToOthers(ctx, member.HouseholdID, member.ID)
	if err != nil {
		return components.ProposeTradeForm{}, err
	}

	activeTasks, err := h.taskRepo.ListActive(ctx, member.HouseholdID)
	if err != nil {
		return components.ProposeTradeForm{}, err
	}
	taskMeta := make(map[domain.RecurringTaskID]*domain.RecurringTask, len(activeTasks))
	for _, t := range activeTasks {
		taskMeta[t.ID] = t
	}

	members, err := h.households.ListMembers(ctx, member.HouseholdID)
	if err != nil {
		return components.ProposeTradeForm{}, err
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}

	candidateOpts := make([]components.TradeCandidate, 0, len(candidates))
	for _, c := range candidates {
		title, points := "(archived)", 0
		if meta, ok := taskMeta[c.RecurringTaskID]; ok {
			title, points = meta.Title, meta.Points
		}
		assigneeName := ""
		if c.AssigneeID != nil {
			if m, ok := memberByID[*c.AssigneeID]; ok {
				assigneeName = m.DisplayName
			}
		}
		candidateOpts = append(candidateOpts, components.TradeCandidate{
			InstanceID:   c.ID.String(),
			Title:        title,
			Points:       points,
			AssigneeName: assigneeName,
		})
	}

	offeredTitle, offeredPoints := "(archived)", 0
	if meta, ok := taskMeta[offered.RecurringTaskID]; ok {
		offeredTitle, offeredPoints = meta.Title, meta.Points
	}

	return components.ProposeTradeForm{
		OfferedInstanceID: offeredID.String(),
		OfferedTitle:      offeredTitle,
		OfferedPoints:     offeredPoints,
		Candidates:        candidateOpts,
		CSRFToken:         authadapter.GetCSRFToken(ctx, h.sm),
		Error:             errMsg,
	}, nil
}

// ProposeTrade handles POST /trades. It verifies the CSRF token, resolves
// the responder server-side from the requested instance's CURRENT
// assignee (never trusting a client-submitted responder id), and delegates
// to TradeService.Propose. On success it redirects to the dashboard, where
// the responder's card (or, once accepted, the outcome) becomes visible.
//
// Error mapping (re-renders the picker at the given status rather than a
// bare error page, so the member can immediately retry):
//   - bad CSRF                    → 403
//   - malformed offered/requested id → 400
//   - requested instance unassigned → 422 (re-render)
//   - ErrTradeSelf                → 400 (re-render)
//   - ErrNotYourChore             → 403 (re-render)
//   - ErrInstanceNotTradeable     → 409 (re-render)
//   - ErrInstanceNotFound         → 404 (re-render)
//   - other                       → 500
func (h *TradeWebHandlers) ProposeTrade(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !authadapter.VerifyCSRF(r, h.sm) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		offeredID, err := domain.ParseTaskInstanceID(r.FormValue("offered_instance_id"))
		if err != nil {
			http.Error(w, "invalid offered instance id", http.StatusBadRequest)
			return
		}

		requestedID, err := domain.ParseTaskInstanceID(r.FormValue("requested_instance_id"))
		if err != nil {
			h.rerenderProposeForm(w, r, member, offeredID, "Please select a chore to trade for.", http.StatusUnprocessableEntity, layoutFn)
			return
		}

		requested, err := h.instanceRepo.Get(r.Context(), member.HouseholdID, requestedID)
		if err != nil {
			h.handleProposeError(w, r, member, offeredID, err, layoutFn)
			return
		}
		if requested.AssigneeID == nil {
			h.rerenderProposeForm(w, r, member, offeredID, "That chore is no longer assigned to anyone.", http.StatusConflict, layoutFn)
			return
		}
		responderID := *requested.AssigneeID

		if _, err := h.svc.Propose(r.Context(), member.HouseholdID, member.ID, responderID, offeredID, requestedID); err != nil {
			h.handleProposeError(w, r, member, offeredID, err, layoutFn)
			return
		}

		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// handleProposeError maps a TradeService.Propose error to a user-facing
// message and status, then re-renders the picker. The caller must return
// immediately after this.
func (h *TradeWebHandlers) handleProposeError(
	w http.ResponseWriter,
	r *http.Request,
	member *household.Member,
	offeredID domain.TaskInstanceID,
	err error,
	layoutFn LayoutFunc,
) {
	var msg string
	var status int
	switch {
	case errors.Is(err, domain.ErrTradeSelf):
		msg, status = "You can't propose a trade with yourself.", http.StatusBadRequest
	case errors.Is(err, domain.ErrNotYourChore):
		msg, status = "One of the selected chores is no longer assigned as expected.", http.StatusForbidden
	case errors.Is(err, domain.ErrInstanceNotTradeable):
		msg, status = "One of the selected chores is no longer tradeable.", http.StatusConflict
	case errors.Is(err, domain.ErrInstanceNotFound):
		msg, status = "One of the selected chores could not be found.", http.StatusNotFound
	default:
		h.logger.ErrorContext(r.Context(), "propose trade: service error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.rerenderProposeForm(w, r, member, offeredID, msg, status, layoutFn)
}

// rerenderProposeForm rebuilds and re-renders the propose-trade picker at
// status with msg shown as a validation error, mirroring
// WebHandlers.renderNewTaskForm's re-render-on-failure precedent.
//
// The rebuild itself can fail with the SAME kind of domain error that
// caused this re-render in the first place — e.g. the offered instance was
// reassigned or completed between the original Propose/validation failure
// and this rebuild, so buildProposeTradeForm's own ownership/tradeability
// checks now also fail. That failure is routed through
// handlePickerBuildError (the same mapping ProposePickerPage uses) rather
// than an unconditional 500, so a "known" cause still surfaces its correct
// mapped status (403/409/404) instead of collapsing to a misleading
// "internal server error".
func (h *TradeWebHandlers) rerenderProposeForm(
	w http.ResponseWriter,
	r *http.Request,
	member *household.Member,
	offeredID domain.TaskInstanceID,
	msg string,
	status int,
	layoutFn LayoutFunc,
) {
	form, err := h.buildProposeTradeForm(r, member, offeredID, msg)
	if err != nil {
		h.handlePickerBuildError(w, r, err)
		return
	}
	content := components.ProposeTradePage(form)
	if err := render.Render(r.Context(), w, status, layoutFn(member)(content)); err != nil {
		h.logger.ErrorContext(r.Context(), "propose trade: render picker error", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Accept / Decline / Cancel
// ---------------------------------------------------------------------------

// Accept handles POST /trades/{id}/accept. On success it responds with the
// resolved card's removal plus an out-of-band #task-groups refresh for HTMX
// requests (NES-122; see components.TradeAcceptResponse), since Accept
// swaps both traded instances' assignees.
//
// Error mapping:
//   - bad CSRF              → 403
//   - malformed id          → 400
//   - ErrTradeNotPending    → 409
//   - ErrInstanceNotTradeable → 409
//   - other                 → 500
func (h *TradeWebHandlers) Accept(w http.ResponseWriter, r *http.Request) {
	member, id, ok := h.beginTradeMutation(w, r)
	if !ok {
		return
	}

	if err := h.svc.Accept(r.Context(), member.HouseholdID, id, member.ID, time.Now()); err != nil {
		h.handleTradeMutationError(w, r, err)
		return
	}

	if !render.IsHTMX(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	groups, err := h.groupsBuilder.BuildGroups(r, member)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "trades: build groups after accept", "error", err)
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := render.Render(r.Context(), w, http.StatusOK, components.TradeAcceptResponse(id.String(), groups)); err != nil {
		h.logger.ErrorContext(r.Context(), "trades: render accept response", "error", err)
	}
}

// Decline handles POST /trades/{id}/decline. No instance assignment changes,
// so the HTMX response only removes the resolved card in place.
//
// Error mapping: same as Accept, minus ErrInstanceNotTradeable (Decline
// never re-validates instance state).
func (h *TradeWebHandlers) Decline(w http.ResponseWriter, r *http.Request) {
	member, id, ok := h.beginTradeMutation(w, r)
	if !ok {
		return
	}

	if err := h.svc.Decline(r.Context(), member.HouseholdID, id, member.ID); err != nil {
		h.handleTradeMutationError(w, r, err)
		return
	}

	h.respondCardRemoved(w, r, id)
}

// Cancel handles POST /trades/{id}/cancel. Only the trade's proposer may
// cancel a still-pending proposal. No instance assignment changes.
//
// Error mapping: same as Decline.
func (h *TradeWebHandlers) Cancel(w http.ResponseWriter, r *http.Request) {
	member, id, ok := h.beginTradeMutation(w, r)
	if !ok {
		return
	}

	if err := h.svc.Cancel(r.Context(), member.HouseholdID, id, member.ID); err != nil {
		h.handleTradeMutationError(w, r, err)
		return
	}

	h.respondCardRemoved(w, r, id)
}

// beginTradeMutation runs the CSRF/session/path-id boilerplate shared by
// Accept, Decline, and Cancel, mirroring WebHandlers.Complete/Skip/Claim's
// shape. ok is false when a response has already been written and the
// caller must return immediately.
func (h *TradeWebHandlers) beginTradeMutation(w http.ResponseWriter, r *http.Request) (*household.Member, domain.ChoreTradeID, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, domain.ChoreTradeID{}, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, domain.ChoreTradeID{}, false
	}
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, domain.ChoreTradeID{}, false
	}
	id, err := domain.ParseChoreTradeID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid trade id", http.StatusBadRequest)
		return nil, domain.ChoreTradeID{}, false
	}
	return member, id, true
}

// handleTradeMutationError maps domain errors to HTTP status codes and
// writes a plain-text error response. The caller must return immediately
// after this.
func (h *TradeWebHandlers) handleTradeMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrTradeNotPending):
		http.Error(w, "trade is no longer pending", http.StatusConflict)
	case errors.Is(err, domain.ErrInstanceNotTradeable):
		http.Error(w, "one of the traded chores is no longer tradeable", http.StatusConflict)
	default:
		h.logger.ErrorContext(r.Context(), "trades: mutation error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// respondCardRemoved responds after a successful Decline/Cancel: HTMX
// requests receive the resolved card's removal; full navigations get a 303
// redirect to the dashboard.
func (h *TradeWebHandlers) respondCardRemoved(w http.ResponseWriter, r *http.Request, id domain.ChoreTradeID) {
	if !render.IsHTMX(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := render.Render(r.Context(), w, http.StatusOK, components.TradeCardRemoved(id.String())); err != nil {
		h.logger.ErrorContext(r.Context(), "trades: render card removal", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Dashboard trade sections
// ---------------------------------------------------------------------------

// DashboardSections builds the current member's pending trade cards for the
// dashboard (NES-122): proposals awaiting the member's own decision (they
// are the responder) and their own outgoing pending proposals (cancelable).
// Errors degrade to an empty TradeSections rather than failing the whole
// dashboard render, mirroring dashboardShell's existing degrade-on-error
// precedent for the member list (cmd/server/home.go).
func (h *TradeWebHandlers) DashboardSections(r *http.Request, member *household.Member) components.TradeSections {
	ctx := r.Context()
	sections := components.TradeSections{CanViewHistory: isParent(member)}

	pending, err := h.tradeRepo.ListPendingByMember(ctx, member.HouseholdID, member.ID)
	if err != nil {
		h.logger.ErrorContext(ctx, "trades: list pending for dashboard", "error", err)
		return sections
	}
	if len(pending) == 0 {
		return sections
	}

	members, err := h.households.ListMembers(ctx, member.HouseholdID)
	if err != nil {
		h.logger.ErrorContext(ctx, "trades: list members for dashboard", "error", err)
		return sections
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}

	csrfToken := authadapter.GetCSRFToken(ctx, h.sm)
	for _, summary := range pending {
		card := buildTradeCard(summary, memberByID, csrfToken, member.ID)
		if summary.ResponderID == member.ID {
			sections.AwaitingYou = append(sections.AwaitingYou, card)
		} else {
			sections.AwaitingThem = append(sections.AwaitingThem, card)
		}
	}
	return sections
}

// ---------------------------------------------------------------------------
// Trade history (parent-only)
// ---------------------------------------------------------------------------

// HistoryPage handles GET /trades/history. Access is role-gated to parents
// (owner or adult) — a child member receives 403 (NES-122).
func (h *TradeWebHandlers) HistoryPage(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !isParent(member) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		page, err := h.buildHistoryPage(r, member)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "trade history: build page", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		content := components.TradeHistoryPageComponent(page)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "trade history: render", "error", err)
		}
	}
}

// buildHistoryPage loads the household's most recent trades (capped at
// domain.TradeHistoryLimit) and joins each with its parties' display names —
// titles/points arrive already resolved from ListHistory's own joined query
// (NES-122).
func (h *TradeWebHandlers) buildHistoryPage(r *http.Request, member *household.Member) (components.TradeHistoryPage, error) {
	ctx := r.Context()

	trades, err := h.tradeRepo.ListHistory(ctx, member.HouseholdID)
	if err != nil {
		return components.TradeHistoryPage{}, err
	}
	if len(trades) == 0 {
		return components.TradeHistoryPage{}, nil
	}

	members, err := h.households.ListMembers(ctx, member.HouseholdID)
	if err != nil {
		return components.TradeHistoryPage{}, err
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}

	cards := make([]components.TradeCard, 0, len(trades))
	for _, summary := range trades {
		cards = append(cards, buildTradeCard(summary, memberByID, "", member.ID))
	}
	return components.TradeHistoryPage{Trades: cards}, nil
}

// ---------------------------------------------------------------------------
// Shared view-model joins
// ---------------------------------------------------------------------------

// buildTradeCard maps a single domain.TradeSummary — already pre-joined with
// both chores' titles/points by ListPendingByMember/ListHistory (NES-122) —
// to its TradeCard view model, resolving only the two parties' display
// names from the caller's already-loaded memberByID map. viewerID
// determines IsResponder/IsProposer, which the template uses to decide
// which actions (if any) to render. A free function (not a *TradeWebHandlers
// method): it touches no repository, so it needs no receiver.
func buildTradeCard(
	summary domain.TradeSummary,
	memberByID map[household.MemberID]*household.Member,
	csrfToken string,
	viewerID household.MemberID,
) components.TradeCard {
	proposerName, responderName := "(unknown member)", "(unknown member)"
	if m, ok := memberByID[summary.ProposerID]; ok {
		proposerName = m.DisplayName
	}
	if m, ok := memberByID[summary.ResponderID]; ok {
		responderName = m.DisplayName
	}

	return components.TradeCard{
		TradeID:       summary.TradeID.String(),
		ProposerName:  proposerName,
		ResponderName: responderName,
		Offered:       components.TradeChoreSummary{Title: summary.OfferedTitle, Points: summary.OfferedPoints},
		Requested:     components.TradeChoreSummary{Title: summary.RequestedTitle, Points: summary.RequestedPoints},
		Status:        summary.Status.String(),
		CreatedAt:     summary.CreatedAt.Format("Jan 2"),
		ResolvedAt:    formatResolvedAt(summary.ResolvedAt),
		IsResponder:   summary.ResponderID == viewerID,
		IsProposer:    summary.ProposerID == viewerID,
		CSRFToken:     csrfToken,
	}
}

// formatResolvedAt pre-formats a trade's ResolvedAt for display, or returns
// empty for a still-live trade — the template never formats a time itself.
func formatResolvedAt(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("Jan 2")
}
