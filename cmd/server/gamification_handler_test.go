package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksadapter "github.com/ericfisherdev/nestova/internal/tasks/adapter"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// configurable gamification fakes
// ---------------------------------------------------------------------------

// configurableRewardRepo is a RewardRedeemer whose behaviour is controlled per-
// test. It satisfies both domain.RewardRepository and tasksapp.RewardRedeemer.
type configurableRewardRepo struct {
	reward      *tasksdomain.Reward
	getErr      error
	redeemErr   error
	redeemCalls int
	// archiveErr overrides ArchiveReward's return when non-nil (NES-126).
	archiveErr error
	// createCalls and updateCalls record every reward passed to CreateReward
	// and UpdateReward respectively, so a test can assert on what the admin
	// service persisted without a database (NES-126).
	createCalls []*tasksdomain.Reward
	updateCalls []*tasksdomain.Reward
	// archiveCalls records every (householdID, rewardID) pair passed to
	// ArchiveReward, so a test can assert both the call count AND that the
	// correct household/reward were targeted, not just a non-zero count
	// (NES-126, CodeRabbit finding).
	archiveCalls []archiveRewardCall

	// pendingRedemptions and memberRedemptions are returned by
	// ListPendingRedemptions and ListMemberRedemptions respectively (NES-127).
	pendingRedemptions []tasksdomain.RedemptionDetail
	memberRedemptions  []tasksdomain.RedemptionDetail
	// resolvedRedemption is returned by Fulfill/Deny/Cancel on success; its
	// Status field is left to the test to set appropriately for the action
	// under test.
	resolvedRedemption tasksdomain.ResolvedRedemption
	// fulfillErr/denyErr/cancelErr override Fulfill/Deny/Cancel's error
	// return when non-nil (NES-127).
	fulfillErr, denyErr, cancelErr error
	// fulfillCalls records every redemption id passed to Fulfill, mirroring
	// archiveCalls' call-recording precedent.
	fulfillCalls []tasksdomain.RewardRedemptionID
	// denyCalls and cancelCalls record every Deny/Cancel invocation's full
	// argument set — not just the redemption id — so a test can assert the
	// handler passed through the submitted reason (Deny) or the
	// AUTHENTICATED member id (Cancel), not merely that the method was
	// called (NES-127, CodeRabbit finding).
	denyCalls   []denyCall
	cancelCalls []cancelCall
}

// archiveRewardCall records one ArchiveReward invocation's arguments.
type archiveRewardCall struct {
	householdID household.HouseholdID
	rewardID    tasksdomain.RewardID
}

// denyCall records one Deny invocation's arguments.
type denyCall struct {
	id     tasksdomain.RewardRedemptionID
	reason string
}

// cancelCall records one Cancel invocation's arguments.
type cancelCall struct {
	id       tasksdomain.RewardRedemptionID
	memberID household.MemberID
}

func (r *configurableRewardRepo) CreateReward(_ context.Context, reward *tasksdomain.Reward) error {
	r.createCalls = append(r.createCalls, reward)
	return nil
}

func (r *configurableRewardRepo) GetReward(
	_ context.Context,
	_ household.HouseholdID,
	_ tasksdomain.RewardID,
) (*tasksdomain.Reward, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.reward, nil
}

func (r *configurableRewardRepo) ListActiveRewards(
	_ context.Context,
	_ household.HouseholdID,
) ([]*tasksdomain.Reward, error) {
	if r.reward != nil {
		return []*tasksdomain.Reward{r.reward}, nil
	}
	return nil, nil
}

func (r *configurableRewardRepo) ListStorefrontRewards(
	_ context.Context,
	_ household.HouseholdID,
) ([]tasksdomain.StorefrontReward, error) {
	if r.reward != nil {
		return []tasksdomain.StorefrontReward{{Reward: r.reward, RemainingStock: r.reward.QuantityAvailable}}, nil
	}
	return nil, nil
}

func (r *configurableRewardRepo) ListAllRewards(
	_ context.Context,
	_ household.HouseholdID,
) ([]*tasksdomain.Reward, error) {
	if r.reward != nil {
		return []*tasksdomain.Reward{r.reward}, nil
	}
	return nil, nil
}

func (r *configurableRewardRepo) UpdateReward(_ context.Context, reward *tasksdomain.Reward) error {
	r.updateCalls = append(r.updateCalls, reward)
	return nil
}

func (r *configurableRewardRepo) ArchiveReward(
	_ context.Context,
	householdID household.HouseholdID,
	rewardID tasksdomain.RewardID,
) error {
	r.archiveCalls = append(r.archiveCalls, archiveRewardCall{householdID: householdID, rewardID: rewardID})
	return r.archiveErr
}

func (r *configurableRewardRepo) DeleteReward(
	_ context.Context,
	_ household.HouseholdID,
	_ tasksdomain.RewardID,
) error {
	return nil
}

func (r *configurableRewardRepo) Redeem(_ context.Context, _ *tasksdomain.RewardRedemption) error {
	return nil
}

func (r *configurableRewardRepo) RedeemWithDebit(
	_ context.Context,
	_ *tasksdomain.RewardRedemption,
) (int, error) {
	r.redeemCalls++
	if r.redeemErr != nil {
		return 0, r.redeemErr
	}
	if r.reward != nil {
		return r.reward.CostPoints, nil
	}
	return 0, nil
}

func (r *configurableRewardRepo) ListPendingRedemptions(
	_ context.Context,
	_ household.HouseholdID,
) ([]tasksdomain.RedemptionDetail, error) {
	return r.pendingRedemptions, nil
}

func (r *configurableRewardRepo) ListMemberRedemptions(
	_ context.Context,
	_ household.HouseholdID,
	_ household.MemberID,
	_ int,
) ([]tasksdomain.RedemptionDetail, error) {
	return r.memberRedemptions, nil
}

func (r *configurableRewardRepo) Fulfill(
	_ context.Context,
	_ household.HouseholdID,
	id tasksdomain.RewardRedemptionID,
) (tasksdomain.ResolvedRedemption, error) {
	r.fulfillCalls = append(r.fulfillCalls, id)
	if r.fulfillErr != nil {
		return tasksdomain.ResolvedRedemption{}, r.fulfillErr
	}
	return r.resolvedRedemption, nil
}

func (r *configurableRewardRepo) Deny(
	_ context.Context,
	_ household.HouseholdID,
	id tasksdomain.RewardRedemptionID,
	reason string,
) (tasksdomain.ResolvedRedemption, error) {
	r.denyCalls = append(r.denyCalls, denyCall{id: id, reason: reason})
	if r.denyErr != nil {
		return tasksdomain.ResolvedRedemption{}, r.denyErr
	}
	return r.resolvedRedemption, nil
}

func (r *configurableRewardRepo) Cancel(
	_ context.Context,
	_ household.HouseholdID,
	id tasksdomain.RewardRedemptionID,
	memberID household.MemberID,
) (tasksdomain.ResolvedRedemption, error) {
	r.cancelCalls = append(r.cancelCalls, cancelCall{id: id, memberID: memberID})
	if r.cancelErr != nil {
		return tasksdomain.ResolvedRedemption{}, r.cancelErr
	}
	return r.resolvedRedemption, nil
}

// Compile-time assertions.
var (
	_ tasksdomain.RewardRepository = (*configurableRewardRepo)(nil)
	_ tasksapp.RewardRedeemer      = (*configurableRewardRepo)(nil)
	_ tasksapp.RedemptionFulfiller = (*configurableRewardRepo)(nil)
)

// configurableLedgerRepo is a PointLedgerRepository whose Balance is fixed
// per test, used to exercise the storefront's affordability badge (NES-126
// AC3) without a database.
type configurableLedgerRepo struct {
	balance int
}

func (r configurableLedgerRepo) Append(_ context.Context, _ *tasksdomain.PointEntry) error {
	return nil
}

func (r configurableLedgerRepo) Balance(
	_ context.Context,
	_ household.HouseholdID,
	_ household.MemberID,
) (int, error) {
	return r.balance, nil
}

func (r configurableLedgerRepo) Leaderboard(
	_ context.Context,
	_ household.HouseholdID,
	_ time.Time,
) ([]tasksdomain.MemberPoints, error) {
	return nil, nil
}

func (r configurableLedgerRepo) History(
	_ context.Context,
	_ household.HouseholdID,
	_ household.MemberID,
	_ int,
) ([]tasksdomain.PointHistoryEntry, error) {
	return nil, nil
}

// Compile-time assertion.
var _ tasksdomain.PointLedgerRepository = configurableLedgerRepo{}

// ---------------------------------------------------------------------------
// Test handler builder
// ---------------------------------------------------------------------------

// buildGamificationTestHandler wires a full http.Handler using the supplied
// reward repo and an authedHouseholdRepo so the gamification routes are
// exercisable under an authenticated session. The point ledger is a fixed
// zero-balance fake; use buildGamificationTestHandlerWithLedger for tests
// that need a configurable balance (NES-126 AC3).
func buildGamificationTestHandler(
	rewardRepo *configurableRewardRepo,
	member *household.Member,
) (http.Handler, *scs.SessionManager) {
	return buildGamificationTestHandlerWithLedger(rewardRepo, fakePointLedgerRepo{}, member)
}

// buildGamificationTestHandlerWithLedger is buildGamificationTestHandler with
// an injectable point ledger repo, so a test can control the current
// member's balance and assert on the storefront's computed affordability
// badge (NES-126 AC3).
func buildGamificationTestHandlerWithLedger(
	rewardRepo *configurableRewardRepo,
	ledgerRepo tasksdomain.PointLedgerRepository,
	member *household.Member,
) (http.Handler, *scs.SessionManager) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}
	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, logger)
	onboardingHandlers := authadapter.NewOnboardingHandlers(
		householdRepo, testCredStore{}, testProvisioner{}, sm, logger,
	)

	recurringRepo := fakeRecurringTaskRepo{}
	instanceRepo := &fakeTaskInstanceRepo{}
	taskService, err := tasksapp.NewTaskService(recurringRepo, instanceRepo)
	if err != nil {
		panic("buildGamificationTestHandlerWithLedger: " + err.Error())
	}
	taskWebHandlers := tasksadapter.NewWebHandlers(
		taskService, recurringRepo, instanceRepo, householdRepo, sm, logger,
	)

	rewardSvc := tasksapp.NewRewardService(rewardRepo, householdRepo, fakeEnqueuer{}, logger)
	rewardAdminSvc := tasksapp.NewRewardAdminService(rewardRepo, logger)
	redemptionSvc, err := tasksapp.NewRedemptionService(rewardRepo, fakeEnqueuer{}, logger)
	if err != nil {
		panic("buildGamificationTestHandlerWithLedger: " + err.Error())
	}
	gamificationHandlers := tasksadapter.NewGamificationWebHandlers(
		ledgerRepo,
		rewardRepo,
		rewardSvc,
		rewardAdminSvc,
		redemptionSvc,
		instanceRepo,
		householdRepo,
		sm,
		logger,
	)

	groceryHandlers := newTestGroceryHandlers(householdRepo, sm, logger)

	mux := http.NewServeMux()
	registerWebRoutes(mux, logger, sm, authHandlers, onboardingHandlers, householdRepo, taskWebHandlers, newTestTradeHandlers(taskWebHandlers, instanceRepo, householdRepo, sm, logger), gamificationHandlers, groceryHandlers, newTestMealsHandlers(sm, logger), newTestCalendarHandlers(sm, logger))

	return sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	), sm
}

// testMember returns a minimal Member suitable for seeding an authed session.
func testMember() *household.Member {
	return &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "TestUser",
		Color:       household.ColorSage,
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /rewards — auth guard
// ---------------------------------------------------------------------------

func TestRewardsPageRequiresAuth(t *testing.T) {
	handler, _ := buildGamificationTestHandler(&configurableRewardRepo{}, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rewards", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET /rewards: status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want /login...", loc)
	}
}

func TestRewardsPageHTMXRequiresAuth(t *testing.T) {
	handler, _ := buildGamificationTestHandler(&configurableRewardRepo{}, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
	req.Header.Set("HX-Request", "true")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HX GET /rewards: status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /rewards — authenticated renders page
// ---------------------------------------------------------------------------

func TestRewardsPageRendersForAuthedMember(t *testing.T) {
	member := testMember()
	reward := &tasksdomain.Reward{
		ID:          tasksdomain.NewRewardID(),
		HouseholdID: member.HouseholdID,
		Name:        "Movie night",
		CostPoints:  50,
		Active:      true,
	}
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{reward: reward}, member)

	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated GET /rewards: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Movie night") {
		t.Errorf("rewards page missing reward name: %q", body)
	}
	if !strings.Contains(body, "Your Balance") {
		t.Errorf("rewards page missing balance section: %q", body)
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /rewards/{id}/redeem — CSRF guard
// ---------------------------------------------------------------------------

func TestRedeemRejectsBadCSRF(t *testing.T) {
	member := testMember()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, member)

	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())
	fakeID := tasksdomain.NewRewardID().String()

	req := httptest.NewRequest(
		http.MethodPost,
		"/rewards/"+fakeID+"/redeem",
		strings.NewReader("csrf_token=wrong-token"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /rewards/.../redeem with wrong CSRF: status = %d, want 403", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /rewards/{id}/redeem — success redirects
// ---------------------------------------------------------------------------

func TestRedeemSuccessRedirects(t *testing.T) {
	member := testMember()
	reward := &tasksdomain.Reward{
		ID:          tasksdomain.NewRewardID(),
		HouseholdID: member.HouseholdID,
		Name:        "Redeem me",
		CostPoints:  10,
		Active:      true,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	// No redeemErr → success.
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{reward: reward}, member)

	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	body := strings.NewReader("csrf_token=" + csrfToken)
	req := httptest.NewRequest(
		http.MethodPost,
		"/rewards/"+reward.ID.String()+"/redeem",
		body,
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful redeem: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/rewards" {
		t.Errorf("Location = %q, want /rewards", loc)
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /rewards/{id}/redeem — ErrInsufficientPoints → 409
// ---------------------------------------------------------------------------

func TestRedeemInsufficientPointsReturns409(t *testing.T) {
	member := testMember()
	reward := &tasksdomain.Reward{
		ID:          tasksdomain.NewRewardID(),
		HouseholdID: member.HouseholdID,
		Name:        "Too expensive",
		CostPoints:  9999,
		Active:      true,
	}
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{
		reward:    reward,
		redeemErr: tasksdomain.ErrInsufficientPoints,
	}, member)

	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	reqBody := strings.NewReader("csrf_token=" + csrfToken)
	req := httptest.NewRequest(
		http.MethodPost,
		"/rewards/"+reward.ID.String()+"/redeem",
		reqBody,
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("insufficient points redeem: status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "enough points") {
		t.Errorf("409 response missing insufficient-points message: %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /rewards/{id}/redeem — ErrRewardNotFound → 404
// ---------------------------------------------------------------------------

func TestRedeemUnknownRewardReturns404(t *testing.T) {
	member := testMember()
	// getErr = ErrRewardNotFound so GetReward returns not-found.
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{
		getErr: tasksdomain.ErrRewardNotFound,
	}, member)

	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	fakeID := tasksdomain.NewRewardID().String()
	reqBody := strings.NewReader("csrf_token=" + csrfToken)
	req := httptest.NewRequest(
		http.MethodPost,
		"/rewards/"+fakeID+"/redeem",
		reqBody,
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown reward redeem: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: nav — Rewards item exists
// ---------------------------------------------------------------------------

func TestRewardsNavItemExists(t *testing.T) {
	nav := primaryNav("")
	var found bool
	for _, item := range nav {
		if item.Href == rewardsNavHref {
			found = true
			if item.Label != "Rewards" {
				t.Errorf("Rewards nav item label = %q, want Rewards", item.Label)
			}
		}
	}
	if !found {
		t.Errorf("primary nav has no item with href %q", rewardsNavHref)
	}
}

func TestRewardsNavActiveWhenOnRewards(t *testing.T) {
	nav := primaryNav(rewardsNavHref)
	var activeCount int
	for _, item := range nav {
		if item.Active {
			activeCount++
			if item.Href != rewardsNavHref {
				t.Errorf("unexpected active item: %q", item.Href)
			}
		}
	}
	if activeCount != 1 {
		t.Errorf("active nav items = %d, want 1", activeCount)
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /rewards — affordability badge (NES-126 AC3)
// ---------------------------------------------------------------------------

func TestRewardsPageShowsNeedMoreBadgeWhenUnaffordable(t *testing.T) {
	member := testMember()
	reward := &tasksdomain.Reward{
		ID:          tasksdomain.NewRewardID(),
		HouseholdID: member.HouseholdID,
		Name:        "Pricey prize",
		CostPoints:  50,
		Active:      true,
	}
	// Balance of 10 against a 50-point reward leaves a 40-point shortfall.
	handler, sm := buildGamificationTestHandlerWithLedger(
		&configurableRewardRepo{reward: reward}, configurableLedgerRepo{balance: 10}, member,
	)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "You need 40 more") {
		t.Errorf("rewards page missing need-more badge for unaffordable reward: %q", body)
	}
	if strings.Contains(body, `action="/rewards/`+reward.ID.String()+`/redeem"`) {
		t.Errorf("rewards page renders an active redeem form for an unaffordable reward: %q", body)
	}
}

func TestRewardsPageShowsRedeemButtonWhenAffordable(t *testing.T) {
	// The affordability contract is balance >= CostPoints: an exact balance
	// must redeem, so both the above-cost and exactly-at-cost boundaries are
	// covered (a regression to a strict > would only fail the second case).
	cases := []struct {
		name    string
		balance int
	}{
		{"balance above cost", 25},
		{"balance exactly at cost", 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			member := testMember()
			reward := &tasksdomain.Reward{
				ID:          tasksdomain.NewRewardID(),
				HouseholdID: member.HouseholdID,
				Name:        "Cheap prize",
				CostPoints:  10,
				Active:      true,
			}
			handler, sm := buildGamificationTestHandlerWithLedger(
				&configurableRewardRepo{reward: reward}, configurableLedgerRepo{balance: tc.balance}, member,
			)
			cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

			req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
			req.Header.Set("Cookie", cookie)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, "You need") {
				t.Errorf("rewards page shows a need-more badge for an affordable reward (balance %d): %q", tc.balance, body)
			}
			if !strings.Contains(body, `action="/rewards/`+reward.ID.String()+`/redeem"`) {
				t.Errorf("rewards page missing an active redeem form for an affordable reward (balance %d): %q", tc.balance, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /rewards — "Manage rewards" link role gate
// ---------------------------------------------------------------------------

func TestRewardsPageShowsManageLinkForParent(t *testing.T) {
	// "Parent" means owner OR adult — both roles must see the link so an
	// accidental single-role gate cannot pass unnoticed.
	cases := []struct {
		name string
		role household.Role
	}{
		{"adult", household.RoleAdult},
		{"owner", household.RoleOwner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parent := &household.Member{
				ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
				DisplayName: "Alice", Role: tc.role, Color: household.ColorSage,
			}
			handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, parent)
			cookie, _ := seedAuthedSession(t, handler, sm, parent.ID.String())

			req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
			req.Header.Set("Cookie", cookie)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `href="/admin/rewards"`) {
				t.Errorf("rewards page missing Manage rewards link for %s: %q", tc.role, rec.Body.String())
			}
		})
	}
}

func TestRewardsPageHidesManageLinkForChild(t *testing.T) {
	child := &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Kiddo", Role: household.RoleChild, Color: household.ColorSage,
	}
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, child)
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /rewards as a child: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `href="/admin/rewards"`) {
		t.Errorf("rewards page shows Manage rewards link for a child: %q", rec.Body.String())
	}
}
