// Package adapter serves the /go/{action}/{id} kiosk QR deep links (NES-129):
// verifying the signed link, requiring the scanning phone's own member
// session (never the kiosk device identity — see the composition root's route
// registration), and performing the named action through the SAME
// application-layer services the member-facing pages already use
// (tasksapp.TaskService, tasksapp.RewardService), so every domain rule
// (claim eligibility, point balance, tenant isolation, ...) is enforced
// exactly once, in exactly the place it already lived.
//
// A link's signature is never itself an authorization grant — see
// internal/deeplink/domain's package doc — so every mutation here still goes
// through the same tenant-scoped service call a member-facing button would
// make, keyed by the AUTHENTICATED member's own household id, never anything
// carried in the URL.
package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/ericfisherdev/nestcore/render"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	deeplinkapp "github.com/ericfisherdev/nestova/internal/deeplink/app"
	deeplinkdomain "github.com/ericfisherdev/nestova/internal/deeplink/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// Friendly, deliberately non-specific copy for the signature-verification
// failure page (AC3): whether the link was tampered with or has simply
// expired, the remedy and the message are identical, so neither case can be
// used as an oracle to probe which failure occurred (mirroring
// KioskWebHandlers.Activate's "never distinguishing which of the three
// applies" precedent for activation codes).
const (
	rescanHeading = "This link isn't valid anymore"
	rescanMessage = "Please rescan the QR code from the kiosk to get a fresh one."
)

// pinFormField is the form field the confirm screen submits a member's PIN
// under (NES-166) — the same name the /tasks chore row uses.
const pinFormField = "pin"

// TaskPINAuthorizer is the narrow port onto the auth context's per-member
// PIN gate this confirm flow depends on (NES-166). The complete-task deep
// link reaches TaskService by a different route than the chore list does,
// so it must apply the same gate — otherwise a notification link would be a
// way around the PIN the /tasks row demands. It needs one method less than
// the tasks adapter's port of the same name: this screen confirms exactly
// one instance, so a single-member IsEnrolled replaces the whole-household
// enrolment lookup a list render needs.
type TaskPINAuthorizer interface {
	// AuthorizeTaskAction verifies submittedPIN against assigneeID's own
	// PIN, returning assigneeID as the actor on success, the zero MemberID
	// with a nil error when the assignee has no PIN enrolled, and
	// authdomain.ErrPINMismatch/ErrPINLocked when the action must be
	// refused.
	AuthorizeTaskAction(ctx context.Context, assigneeID household.MemberID, submittedPIN string) (household.MemberID, error)
	// IsEnrolled reports whether memberID has a PIN on file, so the confirm
	// screen renders a PIN field on exactly the links that will be gated.
	IsEnrolled(ctx context.Context, memberID household.MemberID) (bool, error)
}

// WebHandlers serves the /go/{action}/{id} deep-link confirm screens and
// their POST actions. It depends directly on each target bounded context's
// APPLICATION-layer service or read repository — never on their adapter/
// WebHandlers types — mirroring KioskWebHandlers' own documented rationale:
// those handlers are built around member-specific concerns (CSRF forms, HTMX
// fragments, full page shells) this standalone confirm flow does not share.
type WebHandlers struct {
	signer         *deeplinkapp.Signer
	taskSvc        *tasksapp.TaskService
	recurringTasks tasksdomain.RecurringTaskRepository
	taskInstances  tasksdomain.TaskInstanceRepository
	rewardSvc      *tasksapp.RewardService
	rewards        tasksdomain.RewardRepository
	pin            TaskPINAuthorizer
	sm             *scs.SessionManager
	logger         *slog.Logger
	now            func() time.Time
	limiter        *perKeyLimiter
}

// NewWebHandlers constructs WebHandlers with all required dependencies. It
// panics when any dependency is nil so a misconfigured composition root is
// caught at startup. now defaults to time.Now.
func NewWebHandlers(
	signer *deeplinkapp.Signer,
	taskSvc *tasksapp.TaskService,
	recurringTasks tasksdomain.RecurringTaskRepository,
	taskInstances tasksdomain.TaskInstanceRepository,
	rewardSvc *tasksapp.RewardService,
	rewards tasksdomain.RewardRepository,
	pin TaskPINAuthorizer,
	sm *scs.SessionManager,
	logger *slog.Logger,
	now func() time.Time,
) *WebHandlers {
	switch {
	case signer == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil Signer")
	case taskSvc == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil TaskService")
	case recurringTasks == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil RecurringTaskRepository")
	case taskInstances == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil TaskInstanceRepository")
	case rewardSvc == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil RewardService")
	case rewards == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil RewardRepository")
	case pin == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil TaskPINAuthorizer")
	case sm == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil session manager")
	case logger == nil:
		panic("deeplink/adapter: NewWebHandlers requires a non-nil logger")
	}
	if now == nil {
		now = time.Now
	}
	return &WebHandlers{
		signer: signer, taskSvc: taskSvc, recurringTasks: recurringTasks, taskInstances: taskInstances,
		rewardSvc: rewardSvc, rewards: rewards, pin: pin, sm: sm, logger: logger, now: now,
		limiter: newPerKeyLimiter(confirmRateEvery, confirmRateBurst),
	}
}

// ---------------------------------------------------------------------------
// Route entry points — thin wrappers so both the {action}/{id} shape and
// add-chore's id-less shape share the same show/confirm implementation.
// ---------------------------------------------------------------------------

// Show handles GET /go/{action}/{id}.
func (h *WebHandlers) Show(w http.ResponseWriter, r *http.Request) {
	h.show(w, r, deeplinkdomain.Action(r.PathValue("action")), r.PathValue("id"))
}

// Confirm handles POST /go/{action}/{id}.
func (h *WebHandlers) Confirm(w http.ResponseWriter, r *http.Request) {
	h.confirm(w, r, deeplinkdomain.Action(r.PathValue("action")), r.PathValue("id"))
}

// ShowAddChore handles GET /go/add-chore — add-chore carries no id (see
// [deeplinkdomain.Action.RequiresID]), so it is routed separately from the
// {action}/{id} pattern rather than trying to make one mux pattern match a
// variable number of path segments.
func (h *WebHandlers) ShowAddChore(w http.ResponseWriter, r *http.Request) {
	h.show(w, r, deeplinkdomain.ActionAddChore, "")
}

// ConfirmAddChore handles POST /go/add-chore.
func (h *WebHandlers) ConfirmAddChore(w http.ResponseWriter, r *http.Request) {
	h.confirm(w, r, deeplinkdomain.ActionAddChore, "")
}

// Done handles GET /go/{action}/done: the PRG (Post-Redirect-Get) landing
// page confirmClaim/confirmComplete/confirmRedeem redirect to on success
// (NES-129), so a browser refresh after a completed action replays this
// harmless GET instead of resubmitting the mutating POST. It performs no
// action of its own — by the time a request reaches this handler, the
// action already happened (or, for redeem, was already rejected and the
// member is retrying the confirm screen instead) — so it needs neither CSRF
// nor a signature to verify, only the same MEMBER session gate as every
// other /go/ route (enforced at the router; see registerDeepLinkPages).
//
// Routing note: "GET /go/{action}/done" and "GET /go/{action}/{id}" are both
// two-segment patterns, but net/http's ServeMux resolves the exact path
// ".../done" to this handler (a literal segment is more specific than a
// wildcard at the same position) while any other id still reaches Show —
// verified directly against net/http's documented precedence rules, not
// assumed.
func (h *WebHandlers) Done(w http.ResponseWriter, r *http.Request) {
	action := deeplinkdomain.Action(r.PathValue("action"))
	heading, message, ok := doneMessage(action)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.renderMessage(w, r, http.StatusOK, heading, message)
}

// doneURL builds the PRG redirect target for action's "done" page.
func doneURL(action deeplinkdomain.Action) string {
	return "/go/" + string(action) + "/done"
}

// doneMessage returns the canned success heading/message Done displays for
// action, and false for any action with no "done" page (add-chore redirects
// straight to /tasks/new instead — it has no action of its own to confirm
// completion of).
func doneMessage(action deeplinkdomain.Action) (heading, message string, ok bool) {
	switch action {
	case deeplinkdomain.ActionClaimTask:
		return "Chore claimed", "You're on it — thanks for taking care of it!", true
	case deeplinkdomain.ActionCompleteTask:
		return "Chore completed", "Nice work!", true
	case deeplinkdomain.ActionRedeemReward:
		return "Reward redeemed", "Enjoy — a parent will follow up to fulfill it.", true
	default:
		return "", "", false
	}
}

// ---------------------------------------------------------------------------
// GET: render the confirm screen
// ---------------------------------------------------------------------------

// show renders the phone-sized confirm screen for action/id: it verifies the
// link's signature (rendering the friendly rescan page on failure, AC3),
// resolves the current MEMBER session (guaranteed present by RequireMember at
// the router — see registerDeepLinkPages), and looks up just enough about the
// target to describe it, WITHOUT performing the action — see [WebHandlers.confirm]
// for the POST that actually acts.
//
// Error mapping:
//   - Unknown action, or an id shape the action does not accept → 404.
//   - Missing/malformed/expired/tampered signature → the friendly rescan
//     page (400), never distinguishing which (AC3).
//   - Target not found or belongs to another household (tenant isolation,
//     AC5) → 404, since [tasksdomain.TaskInstanceRepository.Get] and
//     [tasksdomain.RewardRepository.GetReward] both fold "unknown" and
//     "wrong household" into the same not-found sentinel.
//   - Other repository error → 500.
func (h *WebHandlers) show(w http.ResponseWriter, r *http.Request, action deeplinkdomain.Action, id string) {
	path, err := action.Path(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, _, ok := h.verifySignature(w, r, path); !ok {
		return
	}

	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		// Defensive: RequireMember already guarantees this at the router; a
		// handler must never assume its own gate ran.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	view, err := h.buildConfirmView(r.Context(), member, action, id, r.URL.RequestURI())
	if err != nil {
		h.respondLookupError(w, r, action, err)
		return
	}
	if renderErr := render.Render(r.Context(), w, http.StatusOK, components.DeepLinkConfirmPage(view)); renderErr != nil {
		h.logger.ErrorContext(r.Context(), "deeplink: render confirm page", "error", renderErr)
	}
}

// buildConfirmView assembles the view model for action's confirm screen.
// formAction is the original request's exact path+query (preserving exp/sig)
// so the confirm POST re-submits the same signed link that authorized this
// GET.
func (h *WebHandlers) buildConfirmView(
	ctx context.Context,
	member *household.Member,
	action deeplinkdomain.Action,
	id string,
	formAction string,
) (components.DeepLinkConfirmView, error) {
	view := components.DeepLinkConfirmView{
		FormAction: formAction,
		CSRFToken:  authadapter.GetCSRFToken(ctx, h.sm),
	}

	switch action {
	case deeplinkdomain.ActionClaimTask, deeplinkdomain.ActionCompleteTask:
		instanceID, err := tasksdomain.ParseTaskInstanceID(id)
		if err != nil {
			return components.DeepLinkConfirmView{}, tasksdomain.ErrInstanceNotFound
		}
		inst, err := h.taskInstances.Get(ctx, member.HouseholdID, instanceID)
		if err != nil {
			return components.DeepLinkConfirmView{}, err
		}
		title := choreTitle(ctx, h.recurringTasks, member.HouseholdID, inst.RecurringTaskID)
		if action == deeplinkdomain.ActionClaimTask {
			view.Heading = "Claim this chore?"
			view.ConfirmLabel = "Claim"
		} else {
			view.Heading = "Mark this chore complete?"
			view.ConfirmLabel = "Mark complete"
			// NES-166: the completion is PIN-gated whenever the instance's
			// assignee has one enrolled, so the confirm screen must offer
			// the field the POST will be refused without.
			if inst.AssigneeID != nil {
				enrolled, err := h.pin.IsEnrolled(ctx, *inst.AssigneeID)
				if err != nil {
					return components.DeepLinkConfirmView{}, err
				}
				view.RequiresPIN = enrolled
			}
		}
		view.Description = title
		return view, nil

	case deeplinkdomain.ActionRedeemReward:
		rewardID, err := tasksdomain.ParseRewardID(id)
		if err != nil {
			return components.DeepLinkConfirmView{}, tasksdomain.ErrRewardNotFound
		}
		reward, err := h.rewards.GetReward(ctx, member.HouseholdID, rewardID)
		if err != nil {
			return components.DeepLinkConfirmView{}, err
		}
		view.Heading = "Redeem this reward?"
		view.Description = reward.Name + " — " + strconv.Itoa(reward.CostPoints) + " points"
		view.ConfirmLabel = "Redeem"
		return view, nil

	case deeplinkdomain.ActionAddChore:
		view.Heading = "Add a chore"
		view.Description = "Continue to the new chore form."
		view.ConfirmLabel = "Continue"
		return view, nil

	default:
		return components.DeepLinkConfirmView{}, deeplinkdomain.ErrUnknownAction
	}
}

// choreTitle resolves a chore instance's display title, degrading to
// "(archived)" on any lookup failure — mirroring
// KioskWebHandlers.buildChoresView's identical fallback for the same reason
// (an archived/deleted recurring task must not turn an otherwise-valid
// instance lookup into a hard error).
func choreTitle(ctx context.Context, recurringTasks tasksdomain.RecurringTaskRepository, householdID household.HouseholdID, id tasksdomain.RecurringTaskID) string {
	rt, err := recurringTasks.Get(ctx, householdID, id)
	if err != nil {
		return "(archived)"
	}
	return rt.Title
}

// respondLookupError maps a show()-time lookup failure to an HTTP response.
func (h *WebHandlers) respondLookupError(w http.ResponseWriter, r *http.Request, action deeplinkdomain.Action, err error) {
	switch {
	case errors.Is(err, tasksdomain.ErrInstanceNotFound), errors.Is(err, tasksdomain.ErrRewardNotFound),
		errors.Is(err, deeplinkdomain.ErrUnknownAction):
		http.NotFound(w, r)
	default:
		h.logger.ErrorContext(r.Context(), "deeplink: build confirm view", "action", string(action), "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// POST: perform the action
// ---------------------------------------------------------------------------

// confirm performs action/id as the current member, after re-verifying the
// CSRF token, the link's signature (defense in depth: a link that expired
// between the GET and this POST is still rejected here), and a per-member
// rate limit (NES-129) — then delegates to the SAME application-layer service
// method a member-facing button would call, so every domain rule (claim
// eligibility, point balance, tenant isolation) is enforced exactly once, in
// exactly the place it already lived. The signature itself never authorizes
// anything beyond "this URL was minted by this server" — see the package doc.
//
// Error mapping:
//   - Bad CSRF                                        → 403
//   - Unknown action, or an id shape the action does not accept → 404
//   - Missing/malformed/expired/tampered signature     → the friendly rescan
//     page (400), never distinguishing which (AC3)
//   - Rate limit exceeded                              → 429
//   - Malformed id                                     → 400
//   - Target not found / wrong household (AC5)          → 404
//   - Domain-rule rejection (already claimed, terminal state, insufficient
//     points, out of stock) → 409, with a friendly message
//   - Redeem specifically: the signed link was already used to redeem
//     successfully once → 409, with a friendly "already used" message (see
//     confirmRedeem)
//   - Other error                                       → 500
//   - Success: a PRG (Post-Redirect-Get) 303 redirect — to /go/{action}/done
//     (claim/complete/redeem, see Done) or to /tasks/new (add-chore, which
//     has no action of its own to perform — it exists purely to land the
//     member on the existing new-task form). Every confirm POST redirects
//     rather than rendering its result directly, so a browser refresh after
//     a completed action replays a harmless GET, never the mutating POST.
func (h *WebHandlers) confirm(w http.ResponseWriter, r *http.Request, action deeplinkdomain.Action, id string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}

	path, err := action.Path(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The verified expiry is not needed beyond this signature check itself:
	// the redemption-replay guard (confirmRedeem) is now a durable DATABASE
	// constraint keyed by a hash of sigBytes, with no expiry-based sweeping
	// of its own to worry about — see confirmRedeem's doc comment.
	_, sigBytes, ok := h.verifySignature(w, r, path)
	if !ok {
		return
	}

	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if !h.limiter.allow(member.ID.String()) {
		http.Error(w, "too many attempts — please slow down", http.StatusTooManyRequests)
		return
	}

	switch action {
	case deeplinkdomain.ActionClaimTask:
		h.confirmClaim(w, r, member, id)
	case deeplinkdomain.ActionCompleteTask:
		h.confirmComplete(w, r, member, id)
	case deeplinkdomain.ActionRedeemReward:
		h.confirmRedeem(w, r, member, id, sigBytes)
	case deeplinkdomain.ActionAddChore:
		http.Redirect(w, r, "/tasks/new", http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

// confirmClaim performs the claim, then PRG-redirects (303) to the shared
// "done" page rather than rendering the confirmation directly (NES-129: a
// browser refresh after a direct render would resubmit the POST). Unlike
// confirmRedeem, this redirect is pure UX consistency, not a correctness
// fix: re-running ClaimInstance for the SAME member is already a safe no-op
// by design (NES-117 — a self-claim on an instance the member already holds
// just re-records the claim with no expiry and no penalty), so a resubmitted
// claim POST was never a real double-effect risk.
func (h *WebHandlers) confirmClaim(w http.ResponseWriter, r *http.Request, member *household.Member, id string) {
	instanceID, err := tasksdomain.ParseTaskInstanceID(id)
	if err != nil {
		http.Error(w, "invalid chore id", http.StatusBadRequest)
		return
	}
	if err := h.taskSvc.ClaimInstance(r.Context(), member.HouseholdID, instanceID, member.ID); err != nil {
		h.respondTaskMutationError(w, r, err)
		return
	}
	http.Redirect(w, r, doneURL(deeplinkdomain.ActionClaimTask), http.StatusSeeOther)
}

// NES-166: the completion is gated on the assignee's PIN exactly as the
// /tasks chore row is (see authorizeComplete) — following the signed link
// alone still never completes anything, and now neither does confirming it
// without the assignee's PIN.
//
// confirmComplete performs the completion, then PRG-redirects (303) to the
// shared "done" page — see confirmClaim's doc for the general PRG
// rationale. As with claim, this is UX consistency rather than a
// correctness fix: CompleteAndAward's own terminal-state guard already
// makes a resubmitted POST safe — it returns ErrInstanceInTerminalState and
// awards nothing a second time (see TaskService.CompleteInstance's doc).
func (h *WebHandlers) confirmComplete(w http.ResponseWriter, r *http.Request, member *household.Member, id string) {
	instanceID, err := tasksdomain.ParseTaskInstanceID(id)
	if err != nil {
		http.Error(w, "invalid chore id", http.StatusBadRequest)
		return
	}
	by, guarded, ok := h.authorizeComplete(w, r, member, instanceID)
	if !ok {
		return
	}
	// A PIN-verified actor must be re-asserted as the assignee inside the
	// mutation itself (NES-166): the instance the PIN was checked against can
	// be traded away before this write lands.
	var opts []tasksapp.ActionOption
	if guarded {
		opts = append(opts, tasksapp.RequireAssignee(by))
	}
	if err := h.taskSvc.CompleteInstance(r.Context(), member.HouseholdID, instanceID, by, h.now(), opts...); err != nil {
		h.respondTaskMutationError(w, r, err)
		return
	}
	http.Redirect(w, r, doneURL(deeplinkdomain.ActionCompleteTask), http.StatusSeeOther)
}

// authorizeComplete applies the NES-166 PIN gate to a complete-task confirm
// POST: the same rules the tasks adapter enforces on POST /tasks/{id}/complete
// — no assignee or an unenrolled assignee leaves the signed-in member as the
// actor, an enrolled assignee's own PIN must verify and makes THAT member the
// actor, and anything else refuses the action.
//
// A refusal re-renders this confirm screen (the member is on a phone, with a
// field to retype into) at 403 with a form-level message that never
// discloses whether a member is enrolled. ok=false means the response has
// already been written and the caller must return without completing
// anything.
func (h *WebHandlers) authorizeComplete(
	w http.ResponseWriter,
	r *http.Request,
	member *household.Member,
	instanceID tasksdomain.TaskInstanceID,
) (actorID household.MemberID, verifiedAgainstAssignee bool, ok bool) {
	inst, err := h.taskInstances.Get(r.Context(), member.HouseholdID, instanceID)
	if err != nil {
		h.respondTaskMutationError(w, r, err)
		return household.MemberID{}, false, false
	}
	if inst.AssigneeID == nil {
		return member.ID, false, true
	}

	actor, err := h.pin.AuthorizeTaskAction(r.Context(), *inst.AssigneeID, r.FormValue(pinFormField))
	switch {
	case err == nil && actor == (household.MemberID{}):
		return member.ID, false, true
	case err == nil:
		return actor, true, true
	}

	msg, known := authadapter.PINVerificationMessage(err)
	if !known {
		h.logger.ErrorContext(r.Context(), "deeplink: authorize complete", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return household.MemberID{}, false, false
	}
	h.respondPINError(w, r, member, deeplinkdomain.ActionCompleteTask, instanceID.String(), msg)
	return household.MemberID{}, false, false
}

// respondPINError re-renders the confirm screen at 403 with errMsg shown
// under its PIN field, so a mistyped PIN is retried in place rather than
// dead-ending on a message page. If the view cannot be rebuilt, it degrades
// to the same message as plain text at the same status.
func (h *WebHandlers) respondPINError(
	w http.ResponseWriter,
	r *http.Request,
	member *household.Member,
	action deeplinkdomain.Action,
	id string,
	errMsg string,
) {
	view, err := h.buildConfirmView(r.Context(), member, action, id, r.URL.RequestURI())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "deeplink: build confirm view after pin error", "error", err)
		http.Error(w, errMsg, http.StatusForbidden)
		return
	}
	view.Error = errMsg
	if renderErr := render.Render(r.Context(), w, http.StatusForbidden, components.DeepLinkConfirmPage(view)); renderErr != nil {
		h.logger.ErrorContext(r.Context(), "deeplink: render confirm page after pin error", "error", renderErr)
	}
}

// respondTaskMutationError maps a claim/complete failure to an HTTP response.
func (h *WebHandlers) respondTaskMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tasksdomain.ErrInstanceNotFound):
		http.NotFound(w, r)
	case errors.Is(err, tasksdomain.ErrInstanceInTerminalState):
		h.renderMessage(w, r, http.StatusConflict, "Already done", "This chore was already finished.")
	case errors.Is(err, tasksdomain.ErrInstanceAlreadyClaimed):
		h.renderMessage(w, r, http.StatusConflict, "Already claimed", "Someone else already claimed this chore.")
	case errors.Is(err, tasksdomain.ErrAssigneeChanged):
		// NES-166: the PIN was right, but a chore trade moved the instance
		// to another member between verifying it and writing the
		// completion. Nothing to retype, so this is a message page rather
		// than a re-rendered form.
		h.renderMessage(w, r, http.StatusConflict, "Chore reassigned",
			"This chore was just reassigned to someone else, so it was not completed.")
	case errors.Is(err, tasksdomain.ErrBeforePhotoRequired):
		// NES-120: this confirm flow performs the action directly (no
		// capture UI of its own — the member takes proof photos from the
		// /tasks chore row, not from this phone-sized confirm screen), so
		// the friendly message points back there rather than offering a
		// dead-end "try again" on this page.
		h.renderMessage(w, r, http.StatusConflict, "Before photo needed",
			"This chore needs a before photo first. Take one from the chore's card on the tasks page, then come back and confirm.")
	case errors.Is(err, tasksdomain.ErrAfterPhotoRequired):
		h.renderMessage(w, r, http.StatusConflict, "After photo needed",
			"This chore needs an after photo first. Take one from the chore's card on the tasks page, then come back and confirm.")
	default:
		h.logger.ErrorContext(r.Context(), "deeplink: task mutation", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// confirmRedeem performs the redemption. Unlike claim/complete, a REPEATED
// redeem has a real side effect every time (RedeemWithDebit deducts points
// and, for a finite-stock reward, decrements availability again) — nothing
// else in this request stops a double-tap, a resubmitted-on-refresh POST, or
// a second request landing within the rate limiter's own burst from
// redeeming twice, since the signed link's signature and the member's CSRF
// token both stay valid for reuse until the link's own expiry.
//
// The guard against that is a DATABASE constraint, not an in-process one: it
// hashes sigBytes (the link's own canonical decoded signature, from
// confirm's call to verifySignature) via deeplinkapp.HashSignature and
// passes it to RewardService.RedeemViaDeepLink, which persists it as the
// redemption's DeepLinkSignatureHash — enforced by the database's
// reward_redemption_deep_link_signature_uniq partial unique index inside
// the SAME transaction as the debit. This holds durably across process
// restarts and multiple server instances, unlike the in-process map this
// replaced: a resubmitted POST for the same signed link always reaches the
// SAME database constraint and is rejected with
// [tasksdomain.ErrDeepLinkAlreadyRedeemed], regardless of which server
// process handles it or how much time has passed.
func (h *WebHandlers) confirmRedeem(w http.ResponseWriter, r *http.Request, member *household.Member, id string, sigBytes []byte) {
	rewardID, err := tasksdomain.ParseRewardID(id)
	if err != nil {
		http.Error(w, "invalid reward id", http.StatusBadRequest)
		return
	}
	signatureHash := deeplinkapp.HashSignature(sigBytes)
	if _, err := h.rewardSvc.RedeemViaDeepLink(r.Context(), member.HouseholdID, member.ID, rewardID, signatureHash); err != nil {
		h.respondRedeemError(w, r, err)
		return
	}
	// PRG: unlike claim/complete, this redirect IS part of the correctness
	// fix for the double-redeem risk described above (alongside the database
	// constraint itself) — a refreshed GET after this redirect never
	// resubmits the POST.
	http.Redirect(w, r, doneURL(deeplinkdomain.ActionRedeemReward), http.StatusSeeOther)
}

// respondRedeemError maps a redeem failure to an HTTP response.
func (h *WebHandlers) respondRedeemError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, tasksdomain.ErrRewardNotFound):
		http.NotFound(w, r)
	case errors.Is(err, tasksdomain.ErrDeepLinkAlreadyRedeemed):
		h.renderMessage(w, r, http.StatusConflict, "This code has already been used",
			"This QR code was already used to redeem a reward. Please rescan from the kiosk for a new one.")
	case errors.Is(err, tasksdomain.ErrInsufficientPoints):
		h.renderMessage(w, r, http.StatusConflict, "Not enough points yet", "You don't have enough points to redeem this reward yet.")
	case errors.Is(err, tasksdomain.ErrRewardOutOfStock):
		h.renderMessage(w, r, http.StatusConflict, "Out of stock", "This reward is out of stock right now.")
	default:
		h.logger.ErrorContext(r.Context(), "deeplink: redeem reward", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// Shared: signature verification + message rendering
// ---------------------------------------------------------------------------

// verifySignature reads the "exp"/"sig" query parameters, verifies them
// against path, and renders the friendly rescan page (returning ok=false) on
// any failure — a missing/malformed parameter, an invalid signature, or an
// expired link are all indistinguishable to the caller (AC3). On success it
// also returns the verified expiry and the signature's CANONICAL DECODED
// bytes (from Signer.Verify — never the presented query-string value), so a
// caller that needs to key off the link's own signature (confirmRedeem's
// replay guard) always keys off the unambiguous form — see
// deeplinkapp.Signer's decode doc for why the raw string is not safe to use
// as a key directly.
func (h *WebHandlers) verifySignature(w http.ResponseWriter, r *http.Request, path string) (exp int64, sigBytes []byte, ok bool) {
	expStr := r.URL.Query().Get("exp")
	sig := r.URL.Query().Get("sig")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || sig == "" {
		h.renderMessage(w, r, http.StatusBadRequest, rescanHeading, rescanMessage)
		return 0, nil, false
	}
	sigBytes, err = h.signer.Verify(path, exp, sig, h.now())
	if err != nil {
		h.renderMessage(w, r, http.StatusBadRequest, rescanHeading, rescanMessage)
		return 0, nil, false
	}
	return exp, sigBytes, true
}

// renderMessage renders the shared standalone message page at status.
func (h *WebHandlers) renderMessage(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	view := components.DeepLinkMessageView{Heading: heading, Message: message}
	if err := render.Render(r.Context(), w, status, components.DeepLinkMessagePage(view)); err != nil {
		h.logger.ErrorContext(r.Context(), "deeplink: render message page", "error", err)
	}
}
