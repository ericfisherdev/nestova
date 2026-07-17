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
	photoChecker domain.ProofPhotoChecker
}

// NewWebHandlers constructs a WebHandlers with all required dependencies. It
// panics when any dependency is nil so misconfigured composition roots are
// caught at startup rather than at the first HTTP request — EXCEPT
// photoChecker (NES-120), which may be nil: it is used only to populate the
// chore-proof capture/review section on a row whose parent task's
// PhotoPolicy is not domain.PhotoPolicyNone (see instanceToRow), so a
// household with no such task is unaffected by a nil value. A nil checker
// degrades a row that DOES need it to "no photos captured yet" rather than
// panicking or erroring the whole page — safe for a read path, unlike
// TaskService.CompleteInstance's fail-closed behavior on the same
// misconfiguration (see that method's doc for why the write path cannot
// degrade the same way).
func NewWebHandlers(
	svc *app.TaskService,
	taskRepo domain.RecurringTaskRepository,
	instanceRepo domain.TaskInstanceRepository,
	households household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
	photoChecker domain.ProofPhotoChecker,
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
		photoChecker: photoChecker,
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
// a full navigation) via respondAfterTaskMutation. A photo-policy rejection
// (NES-120) is handled separately (handleCompleteError) so it can surface
// inline on the row instead of as a bare error response — see that method's
// doc.
//
// Error mapping:
//   - bad CSRF                     → 403
//   - malformed id                 → 400
//   - ErrInstanceNotFound          → 404
//   - ErrInstanceInTerminalState   → 409
//   - ErrBeforePhotoRequired/
//     ErrAfterPhotoRequired        → 422, inline on the row for an HTMX
//     request (see handleCompleteError); plain text otherwise
//   - other                        → 500
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
		h.handleCompleteError(w, r, member, id, err)
		return
	}

	h.respondAfterTaskMutation(w, r, member, id)
}

// photoPolicyErrorMessage maps a photo-policy completion error (NES-120) to
// a friendly, user-facing message, or "" for any other error — the signal
// handleCompleteError uses to decide whether an inline row re-render
// applies at all.
func photoPolicyErrorMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrBeforePhotoRequired):
		return "Take a before photo before marking this chore complete."
	case errors.Is(err, domain.ErrAfterPhotoRequired):
		return "Take an after photo before marking this chore complete."
	default:
		return ""
	}
}

// handleCompleteError handles a CompleteInstance failure. For
// ErrBeforePhotoRequired/ErrAfterPhotoRequired on an HTMX request, it
// re-renders the SAME chore row (TaskRowItem, the form's own hx-target) at
// HTTP 422 with a friendly PhotoError message set — Layout's htmx-config
// meta tag opts 422 in to htmx's swap behavior (off by default for 4xx/5xx;
// see that meta tag's doc), so the row updates in place with the message
// and no page reload, matching every other complete/skip/claim mutation's
// existing "swap this row" contract instead of introducing a new one. Any
// other error, or the same error on a non-HTMX request (a plain form POST,
// which never reads response bodies as anything but a rendered document),
// falls through to the existing plain-text handleMutationError path.
func (h *WebHandlers) handleCompleteError(
	w http.ResponseWriter,
	r *http.Request,
	member *household.Member,
	id domain.TaskInstanceID,
	err error,
) {
	msg := photoPolicyErrorMessage(err)
	if msg == "" || !render.IsHTMX(r) {
		h.handleMutationError(w, r, err)
		return
	}
	row, buildErr := h.buildInstanceRow(r, member, id)
	if buildErr != nil {
		h.logger.ErrorContext(r.Context(), "tasks: build row after photo policy error", "error", buildErr)
		h.handleMutationError(w, r, err)
		return
	}
	row.PhotoError = msg
	if renderErr := render.Render(r.Context(), w, http.StatusUnprocessableEntity, components.TaskRowItem(row)); renderErr != nil {
		h.logger.ErrorContext(r.Context(), "tasks: render photo policy error", "error", renderErr)
	}
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

// Groups handles GET /tasks/groups. It rebuilds the grouped task list and
// renders just the #task-groups container fragment (NES-118): a claimed
// row's countdown badge, on its claim's expiry, targets this endpoint
// instead of re-rendering only itself, so the reverted claim's row is
// re-grouped under its correct heading (e.g. moving from its former
// assignee's section into "Up for grabs") in the same request, rather than
// rendering correctly in place but staying nested under the wrong group
// heading until a full page reload re-ran the grouping. This is a passive
// read with no state to change, so it is a GET and always succeeds.
func (h *WebHandlers) Groups(w http.ResponseWriter, r *http.Request) {
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := h.buildTaskRows(r, member)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "tasks: build task rows for group refresh", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	groups := groupTaskRows(rows)
	if err := render.Render(r.Context(), w, http.StatusOK, components.TaskGroupsFragment(groups, false)); err != nil {
		h.logger.ErrorContext(r.Context(), "tasks: render group refresh", "error", err)
	}
}

// BuildGroups rebuilds the current grouped task list without rendering it.
// It is the read half of Groups, exported so a sibling handler type can
// refresh #task-groups after mutating a task instance outside this package's
// own mutation endpoints — specifically, TradeWebHandlers.Accept (NES-122),
// which swaps two instances' assignees and must reflect that in an
// already-open /tasks page the same way a claim's expiry already does.
// Keeping this as a narrow, single-method capability (rather than exposing
// WebHandlers' full read/write surface to TradeWebHandlers) is why it is
// consumed through the adapter's own minimal taskGroupsBuilder interface.
func (h *WebHandlers) BuildGroups(r *http.Request, member *household.Member) ([]components.TaskGroup, error) {
	rows, err := h.buildTaskRows(r, member)
	if err != nil {
		return nil, err
	}
	return groupTaskRows(rows), nil
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
	rawPhotoPolicy := r.FormValue("photo_policy")
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
		PhotoPolicy:       rawPhotoPolicy,
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

	// An empty submission defaults to PhotoPolicyNone rather than failing
	// validation — the rendered <select> always carries "none" as its
	// pre-selected default option (see newtask.templ), so an empty value
	// here only ever comes from a non-browser client that omitted the
	// field entirely, not a user who left a choice unmade.
	photoPolicy := domain.PhotoPolicyNone
	if rawPhotoPolicy != "" {
		photoPolicy, err = domain.ParsePhotoPolicy(rawPhotoPolicy)
		if err != nil {
			form.Error = "Please select a valid proof-photo requirement."
			return nil, nil, form, form.Error
		}
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
		PhotoPolicy:    photoPolicy,
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

	// NES-120: one batched ProofPhotoChecker call for every actionable,
	// photo-policy-requiring instance in the page, instead of one call per
	// row — see batchProofPhotoIDs' own doc.
	photoIDs := h.batchProofPhotoIDs(r.Context(), member.HouseholdID, combined, taskMeta)

	rows := make([]components.TaskRow, 0, len(combined))
	for _, inst := range combined {
		// taskMeta is keyed only by ACTIVE recurring tasks, so a missing entry
		// (nil) is rendered as "(archived)" by instanceToRow.
		rows = append(rows, h.instanceToRow(r.Context(), inst, taskMeta[inst.RecurringTaskID], memberByID, csrfToken, today, member.ID, photoIDs))
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
// the assignee's name and color; viewerID is the member currently viewing the
// list, used to compute Mine (NES-122). photoIDs is the NES-120 batch lookup
// buildTaskRows/buildInstanceRow already resolved via ONE ProofPhotoChecker
// call (see batchProofPhotoIDs) — this method performs no I/O of its own,
// only a map read, so calling it per row never reintroduces the N+1 the
// batch call exists to avoid. It is shared by buildTaskRows and the
// single-row partial returned after a complete/skip/claim mutation.
func (h *WebHandlers) instanceToRow(
	ctx context.Context,
	inst *domain.TaskInstance,
	meta *domain.RecurringTask,
	memberByID map[household.MemberID]*household.Member,
	csrfToken string,
	today time.Time,
	viewerID household.MemberID,
	photoIDs map[domain.TaskInstanceID]domain.ProofPhotoIDs,
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

	// A countdown badge renders only when the claim itself carries an expiry
	// risk (NES-118): self-claimed and rotation-assigned instances leave
	// ClaimExpiresAt nil per Claim's contract, so they never populate this
	// field and never render a badge.
	var claimExpiresAtISO string
	if inst.ClaimExpiresAt != nil {
		claimExpiresAtISO = inst.ClaimExpiresAt.UTC().Format(time.RFC3339)
	}

	photoPolicy, beforeURL, afterURL := proofPhotoFields(meta, actionable, photoIDs[inst.ID])

	return components.TaskRow{
		InstanceID:        inst.ID.String(),
		Title:             title,
		Category:          category,
		DueOn:             dueOn,
		DueLabel:          dueLbl,
		Status:            inst.Status.String(),
		AssigneeID:        assigneeID,
		AssigneeName:      assigneeName,
		AssigneeColor:     assigneeColor,
		Claimable:         actionable && inst.AssigneeID == nil,
		Standing:          standing,
		ClaimExpiresAtISO: claimExpiresAtISO,
		CSRFToken:         csrfToken,
		Mine:              inst.AssigneeID != nil && *inst.AssigneeID == viewerID,
		Tradeable:         domain.IsInstanceTradeable(inst),
		PhotoPolicy:       photoPolicy,
		BeforePhotoRawURL: beforeURL,
		AfterPhotoRawURL:  afterURL,
	}
}

// choreProofPhotoRawPath is the URL prefix the NES-120 chore-proof raw-bytes
// serving route (registered at the composition root; see
// registerChoreProofPhotoRoutes) is mounted under.
const choreProofPhotoRawPath = "/tasks/photos/"

// choreProofPhotoRawURL builds the raw-bytes URL for a chore-proof photo id.
func choreProofPhotoRawURL(photoID string) string {
	return choreProofPhotoRawPath + photoID + "/raw"
}

// proofPhotoFields resolves a row's photo-policy display fields (NES-120)
// from an already-fetched batch lookup — the parent task's PhotoPolicy as a
// plain string, and — only when the instance is actionable and the policy
// actually requires a photo — the raw-bytes URLs of any already-captured
// before/after photos. Skipped entirely for an inactive/archived parent
// (meta == nil, mirroring how title/category already degrade to
// "(archived)") or a non-actionable (done/skipped) instance. Performs no
// I/O: ids is a single map read (photoIDs[instance.ID] at the call site),
// which is empty/zero-valued for an instance that was never eligible for
// the batch lookup in the first place (see batchProofPhotoIDs) — the same
// "no photos yet" outcome either way.
func proofPhotoFields(meta *domain.RecurringTask, actionable bool, ids domain.ProofPhotoIDs) (policy, beforeURL, afterURL string) {
	if meta == nil || !meta.PhotoPolicy.RequiresPhotos() {
		return "", "", ""
	}
	policy = meta.PhotoPolicy.String()
	if !actionable {
		return policy, "", ""
	}
	if ids.BeforeID != "" {
		beforeURL = choreProofPhotoRawURL(ids.BeforeID)
	}
	if ids.AfterID != "" {
		afterURL = choreProofPhotoRawURL(ids.AfterID)
	}
	return policy, beforeURL, afterURL
}

// batchProofPhotoIDs resolves the NES-120 chore-proof photo ids for every
// ACTIONABLE instance in instances whose parent task's PhotoPolicy requires
// them, via ONE ProofPhotoChecker.ProofPhotosByInstances call — the N+1
// avoidance behind proofPhotoFields: without this, instanceToRow would issue
// its own ProofPhotoChecker query per row, and a page with an unbounded
// overdue list (see ListByHousehold's own doc) would cost one round trip per
// photo-policy row instead of one for the whole page.
//
// A nil h.photoChecker (see NewWebHandlers' doc) or a lookup error degrades
// to a nil map — instanceToRow's map read on a nil map is a safe zero-value
// read, so every row simply shows "no photos captured yet" rather than
// failing the whole page — safe here because this is a read-only display
// concern, unlike TaskService.CompleteInstance's fail-closed behavior on the
// same missing dependency.
func (h *WebHandlers) batchProofPhotoIDs(
	ctx context.Context,
	householdID household.HouseholdID,
	instances []*domain.TaskInstance,
	taskMeta map[domain.RecurringTaskID]*domain.RecurringTask,
) map[domain.TaskInstanceID]domain.ProofPhotoIDs {
	if h.photoChecker == nil {
		return nil
	}
	var eligible []domain.TaskInstanceID
	for _, inst := range instances {
		actionable := inst.Status == domain.StatusPending || inst.Status == domain.StatusOverdue
		meta := taskMeta[inst.RecurringTaskID]
		if !actionable || meta == nil || !meta.PhotoPolicy.RequiresPhotos() {
			continue
		}
		eligible = append(eligible, inst.ID)
	}
	if len(eligible) == 0 {
		return nil
	}
	photoIDs, err := h.photoChecker.ProofPhotosByInstances(ctx, householdID, eligible)
	if err != nil {
		h.logger.WarnContext(ctx, "tasks: batch check proof photos", "error", err)
		return nil
	}
	return photoIDs
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
	// A single-instance batch call — still goes through batchProofPhotoIDs
	// (not the single-instance ProofPhotos) so instanceToRow has exactly one
	// code path for both callers; one instance is the cheapest possible
	// batch, not a special case.
	photoIDs := h.batchProofPhotoIDs(r.Context(), member.HouseholdID, []*domain.TaskInstance{inst},
		map[domain.RecurringTaskID]*domain.RecurringTask{inst.RecurringTaskID: meta})
	return h.instanceToRow(r.Context(), inst, meta, memberByID, csrfToken, today, member.ID, photoIDs), nil
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
	case errors.Is(err, domain.ErrBeforePhotoRequired), errors.Is(err, domain.ErrAfterPhotoRequired):
		// NES-120: reached for a photo-gated completion failure on a
		// non-HTMX request (handleCompleteError's inline-row path only
		// applies to an HTMX request) — the same friendly message, as a
		// plain-text 422 body instead of a swapped row fragment.
		http.Error(w, photoPolicyErrorMessage(err), http.StatusUnprocessableEntity)
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
