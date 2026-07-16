package adapter

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

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
	ledger       domain.PointLedgerRepository
	rewards      domain.RewardRepository
	rewardSvc    *tasksapp.RewardService
	instanceRepo domain.TaskInstanceRepository
	households   household.HouseholdRepository
	sm           *scs.SessionManager
	logger       *slog.Logger
}

// NewGamificationWebHandlers constructs a GamificationWebHandlers with all
// required dependencies. It panics when any dependency is nil so
// misconfigured composition roots are caught at startup.
func NewGamificationWebHandlers(
	ledger domain.PointLedgerRepository,
	rewards domain.RewardRepository,
	rewardSvc *tasksapp.RewardService,
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
		ledger:       ledger,
		rewards:      rewards,
		rewardSvc:    rewardSvc,
		instanceRepo: instanceRepo,
		households:   households,
		sm:           sm,
		logger:       logger,
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

	// Active rewards catalogue.
	activeRewards, err := h.rewards.ListActiveRewards(ctx, member.HouseholdID)
	if err != nil {
		return components.RewardsPage{}, err
	}
	rewardItems := make([]components.RewardItem, 0, len(activeRewards))
	for _, rw := range activeRewards {
		rewardItems = append(rewardItems, components.RewardItem{
			ID:         rw.ID.String(),
			Name:       rw.Name,
			CostPoints: rw.CostPoints,
			Affordable: balance >= rw.CostPoints,
		})
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
	}, nil
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
