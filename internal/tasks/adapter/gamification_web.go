package adapter

import (
	"errors"
	"fmt"
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
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	"github.com/ericfisherdev/nestova/internal/tasks/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// leaderboardWindowDays is the size of the rolling window used for the
// scoreboard. The leaderboard ranks members by points earned in the last 30
// days so it reflects recent effort rather than lifetime totals — this is
// intentionally different from the "Your Balance" card, which shows all-time
// spendable points (Balance is queried with no since filter). Keeping the two
// distinct lets the scoreboard stay a recency snapshot while spendable balance
// remains the member's full accumulated, not-yet-redeemed points.
const leaderboardWindowDays = 30

// streakLookbackDays is how far back CompletionDays queries task completions
// when computing streaks. A member cannot have a streak longer than this
// value; 366 days covers any year-long streak with a 1-day buffer.
const streakLookbackDays = 366

// pointHistoryLimit caps how many recent point ledger entries buildRewardsPage
// fetches for the current member (NES-118). A "Recent Activity" list is a
// scanning aid, not a full audit trail, so an unbounded query is unnecessary.
const pointHistoryLimit = 20

// GamificationWebHandlers holds the HTTP handler methods for the /rewards
// scoreboard + redemption page. All dependencies are injected via the
// constructor so this type is testable with fakes.
//
// SRP note: this is a separate handler type from WebHandlers (tasks list)
// because gamification has its own domain repositories and page shape. The two
// handler types share no state and are independently wired at the composition
// root.
type GamificationWebHandlers struct {
	ledger         domain.PointLedgerRepository
	rewards        domain.RewardRepository
	rewardSvc      *tasksapp.RewardService
	rewardAdminSvc *tasksapp.RewardAdminService
	instanceRepo   domain.TaskInstanceRepository
	households     household.HouseholdRepository
	sm             *scs.SessionManager
	logger         *slog.Logger
}

// NewGamificationWebHandlers constructs a GamificationWebHandlers with all
// required dependencies. It panics when any dependency is nil so
// misconfigured composition roots are caught at startup.
func NewGamificationWebHandlers(
	ledger domain.PointLedgerRepository,
	rewards domain.RewardRepository,
	rewardSvc *tasksapp.RewardService,
	rewardAdminSvc *tasksapp.RewardAdminService,
	instanceRepo domain.TaskInstanceRepository,
	households household.HouseholdRepository,
	sm *scs.SessionManager,
	logger *slog.Logger,
) *GamificationWebHandlers {
	if ledger == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil PointLedgerRepository")
	}
	if rewards == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil RewardRepository")
	}
	if rewardSvc == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil RewardService")
	}
	if rewardAdminSvc == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil RewardAdminService")
	}
	if instanceRepo == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil TaskInstanceRepository")
	}
	if households == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil HouseholdRepository")
	}
	if sm == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil session manager")
	}
	if logger == nil {
		panic("tasks/adapter: NewGamificationWebHandlers requires a non-nil logger")
	}
	return &GamificationWebHandlers{
		ledger:         ledger,
		rewards:        rewards,
		rewardSvc:      rewardSvc,
		rewardAdminSvc: rewardAdminSvc,
		instanceRepo:   instanceRepo,
		households:     households,
		sm:             sm,
		logger:         logger,
	}
}

// RewardsPage handles GET /rewards. It renders the scoreboard + rewards catalog
// for the authenticated member.
//
// View-model construction (N+1 avoidance):
//   - One ListMembers call.
//   - One Leaderboard call (all-time, a single GROUP BY query).
//   - One Balance call for the current member.
//   - One ListActiveRewards call.
//   - One CompletionDays call per household member to compute streaks. This is
//     O(members) round-trips but is acceptable: households are small (typically
//     2–6 members) and each query is cheap (indexed range scan on
//     point_ledger_household_member_idx).
//
// The layout callback is supplied by the caller (home.go) so this handler
// stays decoupled from the ShellProps / nav construction that depends on the
// request and household repository.
func (h *GamificationWebHandlers) RewardsPage(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		page, err := h.buildRewardsPage(r, member, "")
		if err != nil {
			h.logger.ErrorContext(r.Context(), "rewards page: build page", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		content := components.RewardsPageComponent(page)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "rewards page: render", "error", err)
		}
	}
}

// Redeem handles POST /rewards/{id}/redeem. It verifies the CSRF token, parses
// the reward id from the path, delegates to RewardService.Redeem, and on
// success redirects back to /rewards.
//
// Error mapping:
//   - Bad CSRF                     → 403
//   - Malformed reward id          → 400
//   - ErrRewardNotFound            → 404
//   - ErrInsufficientPoints        → 409 (re-render /rewards with a message)
//   - Other                        → 500
func (h *GamificationWebHandlers) Redeem(layoutFn LayoutFunc) http.HandlerFunc {
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

		rawID := r.PathValue("id")
		rewardID, err := domain.ParseRewardID(rawID)
		if err != nil {
			http.Error(w, "invalid reward id", http.StatusBadRequest)
			return
		}

		_, err = h.rewardSvc.Redeem(r.Context(), member.HouseholdID, member.ID, rewardID)
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrRewardNotFound):
				http.Error(w, "reward not found", http.StatusNotFound)
			case errors.Is(err, domain.ErrInsufficientPoints):
				// Re-render the rewards page with a user-facing message at 409 so
				// the member understands why the action was rejected.
				page, buildErr := h.buildRewardsPage(r, member, "You don't have enough points to redeem this reward.")
				if buildErr != nil {
					h.logger.ErrorContext(r.Context(), "rewards redeem: rebuild page on insufficient", "error", buildErr)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
				content := components.RewardsPageComponent(page)
				if renderErr := render.Render(r.Context(), w, http.StatusConflict, layoutFn(member)(content)); renderErr != nil {
					h.logger.ErrorContext(r.Context(), "rewards redeem: render insufficient page", "error", renderErr)
				}
			default:
				h.logger.ErrorContext(r.Context(), "rewards redeem: service error",
					"reward_id", rawID,
					"error", err,
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}

		if render.IsHTMX(r) {
			w.Header().Set("HX-Redirect", "/rewards")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/rewards", http.StatusSeeOther)
	}
}

// buildRewardsPage assembles the RewardsPage view model. insufficientMessage
// is passed through to the page when a prior redeem attempt was rejected.
func (h *GamificationWebHandlers) buildRewardsPage(
	r *http.Request,
	member *household.Member,
	insufficientMessage string,
) (components.RewardsPage, error) {
	ctx := r.Context()

	// Compute every time-window boundary from a single UTC instant so the
	// leaderboard window, the streak lookback, and today's anchor are mutually
	// consistent and bucketed on UTC days (NES-37 rule) on any server timezone.
	now := time.Now().UTC()

	// Fetch all household members once; used for leaderboard display and streak
	// computation.
	members, err := h.households.ListMembers(ctx, member.HouseholdID)
	if err != nil {
		return components.RewardsPage{}, err
	}
	memberByID := make(map[household.MemberID]*household.Member, len(members))
	for _, m := range members {
		memberByID[m.ID] = m
	}

	// Rolling 30-day leaderboard (single GROUP BY query). This is a recency
	// snapshot, intentionally distinct from the all-time spendable balance below.
	leaderboardSince := now.AddDate(0, 0, -leaderboardWindowDays)
	leaderPts, err := h.ledger.Leaderboard(ctx, member.HouseholdID, leaderboardSince)
	if err != nil {
		return components.RewardsPage{}, err
	}

	// Build a leaderboard entry for every household member. Members with no
	// points are included at 0 so the scoreboard is always complete.
	// The leaderboard result is already ordered by points desc; members with no
	// ledger entries are appended at the end, sorted by display name for stability.
	leaderMap := make(map[household.MemberID]int, len(leaderPts))
	for _, mp := range leaderPts {
		leaderMap[mp.MemberID] = mp.Points
	}

	// Compute per-member streaks (one CompletionDays query per member). Both the
	// lookback cutoff and today's anchor derive from the same UTC now so streak
	// boundaries are consistent with the leaderboard window.
	since := now.AddDate(0, 0, -streakLookbackDays)
	today := domain.DateOf(now)
	streakMap := make(map[household.MemberID]int, len(members))
	for _, m := range members {
		days, err := h.instanceRepo.CompletionDays(ctx, member.HouseholdID, m.ID, since)
		if err != nil {
			return components.RewardsPage{}, err
		}
		streakMap[m.ID] = domain.CurrentStreak(days, today)
	}

	// Build the full leaderboard rows (all members, not just those with points).
	rows := buildLeaderboardRows(members, leaderMap, streakMap, member.ID)

	// Current member's balance.
	balance, err := h.ledger.Balance(ctx, member.HouseholdID, member.ID)
	if err != nil {
		return components.RewardsPage{}, err
	}

	// Storefront rewards catalogue (NES-126 AC2): active AND in-stock only —
	// an archived or sold-out reward is never returned by ListStorefrontRewards,
	// so the template need not re-check either condition.
	storefront, err := h.rewards.ListStorefrontRewards(ctx, member.HouseholdID)
	if err != nil {
		return components.RewardsPage{}, err
	}
	rewardItems := make([]components.RewardItem, 0, len(storefront))
	for _, sr := range storefront {
		rewardItems = append(rewardItems, buildRewardItem(sr, balance))
	}

	// Current member's recent point ledger activity (NES-118), enriched with a
	// human-readable reason so the template never branches on SourceType.
	history, err := h.ledger.History(ctx, member.HouseholdID, member.ID, pointHistoryLimit)
	if err != nil {
		return components.RewardsPage{}, err
	}
	historyRows := make([]components.PointHistoryRow, 0, len(history))
	for _, entry := range history {
		historyRows = append(historyRows, components.PointHistoryRow{
			Reason:    historyReason(entry),
			Points:    entry.Points,
			CreatedAt: entry.CreatedAt.Format("Jan 2"),
		})
	}

	return components.RewardsPage{
		Leaderboard:         rows,
		Balance:             balance,
		Rewards:             rewardItems,
		History:             historyRows,
		CSRFToken:           authadapter.GetCSRFToken(r.Context(), h.sm),
		InsufficientMessage: insufficientMessage,
		// CanManageRewards gates the "Manage rewards" link to parents (owner or
		// adult), mirroring TradeSections.CanViewHistory's role gate (NES-122).
		CanManageRewards: isParent(member),
	}, nil
}

// buildRewardItem maps one storefront read model to its RewardItem view,
// computing the affordability badge and stock label the template renders
// verbatim (NES-126 AC3): NeedMore is the positive point shortfall when the
// member cannot yet afford the reward, and StockLabel is empty for unlimited
// stock or "N left" when a cap is set.
func buildRewardItem(sr domain.StorefrontReward, balance int) components.RewardItem {
	rw := sr.Reward
	affordable := balance >= rw.CostPoints
	needMore := 0
	if !affordable {
		needMore = rw.CostPoints - balance
	}
	stockLabel := ""
	if sr.RemainingStock != nil {
		stockLabel = fmt.Sprintf("%d left", *sr.RemainingStock)
	}
	imageRef := ""
	if rw.ImageRef != nil {
		imageRef = *rw.ImageRef
	}
	return components.RewardItem{
		ID:          rw.ID.String(),
		Name:        rw.Name,
		Description: rw.Description,
		ImageRef:    imageRef,
		CostPoints:  rw.CostPoints,
		Affordable:  affordable,
		NeedMore:    needMore,
		StockLabel:  stockLabel,
	}
}

// historyReason builds a human-readable label for a point ledger entry based
// on its source (NES-118), mirroring reminders.categoryLabel's simplicity: a
// small switch over the known source types, falling back to a generic label
// for a manual adjustment or any other source_type this handler does not yet
// describe.
func historyReason(entry domain.PointHistoryEntry) string {
	switch entry.SourceType {
	case "task_instance":
		if entry.TaskTitle != "" {
			return fmt.Sprintf("Completed: %s", entry.TaskTitle)
		}
		return "Task completed"
	case domain.SourceTypeClaimExpiry:
		if entry.TaskTitle != "" {
			return fmt.Sprintf("Claim expired: %s", entry.TaskTitle)
		}
		return "Claim expired"
	case "redemption":
		if entry.RewardName != "" {
			return fmt.Sprintf("Redeemed: %s", entry.RewardName)
		}
		return "Reward redeemed"
	default:
		return "Points adjustment"
	}
}

// buildLeaderboardRows constructs the ordered leaderboard view-model slice.
// Every member in the household is represented; members with no ledger entries
// receive 0 points. The slice is ordered: members with points are sorted by
// points descending (matching the Leaderboard query order), then members with
// zero points are appended alphabetically.
func buildLeaderboardRows(
	members []*household.Member,
	leaderMap map[household.MemberID]int,
	streakMap map[household.MemberID]int,
	currentMemberID household.MemberID,
) []components.LeaderboardRow {
	type entry struct {
		row    components.LeaderboardRow
		points int
	}
	entries := make([]entry, 0, len(members))
	for _, m := range members {
		pts := leaderMap[m.ID]
		entries = append(entries, entry{
			row: components.LeaderboardRow{
				MemberID:        m.ID.String(),
				Name:            m.DisplayName,
				Color:           m.Color.String(),
				Points:          pts,
				Streak:          streakMap[m.ID],
				IsCurrentMember: m.ID == currentMemberID,
			},
			points: pts,
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].points != entries[j].points {
			return entries[i].points > entries[j].points
		}
		// Stable secondary sort: display name asc, then member id as tiebreaker.
		if entries[i].row.Name != entries[j].row.Name {
			return entries[i].row.Name < entries[j].row.Name
		}
		return entries[i].row.MemberID < entries[j].row.MemberID
	})

	rows := make([]components.LeaderboardRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, e.row)
	}
	return rows
}

// ---------------------------------------------------------------------------
// Reward admin (NES-126) — parent-only create/edit/archive.
//
// Every handler below re-checks isParent (defined in trade_web.go, shared by
// this package) after the RequireMember auth gate: a child member who is
// authenticated must still be refused with 403, mirroring
// TradeWebHandlers.HistoryPage's exact role-gate shape (NES-122).
// ---------------------------------------------------------------------------

// RewardsAdminPage handles GET /admin/rewards. It lists every reward in the
// household — active and archived — for the parent-only catalogue admin
// (NES-126 AC1).
func (h *GamificationWebHandlers) RewardsAdminPage(layoutFn LayoutFunc) http.HandlerFunc {
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

		rewards, err := h.rewards.ListAllRewards(r.Context(), member.HouseholdID)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "rewards admin: list all rewards", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		page := components.RewardAdminListPage{
			Rewards:   toRewardAdminListItems(rewards),
			CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm),
		}
		content := components.RewardsAdminPage(page)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "rewards admin: render list", "error", err)
		}
	}
}

// NewRewardPage handles GET /admin/rewards/new. It renders a blank
// create-reward form for a parent (NES-126 AC1).
func (h *GamificationWebHandlers) NewRewardPage(layoutFn LayoutFunc) http.HandlerFunc {
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

		form := components.RewardAdminForm{CSRFToken: authadapter.GetCSRFToken(r.Context(), h.sm)}
		content := components.RewardAdminFormPage(form)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "new reward page: render", "error", err)
		}
	}
}

// CreateReward handles POST /admin/rewards. It parses and validates the
// form, delegates to RewardAdminService.Create, and on success redirects to
// the admin list. On validation failure it re-renders the form at HTTP 422
// with the submitted values preserved and an error message (NES-126 AC1).
//
// Error mapping:
//   - bad CSRF                        → 403
//   - not a parent (owner/adult)      → 403
//   - missing/invalid name/cost/qty   → 422 (form re-render)
//   - other                           → 500
func (h *GamificationWebHandlers) CreateReward(layoutFn LayoutFunc) http.HandlerFunc {
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
		if !isParent(member) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		name, description, cost, imageRef, quantity, form, validationMsg := parseRewardAdminForm(r, h.sm, "")
		if validationMsg != "" {
			h.renderRewardAdminForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
			return
		}

		if _, err := h.rewardAdminSvc.Create(r.Context(), member.HouseholdID, name, description, cost, imageRef, quantity); err != nil {
			errMsg := rewardAdminErrMessage(err)
			if errMsg == "" {
				h.logger.ErrorContext(r.Context(), "create reward: service error", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			form.Error = errMsg
			h.renderRewardAdminForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
			return
		}

		http.Redirect(w, r, "/admin/rewards", http.StatusSeeOther)
	}
}

// EditRewardPage handles GET /admin/rewards/{id}/edit. It loads the reward
// (tenant-checked) and renders the form pre-filled with its current values
// (NES-126 AC1).
//
// Error mapping:
//   - malformed reward id   → 400
//   - ErrRewardNotFound     → 404
//   - other                 → 500
func (h *GamificationWebHandlers) EditRewardPage(layoutFn LayoutFunc) http.HandlerFunc {
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

		id, err := domain.ParseRewardID(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid reward id", http.StatusBadRequest)
			return
		}

		reward, err := h.rewards.GetReward(r.Context(), member.HouseholdID, id)
		if err != nil {
			if errors.Is(err, domain.ErrRewardNotFound) {
				http.Error(w, "reward not found", http.StatusNotFound)
				return
			}
			h.logger.ErrorContext(r.Context(), "edit reward page: get reward", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		form := rewardToForm(reward, authadapter.GetCSRFToken(r.Context(), h.sm))
		content := components.RewardAdminFormPage(form)
		if err := render.Page(r.Context(), w, r, layoutFn(member), content); err != nil {
			h.logger.ErrorContext(r.Context(), "edit reward page: render", "error", err)
		}
	}
}

// UpdateReward handles POST /admin/rewards/{id}. It parses and validates the
// form, delegates to RewardAdminService.Update, and on success redirects to
// the admin list (NES-126 AC1).
//
// Error mapping:
//   - bad CSRF                        → 403
//   - not a parent (owner/adult)      → 403
//   - malformed reward id             → 400
//   - missing/invalid name/cost/qty   → 422 (form re-render)
//   - ErrRewardNotFound                → 404
//   - other                           → 500
func (h *GamificationWebHandlers) UpdateReward(layoutFn LayoutFunc) http.HandlerFunc {
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
		if !isParent(member) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		rawID := r.PathValue("id")
		id, err := domain.ParseRewardID(rawID)
		if err != nil {
			http.Error(w, "invalid reward id", http.StatusBadRequest)
			return
		}

		name, description, cost, imageRef, quantity, form, validationMsg := parseRewardAdminForm(r, h.sm, rawID)
		if validationMsg != "" {
			h.renderRewardAdminForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
			return
		}

		if _, err := h.rewardAdminSvc.Update(r.Context(), member.HouseholdID, id, name, description, cost, imageRef, quantity); err != nil {
			if errors.Is(err, domain.ErrRewardNotFound) {
				http.Error(w, "reward not found", http.StatusNotFound)
				return
			}
			errMsg := rewardAdminErrMessage(err)
			if errMsg == "" {
				h.logger.ErrorContext(r.Context(), "update reward: service error", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			form.Error = errMsg
			h.renderRewardAdminForm(w, r, http.StatusUnprocessableEntity, form, layoutFn(member))
			return
		}

		http.Redirect(w, r, "/admin/rewards", http.StatusSeeOther)
	}
}

// ArchiveReward handles POST /admin/rewards/{id}/archive. It retires the
// reward from the storefront without touching its redemption history
// (NES-126 AC1, AC5 — archive is the only removal action exposed to parents;
// see domain.RewardRepository.DeleteReward's doc for the hard-delete guard).
//
// Error mapping:
//   - bad CSRF                   → 403
//   - not a parent (owner/adult) → 403
//   - malformed reward id        → 400
//   - ErrRewardNotFound          → 404
//   - other                      → 500
func (h *GamificationWebHandlers) ArchiveReward(w http.ResponseWriter, r *http.Request) {
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
	if !isParent(member) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	id, err := domain.ParseRewardID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid reward id", http.StatusBadRequest)
		return
	}

	if err := h.rewardAdminSvc.Archive(r.Context(), member.HouseholdID, id); err != nil {
		if errors.Is(err, domain.ErrRewardNotFound) {
			http.Error(w, "reward not found", http.StatusNotFound)
			return
		}
		h.logger.ErrorContext(r.Context(), "archive reward: service error", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/admin/rewards", http.StatusSeeOther)
}

// renderRewardAdminForm renders the create/edit reward form component at the
// given HTTP status, mirroring WebHandlers.renderNewTaskForm's
// render-at-status precedent.
func (h *GamificationWebHandlers) renderRewardAdminForm(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	form components.RewardAdminForm,
	layout func(templ.Component) templ.Component,
) {
	content := components.RewardAdminFormPage(form)
	if err := render.Render(r.Context(), w, status, layout(content)); err != nil {
		h.logger.ErrorContext(r.Context(), "reward admin: render form", "error", err)
	}
}

// parseRewardAdminForm reads the submitted form values from r, validates them
// locally (before the service is ever called — the service re-validates the
// same rules and is the source of truth, but a local check gives a friendlier
// message without a round trip), and returns a sticky RewardAdminForm for
// re-rendering. editID is "" for the create form and the reward's string id
// for the edit form; it only affects the returned form's ID/IsEdit fields.
// The parsed name/description/cost/imageRef/quantity are only valid when
// validationMsg is empty.
func parseRewardAdminForm(
	r *http.Request,
	sm *scs.SessionManager,
	editID string,
) (name, description string, costPoints int, imageRef *string, quantityAvailable *int, form components.RewardAdminForm, validationMsg string) {
	rawName := strings.TrimSpace(r.FormValue("name"))
	rawDescription := strings.TrimSpace(r.FormValue("description"))
	rawCost := r.FormValue("cost_points")
	rawImageRef := strings.TrimSpace(r.FormValue("image_ref"))
	rawQuantity := strings.TrimSpace(r.FormValue("quantity_available"))

	form = components.RewardAdminForm{
		CSRFToken:         authadapter.GetCSRFToken(r.Context(), sm),
		ID:                editID,
		IsEdit:            editID != "",
		Name:              rawName,
		Description:       rawDescription,
		CostPoints:        rawCost,
		ImageRef:          rawImageRef,
		QuantityAvailable: rawQuantity,
	}

	if rawName == "" {
		form.Error = "Name is required."
		return "", "", 0, nil, nil, form, form.Error
	}

	cost, err := strconv.Atoi(rawCost)
	if err != nil || cost <= 0 {
		form.Error = "Cost must be a positive number of points."
		return "", "", 0, nil, nil, form, form.Error
	}

	var imageRefPtr *string
	if rawImageRef != "" {
		imageRefPtr = &rawImageRef
	}

	var quantityPtr *int
	if rawQuantity != "" {
		q, err := strconv.Atoi(rawQuantity)
		if err != nil || q < 0 {
			form.Error = "Quantity available must be zero or greater, or left blank for unlimited."
			return "", "", 0, nil, nil, form, form.Error
		}
		quantityPtr = &q
	}

	return rawName, rawDescription, cost, imageRefPtr, quantityPtr, form, ""
}

// rewardAdminErrMessage maps a RewardAdminService validation error to a
// user-readable message. An empty string means the error is unexpected and
// should be treated as a 500, mirroring createTaskErrMessage's precedent.
func rewardAdminErrMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrInvalidRewardName):
		return "Name is required."
	case errors.Is(err, domain.ErrInvalidRewardCost):
		return "Cost must be a positive number of points."
	case errors.Is(err, domain.ErrInvalidRewardQuantity):
		return "Quantity available must be zero or greater, or left blank for unlimited."
	default:
		return ""
	}
}

// rewardToForm maps a persisted reward to a sticky RewardAdminForm for the
// edit page's initial (non-error) render.
func rewardToForm(reward *domain.Reward, csrfToken string) components.RewardAdminForm {
	imageRef := ""
	if reward.ImageRef != nil {
		imageRef = *reward.ImageRef
	}
	quantity := ""
	if reward.QuantityAvailable != nil {
		quantity = strconv.Itoa(*reward.QuantityAvailable)
	}
	return components.RewardAdminForm{
		CSRFToken:         csrfToken,
		ID:                reward.ID.String(),
		IsEdit:            true,
		Name:              reward.Name,
		Description:       reward.Description,
		CostPoints:        strconv.Itoa(reward.CostPoints),
		ImageRef:          imageRef,
		QuantityAvailable: quantity,
	}
}

// toRewardAdminListItems maps every reward in the household (active and
// archived) to its admin list-row view model.
func toRewardAdminListItems(rewards []*domain.Reward) []components.RewardAdminListItem {
	items := make([]components.RewardAdminListItem, 0, len(rewards))
	for _, rw := range rewards {
		imageRef := ""
		if rw.ImageRef != nil {
			imageRef = *rw.ImageRef
		}
		items = append(items, components.RewardAdminListItem{
			ID:          rw.ID.String(),
			Name:        rw.Name,
			Description: rw.Description,
			ImageRef:    imageRef,
			CostPoints:  rw.CostPoints,
			StockLabel:  adminStockLabel(rw.QuantityAvailable),
			Active:      rw.Active,
		})
	}
	return items
}

// adminStockLabel renders a reward's configured cap for the admin list —
// distinct from the storefront's remaining-stock label, since the admin view
// shows the cap itself (what the parent set), not what's left after
// redemptions.
func adminStockLabel(quantityAvailable *int) string {
	if quantityAvailable == nil {
		return "Unlimited"
	}
	return fmt.Sprintf("%d available", *quantityAvailable)
}
