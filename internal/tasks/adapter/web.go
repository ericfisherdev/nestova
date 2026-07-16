package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
// returns the updated row for an in-place HTMX swap (or redirects to /tasks for
// a full navigation) via respondAfterTaskMutation.
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

	h.respondAfterTaskMutation(w, r, member, id)
}

// Skip handles POST /tasks/{id}/skip. It verifies the CSRF token, parses the
// instance id, calls SkipInstance, and returns the updated row for an in-place
// HTMX swap (or redirects to /tasks for a full navigation).
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

	h.respondAfterTaskMutation(w, r, member, id)
}

// Claim handles POST /tasks/{id}/claim. It verifies the CSRF token, parses the
// instance id, calls ClaimInstance with the current member as assignee, and
// returns the updated (now-assigned) row for an in-place HTMX swap, or redirects
// to /tasks for a full navigation.
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

	h.respondAfterTaskMutation(w, r, member, id)
}

// NewTaskPage handles GET /tasks/new. It loads the household member list for the
// rotation-pool picker and renders the create-recurring-task form in the app
// shell with a fresh CSRF token.
//
// The layout callback is supplied by the caller (home.go) so this handler stays
// decoupled from ShellProps / nav construction.
func (h *WebHandlers) NewTaskPage(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "new task page: list members", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		form := components.NewTaskForm{
			CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm),
			Members:   toMemberOptions(members),
		}
		content := components.NewTaskPage(form)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "new task page: render", "error", err)
		}
	}
}

// CreateTask handles POST /tasks. It parses and validates the form, builds a
// RecurringTask and a rotation pool, delegates to TaskService.CreateRecurringTask,
// and on success redirects to /tasks. On validation failure it re-renders the
// form at HTTP 422 with the submitted values preserved and an error message.
//
// Error mapping:
//   - bad CSRF                       → 403
//   - missing/invalid title/category → 422 (form re-render)
//   - ErrInvalidCadence              → 422 (form re-render)
//   - ErrNoRotationMembers           → 422 (form re-render)
//   - other                          → 500
func (h *WebHandlers) CreateTask(layoutFn LayoutFunc) http.HandlerFunc {
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

		task, pool, form, validationMsg := h.parseCreateForm(r, member.HouseholdID)
		if validationMsg != "" {
			// Reload member list for the pool picker on re-render.
			members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
			if err != nil {
				h.logger.ErrorContext(r.Context(), "create task: list members on re-render", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			form.Members = toMemberOptions(members)
			h.renderNewTaskForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
			return
		}

		// Defense-in-depth: verify every selected pool member belongs to this
		// household before touching the database. The composite tenant FK also
		// rejects cross-household members, but an app-level check fails earlier
		// with a friendly message. Claimable tasks ignore the pool, so skip it.
		if task.RotationPolicy != domain.RotationClaimable && len(pool) > 0 {
			members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
			if err != nil {
				h.logger.ErrorContext(r.Context(), "create task: list members for pool validation", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			allowed := make(map[household.MemberID]bool, len(members))
			for _, m := range members {
				allowed[m.ID] = true
			}
			for _, id := range pool {
				if !allowed[id] {
					form.Members = toMemberOptions(members)
					form.Error = "One or more selected members are not in your household."
					h.renderNewTaskForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
					return
				}
			}
		}

		if err := h.svc.CreateRecurringTask(r.Context(), task, pool); err != nil {
			errMsg := createTaskErrMessage(err)
			if errMsg == "" {
				h.logger.ErrorContext(r.Context(), "create task: service error", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			// Service returned a known validation error — re-render with message.
			members, listErr := h.households.ListMembers(r.Context(), member.HouseholdID)
			if listErr != nil {
				h.logger.ErrorContext(r.Context(), "create task: list members on service error", "error", listErr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			form.Members = toMemberOptions(members)
			form.Error = errMsg
			h.renderNewTaskForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
			return
		}

		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
	}
}

// parseCreateForm reads the submitted form values from r, builds a RecurringTask
// and pool, and also returns a sticky NewTaskForm for re-rendering. validationMsg
// is non-empty when any field fails local validation (before the service is
// called). task and pool are only valid when validationMsg is empty.
//
// The householdID is threaded in so it can be stamped onto the task immediately
// here, keeping the handler body lean.
func (h *WebHandlers) parseCreateForm(
	r *http.Request,
	householdID household.HouseholdID,
) (task *domain.RecurringTask, pool []household.MemberID, form components.NewTaskForm, validationMsg string) {
	// Capture sticky values first so the form is always populated for re-renders.
	rawTitle := strings.TrimSpace(r.FormValue("title"))
	rawCategory := r.FormValue("category")
	rawFreq := r.FormValue("freq")
	rawInterval := r.FormValue("interval")
	rawWeekdays := r.Form["byweekday"]
	rawAnchor := r.FormValue("anchor")
	rawPolicy := r.FormValue("rotation_policy")
	rawPoints := r.FormValue("points")
	rawLead := r.FormValue("lead_time_days")
	rawPool := r.Form["pool"]

	form = components.NewTaskForm{
		CSRFToken:         authadapter.GetCSRFToken(r.Context(), h.sm),
		Title:             rawTitle,
		Category:          rawCategory,
		Freq:              rawFreq,
		Interval:          rawInterval,
		Weekdays:          rawWeekdays,
		Anchor:            rawAnchor,
		RotationPolicy:    rawPolicy,
		Points:            rawPoints,
		LeadTimeDays:      rawLead,
		SelectedMemberIDs: rawPool,
	}

	if rawTitle == "" {
		form.Error = "Title is required."
		return nil, nil, form, form.Error
	}

	category, err := domain.ParseCategory(rawCategory)
	if err != nil {
		form.Error = "Please select a valid category."
		return nil, nil, form, form.Error
	}

	freq, err := household.ParseFreq(rawFreq)
	if err != nil {
		form.Error = "Please select a valid frequency."
		return nil, nil, form, form.Error
	}

	// An as-needed cadence has no interval — the form hides the field via
	// Alpine, so don't require a submitted value for it.
	interval := 1
	if freq != household.FreqAsNeeded {
		interval, err = strconv.Atoi(rawInterval)
		if err != nil || interval < 1 {
			form.Error = "Interval must be a whole number of 1 or more."
			return nil, nil, form, form.Error
		}
	}

	var byWeekday []time.Weekday
	if freq == household.FreqWeekly {
		for _, s := range rawWeekdays {
			n, convErr := strconv.Atoi(s)
			if convErr != nil || n < int(time.Sunday) || n > int(time.Saturday) {
				form.Error = "One or more selected weekday values are invalid."
				return nil, nil, form, form.Error
			}
			byWeekday = append(byWeekday, time.Weekday(n))
		}
	}

	anchor := domain.DateOf(time.Now())
	if rawAnchor != "" {
		parsed, parseErr := time.Parse("2006-01-02", rawAnchor)
		if parseErr != nil {
			form.Error = "Starting date must be in YYYY-MM-DD format."
			return nil, nil, form, form.Error
		}
		anchor = domain.DateOf(parsed)
	}

	policy, err := domain.ParseRotationPolicy(rawPolicy)
	if err != nil {
		form.Error = "Please select a valid assignment policy."
		return nil, nil, form, form.Error
	}

	points := 0
	if rawPoints != "" {
		points, err = strconv.Atoi(rawPoints)
		if err != nil || points < 0 {
			form.Error = "Points must be a whole number of 0 or more."
			return nil, nil, form, form.Error
		}
	}

	leadDays := 0
	if rawLead != "" {
		leadDays, err = strconv.Atoi(rawLead)
		if err != nil || leadDays < 0 {
			form.Error = "Lead time must be a whole number of 0 or more."
			return nil, nil, form, form.Error
		}
	}

	// Build the rotation pool (ignored by the service for claimable tasks).
	pool = make([]household.MemberID, 0, len(rawPool))
	for _, idStr := range rawPool {
		memberID, parseErr := household.ParseMemberID(idStr)
		if parseErr != nil {
			form.Error = "One or more selected members are invalid."
			return nil, nil, form, form.Error
		}
		pool = append(pool, memberID)
	}

	task = &domain.RecurringTask{
		ID:          domain.NewRecurringTaskID(),
		HouseholdID: householdID,
		Title:       rawTitle,
		Category:    category,
		Cadence: household.Cadence{
			Freq:      freq,
			Interval:  interval,
			ByWeekday: byWeekday,
			Anchor:    anchor,
		},
		RotationPolicy: policy,
		Points:         points,
		LeadTimeDays:   leadDays,
		Active:         true,
	}
	return task, pool, form, ""
}

// createTaskErrMessage maps known service-layer errors to user-readable messages.
// An empty string means the error is unexpected and should be treated as a 500.
func createTaskErrMessage(err error) string {
	switch {
	case errors.Is(err, household.ErrInvalidCadence):
		return "The cadence configuration is invalid. Please check the frequency, interval, and starting date."
	case errors.Is(err, domain.ErrNoRotationMembers):
		return "At least one rotation pool member is required for fixed or round-robin tasks."
	case errors.Is(err, domain.ErrAsNeededRequiresClaimable):
		return "As-needed chores must use the claimable assignment policy."
	default:
		return ""
	}
}

// renderNewTaskForm renders the new-task form component at the given HTTP status.
// It is called for both the GET render (200) and validation re-renders (422).
func (h *WebHandlers) renderNewTaskForm(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	form components.NewTaskForm,
	layout func(templ.Component) templ.Component,
) {
	content := components.NewTaskPage(form)
	if err := render.Render(r.Context(), w, status, layout(content)); err != nil {
		h.logger.ErrorContext(r.Context(), "new task: render form", "error", err)
	}
}

// toMemberOptions maps domain Members to the MemberOption view model used by
// the create-task form's rotation-pool picker.
func toMemberOptions(members []*household.Member) []components.MemberOption {
	opts := make([]components.MemberOption, 0, len(members))
	for _, m := range members {
		opts = append(opts, components.MemberOption{
			ID:    m.ID.String(),
			Name:  m.DisplayName,
			Color: m.Color.String(),
		})
	}
	return opts
}

// upForGrabsLabel is the heading for the group of unassigned, claimable rows.
const upForGrabsLabel = "Up for grabs"

// buildTaskRows fetches pending and overdue instances plus every open
// as-needed standing instance for the member's household, joins each with its
// parent recurring task's title and category and with its assignee member's
// display name and color, and returns a flat slice of TaskRow view models
// sorted by due date then title. A standing instance has no due date, so it
// sorts to the front of the slice by title alone; groupTaskRows then pulls
// every standing row into its own trailing "Anytime" section regardless of
// this position.
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

	// Every as-needed task's single open standing instance, regardless of due
	// date (it has none) — always shown in the list per NES-116.
	standing, err := h.instanceRepo.ListStanding(r.Context(), member.HouseholdID)
	if err != nil {
		return nil, err
	}

	combined := make([]*domain.TaskInstance, 0, len(pending)+len(overdue)+len(standing))
	combined = append(combined, pending...)
	combined = append(combined, overdue...)
	combined = append(combined, standing...)

	rows := make([]components.TaskRow, 0, len(combined))
	for _, inst := range combined {
		// taskMeta is keyed only by ACTIVE recurring tasks, so a missing entry
		// (nil) is rendered as "(archived)" by instanceToRow.
		rows = append(rows, h.instanceToRow(r.Context(), inst, taskMeta[inst.RecurringTaskID], memberByID, csrfToken, today))
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].DueOn.Equal(rows[j].DueOn) {
			return rows[i].Title < rows[j].Title
		}
		return rows[i].DueOn.Before(rows[j].DueOn)
	})
	return rows, nil
}

// instanceToRow maps a single task instance to its TaskRow view model. meta is
// the parent recurring task (nil renders as "(archived)"); memberByID resolves
// the assignee's name and color. It is shared by buildTaskRows and the
// single-row partial returned after a complete/skip/claim mutation.
func (h *WebHandlers) instanceToRow(
	ctx context.Context,
	inst *domain.TaskInstance,
	meta *domain.RecurringTask,
	memberByID map[household.MemberID]*household.Member,
	csrfToken string,
	today time.Time,
) components.TaskRow {
	title, category := "(archived)", "chore"
	if meta != nil {
		title = meta.Title
		category = meta.Category.String()
	}

	var assigneeID, assigneeName, assigneeColor string
	if inst.AssigneeID != nil {
		assigneeID = inst.AssigneeID.String()
		if m, ok := memberByID[*inst.AssigneeID]; ok {
			assigneeName = m.DisplayName
			assigneeColor = m.Color.String()
		} else {
			// The composite tenant FK + ON DELETE SET NULL should keep this from
			// happening (a deleted member clears assignee_id), so a non-resolvable
			// assignee signals a referential-integrity anomaly.
			h.logger.WarnContext(ctx, "tasks: assignee id has no matching member",
				"instance_id", inst.ID.String(), "assignee_id", assigneeID)
		}
	}

	// An instance is claimable when it is unassigned and still actionable. As of
	// NES-32 both pending and overdue are actionable.
	actionable := inst.Status == domain.StatusPending || inst.Status == domain.StatusOverdue

	// A standing instance (NES-116) has no due date; it renders "Anytime" and is
	// pinned into its own section by groupTaskRows instead of being sorted by
	// due date like a normal scheduled instance.
	standing := inst.Kind == domain.KindStanding
	var dueOn time.Time
	dueLbl := "Anytime"
	if !standing {
		if inst.DueOn == nil {
			// A scheduled instance always has a due date (enforced by the
			// task_instance_standing_no_due_on CHECK constraint); a nil DueOn here
			// would signal a referential-integrity anomaly. Render with the zero
			// date rather than panic, and log so the anomaly is visible.
			h.logger.WarnContext(ctx, "tasks: scheduled instance has no due date",
				"instance_id", inst.ID.String())
		} else {
			dueOn = *inst.DueOn
		}
		dueLbl = dueLabel(dueOn, today)
	}

	return components.TaskRow{
		InstanceID:    inst.ID.String(),
		Title:         title,
		Category:      category,
		DueOn:         dueOn,
		DueLabel:      dueLbl,
		Status:        inst.Status.String(),
		AssigneeID:    assigneeID,
		AssigneeName:  assigneeName,
		AssigneeColor: assigneeColor,
		Claimable:     actionable && inst.AssigneeID == nil,
		Standing:      standing,
		CSRFToken:     csrfToken,
	}
}

// buildInstanceRow re-reads one instance (after a mutation has committed) and
// maps it to its row view model, so a complete/skip/claim action can return just
// the updated row for an in-place HTMX swap.
func (h *WebHandlers) buildInstanceRow(r *http.Request, member *household.Member, id domain.TaskInstanceID) (components.TaskRow, error) {
	inst, err := h.instanceRepo.Get(r.Context(), member.HouseholdID, id)
	if err != nil {
		return components.TaskRow{}, err
	}
	// Resolve the parent title only when the task is still active, matching the
	// list builder (which keys off ListActive); an inactive or deleted parent
	// shows as "(archived)". A genuine lookup failure is propagated so the caller
	// falls back to an HX-Redirect rather than rendering a fake archived row.
	var meta *domain.RecurringTask
	m, err := h.taskRepo.Get(r.Context(), member.HouseholdID, inst.RecurringTaskID)
	switch {
	case err == nil && m.Active:
		meta = m
	case err == nil, errors.Is(err, domain.ErrTaskNotFound):
		// inactive or deleted parent → render as "(archived)" (meta stays nil)
	default:
		return components.TaskRow{}, err
	}
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return components.TaskRow{}, err
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}
	csrfToken := authadapter.GetCSRFToken(r.Context(), h.sm)
	today := domain.DateOf(time.Now())
	return h.instanceToRow(r.Context(), inst, meta, memberByID, csrfToken, today), nil
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

// anytimeLabel is the heading for the trailing section of as-needed standing
// instances (NES-116), shown regardless of assignee or claim state.
const anytimeLabel = "Anytime"

// groupTaskRows arranges a flat slice of TaskRow into per-member TaskGroup
// slices. Rows are grouped by the assignee's stable id (NOT display name, which
// is not unique within a household — two members named "Sam" must form two
// groups). Each group is labelled with the assignee's display name and tinted
// with their color. Member groups are ordered deterministically by display name,
// then by id as a tiebreaker. Unassigned claimable rows are collected into an
// "Up for grabs" group; every as-needed standing row (NES-116) — claimed or
// not — is pulled out into a final "Anytime" section instead of its normal
// member/claimable bucket, so dated chores and always-available ones never mix.
//
// Rows within a group preserve the incoming order (sorted by due date then title
// in buildTaskRows).
func groupTaskRows(rows []components.TaskRow) []components.TaskGroup {
	byMemberID := make(map[string]*memberGroup)
	order := make([]*memberGroup, 0)
	var claimable, anytime []components.TaskRow

	for _, row := range rows {
		if row.Standing {
			anytime = append(anytime, row)
			continue
		}
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
	if len(anytime) > 0 {
		groups = append(groups, components.TaskGroup{
			Label: anytimeLabel,
			Rows:  anytime,
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

// respondAfterTaskMutation responds after a successful complete/skip/claim.
// HTMX requests receive the updated instance row so the action swaps it in place
// via the form's hx-target/hx-swap (NES-32: act on a task without a full page
// reload); full navigations get a 303 redirect to /tasks. If the row cannot be
// re-read after the already-committed mutation, it degrades to an HX-Redirect so
// the list still refreshes.
func (h *WebHandlers) respondAfterTaskMutation(
	w http.ResponseWriter,
	r *http.Request,
	member *household.Member,
	id domain.TaskInstanceID,
) {
	if !render.IsHTMX(r) {
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
		return
	}
	row, err := h.buildInstanceRow(r, member, id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "tasks: build row after mutation", "error", err)
		w.Header().Set("HX-Redirect", "/tasks")
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := render.Render(r.Context(), w, http.StatusOK, components.TaskRowItem(row)); err != nil {
		h.logger.ErrorContext(r.Context(), "tasks: render row after mutation", "error", err)
	}
}
