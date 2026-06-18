package adapter

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// listWindowDays is how many calendar days ahead of today pending task
// instances are shown. Matches the scheduler's generation horizon so every
// materialised pending instance falls within this window.
const listWindowDays = 14

// LayoutFunc is the callback type home.go passes to List so the handler can
// wrap its content in the full A·Hearth app shell. It mirrors the pattern used
// by the dashboard handler: build ShellProps + nav, return a templ layout func.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// WebHandlers holds the HTTP handler methods for the tasks read-side UI and the
// three mutation actions (complete, skip, claim). All dependencies are injected
// via the constructor so this type is easily testable with fakes.
//
// The household repository is consumed read-only, purely for display: the list
// is grouped by assignee, so each visible member's display name and color must
// be resolved. Mutations remain tenant-scoped through the TaskService.
type WebHandlers struct {
	svc          *app.TaskService
	taskRepo     domain.RecurringTaskRepository
	instanceRepo domain.TaskInstanceRepository
	households   household.HouseholdRepository
	sm           *scs.SessionManager
	logger       *slog.Logger
}

// NewWebHandlers constructs a WebHandlers with all required dependencies. It
// panics when any dependency is nil so misconfigured composition roots are
// caught at startup rather than at the first HTTP request.
func NewWebHandlers(
	svc *app.TaskService,
	taskRepo domain.RecurringTaskRepository,
	instanceRepo domain.TaskInstanceRepository,
	households household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *WebHandlers {
	if svc == nil {
		panic("tasks/adapter: NewWebHandlers requires a non-nil TaskService")
	}
	if taskRepo == nil {
		panic("tasks/adapter: NewWebHandlers requires a non-nil RecurringTaskRepository")
	}
	if instanceRepo == nil {
		panic("tasks/adapter: NewWebHandlers requires a non-nil TaskInstanceRepository")
	}
	if households == nil {
		panic("tasks/adapter: NewWebHandlers requires a non-nil HouseholdRepository")
	}
	if sm == nil {
		panic("tasks/adapter: NewWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("tasks/adapter: NewWebHandlers requires a non-nil logger")
	}
	return &WebHandlers{
		svc:          svc,
		taskRepo:     taskRepo,
		instanceRepo: instanceRepo,
		households:   households,
		sm:           sm,
		logger:       logger,
	}
}

// List handles GET /tasks. It loads pending instances for the next 14 days and
// overdue instances from the past 90 days, joins them with their parent
// recurring task titles and their assignee member display name/color via two
// in-memory maps (one ListActive call + one ListMembers call, O(1) extra DB
// round trips regardless of instance count), groups them by assignee member,
// and renders TasksPage into the app shell.
//
// The layout callback is supplied by the caller (home.go) so this handler stays
// decoupled from the ShellProps / nav construction that depends on the
// household repository.
func (h *WebHandlers) List(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		rows, err := h.buildTaskRows(r, member)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "tasks: build task rows", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		groups := groupTaskRows(rows)
		content := components.TasksPage(groups)

		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "tasks: render page", "error", err)
		}
	}
}

// Complete handles POST /tasks/{id}/complete. It verifies the CSRF token,
// parses the instance id from the path, calls CompleteInstance, and on success
// redirects (full navigation) or sends an HX-Redirect header (HTMX).
//
// Error mapping:
//   - bad CSRF                   → 403
//   - malformed id               → 400
//   - ErrInstanceNotFound        → 404
//   - ErrInstanceInTerminalState → 409
//   - other                      → 500
func (h *WebHandlers) Complete(w http.ResponseWriter, r *http.Request) {
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

	id, ok := h.parseInstanceID(w, r)
	if !ok {
		return
	}

	if err := h.svc.CompleteInstance(r.Context(), member.HouseholdID, id, member.ID, time.Now()); err != nil {
		h.handleMutationError(w, r, err)
		return
	}

	respondAfterMutation(w, r)
}

// Skip handles POST /tasks/{id}/skip. It verifies the CSRF token, parses the
// instance id, calls SkipInstance, and responds with a redirect or HX-Redirect.
//
// Error mapping: same as Complete.
func (h *WebHandlers) Skip(w http.ResponseWriter, r *http.Request) {
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

	id, ok := h.parseInstanceID(w, r)
	if !ok {
		return
	}

	if err := h.svc.SkipInstance(r.Context(), member.HouseholdID, id); err != nil {
		h.handleMutationError(w, r, err)
		return
	}

	respondAfterMutation(w, r)
}

// Claim handles POST /tasks/{id}/claim. It verifies the CSRF token, parses the
// instance id, calls ClaimInstance with the current member as assignee, and
// responds with a redirect or HX-Redirect.
//
// Error mapping:
//   - bad CSRF                   → 403
//   - malformed id               → 400
//   - ErrInstanceNotFound        → 404
//   - ErrInstanceInTerminalState → 409
//   - ErrInstanceAlreadyClaimed  → 409
//   - other                      → 500
func (h *WebHandlers) Claim(w http.ResponseWriter, r *http.Request) {
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

	id, ok := h.parseInstanceID(w, r)
	if !ok {
		return
	}

	if err := h.svc.ClaimInstance(r.Context(), member.HouseholdID, id, member.ID); err != nil {
		h.handleMutationError(w, r, err)
		return
	}

	respondAfterMutation(w, r)
}

// upForGrabsLabel is the heading for the group of unassigned, claimable rows.
const upForGrabsLabel = "Up for grabs"

// buildTaskRows fetches pending and overdue instances for the member's
// household, joins each with its parent recurring task's title and category and
// with its assignee member's display name and color, and returns a flat slice
// of TaskRow view models sorted by due date then title.
//
// N+1 avoidance: exactly one ListActive call (recurring task metadata) and one
// ListMembers call (member display name/color) are made up front, each loaded
// into an in-memory map keyed by id; every instance is then joined in memory.
func (h *WebHandlers) buildTaskRows(r *http.Request, member *household.Member) ([]components.TaskRow, error) {
	csrfToken := authadapter.GetCSRFToken(r.Context(), h.sm)

	// today is the reference calendar date in UTC. All window bounds and the
	// relative due-date labels are derived from it (never from local time), so the
	// day boundary does not drift with the server's timezone.
	today := domain.DateOf(time.Now())

	// Single query for all active recurring task metadata; keyed by ID for O(1)
	// join with each instance below.
	activeTasks, err := h.taskRepo.ListActive(r.Context(), member.HouseholdID)
	if err != nil {
		return nil, err
	}
	taskMeta := make(map[domain.RecurringTaskID]*domain.RecurringTask, len(activeTasks))
	for _, t := range activeTasks {
		taskMeta[t.ID] = t
	}

	// Single query for the household members; keyed by ID for O(1) assignee
	// name/color resolution.
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return nil, err
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}

	// Two windows, both expressed as UTC calendar dates:
	//   - PENDING: [today, today+listWindowDays] — upcoming instances within the
	//     scheduler's generation horizon.
	//   - OVERDUE: [epoch, today] — every overdue instance, with no effective
	//     lower bound, because an overdue chore stays actionable indefinitely and
	//     must never be hidden by an arbitrary cutoff.
	pending, err := h.instanceRepo.ListByHousehold(
		r.Context(), member.HouseholdID, domain.StatusPending,
		today, today.AddDate(0, 0, listWindowDays),
	)
	if err != nil {
		return nil, err
	}

	overdue, err := h.instanceRepo.ListByHousehold(
		r.Context(), member.HouseholdID, domain.StatusOverdue,
		domain.DateOf(time.Time{}), today,
	)
	if err != nil {
		return nil, err
	}

	combined := make([]*domain.TaskInstance, 0, len(pending)+len(overdue))
	combined = append(combined, pending...)
	combined = append(combined, overdue...)

	rows := make([]components.TaskRow, 0, len(combined))
	for _, inst := range combined {
		meta, found := taskMeta[inst.RecurringTaskID]
		var title, category string
		if found {
			title = meta.Title
			category = meta.Category.String()
		} else {
			// Parent recurring task is inactive (archived); still show the instance
			// so it can be acted on.
			title = "(archived)"
			category = "chore"
		}

		var assigneeID, assigneeName, assigneeColor string
		if inst.AssigneeID != nil {
			assigneeID = inst.AssigneeID.String()
			if m, ok := memberByID[*inst.AssigneeID]; ok {
				assigneeName = m.DisplayName
				assigneeColor = m.Color.String()
			} else {
				// The composite tenant FK + ON DELETE SET NULL should keep this
				// from happening (a deleted member clears assignee_id), so a
				// non-resolvable assignee signals a referential-integrity anomaly.
				h.logger.WarnContext(r.Context(), "tasks: assignee id has no matching member",
					"instance_id", inst.ID.String(), "assignee_id", assigneeID)
			}
		}

		// An instance is claimable when it is unassigned and still actionable.
		// As of NES-32 both pending and overdue are actionable, so an unassigned
		// overdue instance can also be claimed.
		actionable := inst.Status == domain.StatusPending || inst.Status == domain.StatusOverdue
		claimable := actionable && inst.AssigneeID == nil

		rows = append(rows, components.TaskRow{
			InstanceID:    inst.ID.String(),
			Title:         title,
			Category:      category,
			DueOn:         inst.DueOn,
			DueLabel:      dueLabel(inst.DueOn, today),
			Status:        inst.Status.String(),
			AssigneeID:    assigneeID,
			AssigneeName:  assigneeName,
			AssigneeColor: assigneeColor,
			Claimable:     claimable,
			CSRFToken:     csrfToken,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].DueOn.Equal(rows[j].DueOn) {
			return rows[i].Title < rows[j].Title
		}
		return rows[i].DueOn.Before(rows[j].DueOn)
	})
	return rows, nil
}

// dueLabel renders a due date relative to the supplied reference date today
// (both treated as UTC calendar dates): the due day shows "Today", the next day
// "Tomorrow", and any other date the short month-day form ("Jun 20"). Passing
// today explicitly keeps the computation deterministic and free of time.Now().
func dueLabel(due, today time.Time) string {
	d := domain.DateOf(due)
	t := domain.DateOf(today)
	switch {
	case d.Equal(t):
		return "Today"
	case d.Equal(t.AddDate(0, 0, 1)):
		return "Tomorrow"
	default:
		return d.Format("Jan 2")
	}
}

// memberGroup is an intermediate accumulator keyed by member id while grouping.
// It captures the display label and color once (from the first row seen for that
// member) plus the rows assigned to that member.
type memberGroup struct {
	id    string
	label string
	color string
	rows  []components.TaskRow
}

// groupTaskRows arranges a flat slice of TaskRow into per-member TaskGroup
// slices. Rows are grouped by the assignee's stable id (NOT display name, which
// is not unique within a household — two members named "Sam" must form two
// groups). Each group is labelled with the assignee's display name and tinted
// with their color. Member groups are ordered deterministically by display name,
// then by id as a tiebreaker. All unassigned claimable rows are collected into a
// final "Up for grabs" group.
//
// Rows within a group preserve the incoming order (sorted by due date then title
// in buildTaskRows).
func groupTaskRows(rows []components.TaskRow) []components.TaskGroup {
	byMemberID := make(map[string]*memberGroup)
	order := make([]*memberGroup, 0)
	var claimable []components.TaskRow

	for _, row := range rows {
		if row.Claimable {
			claimable = append(claimable, row)
			continue
		}
		// A non-claimable row is assigned. Group by the stable assignee id. The
		// label is the display name; when the id could not be resolved to a member
		// (should not happen given the assignee FK), label it clearly rather than
		// dropping the row.
		key := row.AssigneeID
		label := row.AssigneeName
		if label == "" {
			label = "(unknown member)"
		}
		g, ok := byMemberID[key]
		if !ok {
			g = &memberGroup{id: key, label: label, color: row.AssigneeColor}
			byMemberID[key] = g
			order = append(order, g)
		}
		g.rows = append(g.rows, row)
	}

	// Deterministic ordering: by display label, then by id as a tiebreaker so two
	// members sharing a display name have a stable, repeatable order.
	sort.Slice(order, func(i, j int) bool {
		if order[i].label != order[j].label {
			return order[i].label < order[j].label
		}
		return order[i].id < order[j].id
	})

	groups := make([]components.TaskGroup, 0, len(order)+1)
	for _, g := range order {
		groups = append(groups, components.TaskGroup{
			Label:         g.label,
			AssigneeColor: g.color,
			Rows:          g.rows,
		})
	}
	if len(claimable) > 0 {
		groups = append(groups, components.TaskGroup{
			Label: upForGrabsLabel,
			Rows:  claimable,
		})
	}
	return groups
}

// parseInstanceID extracts and parses the {id} path value from r. It writes a
// 400 response and returns false on any parse failure.
func (h *WebHandlers) parseInstanceID(w http.ResponseWriter, r *http.Request) (domain.TaskInstanceID, bool) {
	raw := r.PathValue("id")
	id, err := domain.ParseTaskInstanceID(raw)
	if err != nil {
		http.Error(w, "invalid instance id", http.StatusBadRequest)
		return domain.TaskInstanceID{}, false
	}
	return id, true
}

// handleMutationError maps domain errors to HTTP status codes and writes a
// plain-text error response. The caller must return immediately after this.
func (h *WebHandlers) handleMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInstanceNotFound):
		http.Error(w, "task not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrInstanceInTerminalState),
		errors.Is(err, domain.ErrInstanceAlreadyClaimed):
		http.Error(w, "task already acted on", http.StatusConflict)
	default:
		h.logger.ErrorContext(r.Context(), "tasks: mutation error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// respondAfterMutation responds after a successful complete/skip/claim. HTMX
// requests receive an HX-Redirect so the full list page refreshes and reflects
// the new state. Full navigations receive a 303 redirect to /tasks.
func respondAfterMutation(w http.ResponseWriter, r *http.Request) {
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", "/tasks")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}
