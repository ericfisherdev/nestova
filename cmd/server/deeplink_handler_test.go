package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	deeplinkadapter "github.com/ericfisherdev/nestova/internal/deeplink/adapter"
	deeplinkapp "github.com/ericfisherdev/nestova/internal/deeplink/app"
	deeplinkdomain "github.com/ericfisherdev/nestova/internal/deeplink/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	tasksapp "github.com/ericfisherdev/nestova/internal/tasks/app"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Household-tenant-aware fakes.
//
// The shared fakeTaskInstanceRepo/configurableRewardRepo fakes elsewhere in
// this package return a single canned instance/reward regardless of the
// householdID argument, which is unsuitable for NES-129's tenant-isolation
// acceptance criterion (AC5): they never actually check household. These
// wrap them and add a real household-scoped store, mirroring the production
// Postgres adapters' documented "unknown or belongs to another household"
// contract.
// ---------------------------------------------------------------------------

type householdScopedTaskInstanceRepo struct {
	fakeTaskInstanceRepo
	mu        sync.Mutex
	instances map[tasksdomain.TaskInstanceID]*tasksdomain.TaskInstance
}

func newHouseholdScopedTaskInstanceRepo() *householdScopedTaskInstanceRepo {
	return &householdScopedTaskInstanceRepo{instances: make(map[tasksdomain.TaskInstanceID]*tasksdomain.TaskInstance)}
}

func (r *householdScopedTaskInstanceRepo) seed(inst *tasksdomain.TaskInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *inst
	r.instances[inst.ID] = &cp
}

func (r *householdScopedTaskInstanceRepo) Get(_ context.Context, householdID household.HouseholdID, id tasksdomain.TaskInstanceID) (*tasksdomain.TaskInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[id]
	if !ok || inst.HouseholdID != householdID {
		return nil, tasksdomain.ErrInstanceNotFound
	}
	cp := *inst
	return &cp, nil
}

func (r *householdScopedTaskInstanceRepo) Claim(_ context.Context, householdID household.HouseholdID, id tasksdomain.TaskInstanceID, assignee household.MemberID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[id]
	if !ok || inst.HouseholdID != householdID {
		return tasksdomain.ErrInstanceNotFound
	}
	if inst.Status == tasksdomain.StatusDone || inst.Status == tasksdomain.StatusSkipped {
		return tasksdomain.ErrInstanceInTerminalState
	}
	if inst.AssigneeID != nil && *inst.AssigneeID != assignee {
		return tasksdomain.ErrInstanceAlreadyClaimed
	}
	inst.AssigneeID = &assignee
	return nil
}

func (r *householdScopedTaskInstanceRepo) CompleteAndAward(_ context.Context, householdID household.HouseholdID, id tasksdomain.TaskInstanceID, by household.MemberID, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[id]
	if !ok || inst.HouseholdID != householdID {
		return tasksdomain.ErrInstanceNotFound
	}
	if inst.Status == tasksdomain.StatusDone || inst.Status == tasksdomain.StatusSkipped {
		return tasksdomain.ErrInstanceInTerminalState
	}
	inst.Status = tasksdomain.StatusDone
	inst.CompletedBy = &by
	inst.CompletedAt = &at
	return nil
}

// Compile-time assertion.
var _ tasksdomain.TaskInstanceRepository = (*householdScopedTaskInstanceRepo)(nil)

// seededRecurringTaskRepo is a household-scoped RecurringTaskRepository fake
// so the confirm screen can resolve a real chore title instead of always
// degrading to "(archived)".
type seededRecurringTaskRepo struct {
	fakeRecurringTaskRepo
	tasks map[tasksdomain.RecurringTaskID]*tasksdomain.RecurringTask
}

func newSeededRecurringTaskRepo() *seededRecurringTaskRepo {
	return &seededRecurringTaskRepo{tasks: make(map[tasksdomain.RecurringTaskID]*tasksdomain.RecurringTask)}
}

func (r *seededRecurringTaskRepo) seed(rt *tasksdomain.RecurringTask) {
	r.tasks[rt.ID] = rt
}

func (r *seededRecurringTaskRepo) Get(_ context.Context, householdID household.HouseholdID, id tasksdomain.RecurringTaskID) (*tasksdomain.RecurringTask, error) {
	rt, ok := r.tasks[id]
	if !ok || rt.HouseholdID != householdID {
		return nil, tasksdomain.ErrTaskNotFound
	}
	return rt, nil
}

// Compile-time assertion.
var _ tasksdomain.RecurringTaskRepository = (*seededRecurringTaskRepo)(nil)

// householdScopedRewardRepo is a household-scoped RewardRedeemer/RewardRepository
// fake with a real per-member balance, so redeem tests can exercise
// insufficient-points and tenant-isolation without a database.
type householdScopedRewardRepo struct {
	configurableRewardRepo
	mu       sync.Mutex
	rewards  map[tasksdomain.RewardID]*tasksdomain.Reward
	balances map[household.MemberID]int
}

func newHouseholdScopedRewardRepo() *householdScopedRewardRepo {
	return &householdScopedRewardRepo{
		rewards:  make(map[tasksdomain.RewardID]*tasksdomain.Reward),
		balances: make(map[household.MemberID]int),
	}
}

func (r *householdScopedRewardRepo) seed(reward *tasksdomain.Reward) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *reward
	r.rewards[reward.ID] = &cp
}

func (r *householdScopedRewardRepo) setBalance(memberID household.MemberID, points int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.balances[memberID] = points
}

func (r *householdScopedRewardRepo) GetReward(_ context.Context, householdID household.HouseholdID, id tasksdomain.RewardID) (*tasksdomain.Reward, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reward, ok := r.rewards[id]
	if !ok || reward.HouseholdID != householdID {
		return nil, tasksdomain.ErrRewardNotFound
	}
	cp := *reward
	return &cp, nil
}

func (r *householdScopedRewardRepo) RedeemWithDebit(_ context.Context, redemption *tasksdomain.RewardRedemption) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// redeemCalls is the promoted field from the embedded configurableRewardRepo
	// — incremented here (this override never calls the embedded method) so
	// tests can assert the underlying service call count directly, mirroring
	// configurableRewardRepo's own precedent for the same field.
	r.redeemCalls++
	reward, ok := r.rewards[redemption.RewardID]
	if !ok || reward.HouseholdID != redemption.HouseholdID || !reward.Active {
		return 0, tasksdomain.ErrRewardNotFound
	}
	if r.balances[redemption.MemberID] < reward.CostPoints {
		return 0, tasksdomain.ErrInsufficientPoints
	}
	r.balances[redemption.MemberID] -= reward.CostPoints
	return reward.CostPoints, nil
}

// Compile-time assertions.
var (
	_ tasksdomain.RewardRepository = (*householdScopedRewardRepo)(nil)
	_ tasksapp.RewardRedeemer      = (*householdScopedRewardRepo)(nil)
)

// ---------------------------------------------------------------------------
// fakeClock: a mutable clock so signature-expiry tests can advance time
// without sleeping.
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---------------------------------------------------------------------------
// successCredRepo: a CredentialRepository that authenticates one known
// email/password pair with a real argon2id hash, so AC2's login-continuation
// test can exercise the REAL POST /login handler (not the member_id-stamping
// shortcut seedAuthedSession uses elsewhere) end to end.
// ---------------------------------------------------------------------------

type successCredRepo struct {
	memberID household.MemberID
	email    string
	hash     string
}

func newSuccessCredRepo(t *testing.T, memberID household.MemberID, email, password string) successCredRepo {
	t.Helper()
	hash, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	return successCredRepo{memberID: memberID, email: email, hash: hash}
}

func (r successCredRepo) FindByEmail(_ context.Context, email string) (*authdomain.Credential, error) {
	if email != r.email {
		return nil, authdomain.ErrInvalidCredentials
	}
	return &authdomain.Credential{MemberID: r.memberID, PasswordHash: r.hash}, nil
}

func (r successCredRepo) SetPassword(_ context.Context, _ household.MemberID, _, _ string) error {
	return nil
}

// Compile-time assertion.
var _ authdomain.CredentialRepository = successCredRepo{}

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// deepLinkFixture bundles everything a deep-link test needs: the wired
// handler, the session manager (for seedAuthedSession), the signer (for
// minting/tampering test URLs), the clock (for expiry tests), and the
// household-scoped repos (for seeding instances/rewards and asserting on
// their post-request state).
type deepLinkFixture struct {
	handler      http.Handler
	sm           *scs.SessionManager
	signer       *deeplinkapp.Signer
	clock        *fakeClock
	instances    *householdScopedTaskInstanceRepo
	recurring    *seededRecurringTaskRepo
	rewards      *householdScopedRewardRepo
	credRepo     successCredRepo
	authHandlers *authadapter.Handlers
}

// buildDeepLinkFixture wires a minimal http.Handler exercising exactly the
// routes NES-129 needs (GET/POST /login for the login-continuation flow,
// registerDeepLinkPages for /go/*) — mirroring buildKioskTestHandler's lean,
// purpose-built harness rather than the full registerWebRoutes surface,
// which this feature does not touch.
func buildDeepLinkFixture(t *testing.T, member *household.Member) *deepLinkFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}

	clock := newFakeClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	instances := newHouseholdScopedTaskInstanceRepo()
	recurring := newSeededRecurringTaskRepo()
	taskService, err := tasksapp.NewTaskService(recurring, instances)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}

	rewards := newHouseholdScopedRewardRepo()
	rewardService := tasksapp.NewRewardService(rewards, householdRepo, fakeEnqueuer{}, logger)

	signer, err := deeplinkapp.NewSigner([]byte("deeplink-test-harness-signing-key"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	deepLinkHandlers := deeplinkadapter.NewWebHandlers(
		signer, taskService, recurring, instances, rewardService, rewards, sm, logger, clock.Now,
	)

	credRepo := newSuccessCredRepo(t, member.ID, "member@example.com", "correct horse battery staple")
	authHandlers := authadapter.NewHandlers(sm, authapp.New(credRepo), logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	registerDeepLinkPages(mux, sm, deepLinkHandlers)

	handler := sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	)

	return &deepLinkFixture{
		handler: handler, sm: sm, signer: signer, clock: clock,
		instances: instances, recurring: recurring, rewards: rewards,
		credRepo: credRepo, authHandlers: authHandlers,
	}
}

// mintURL builds a valid, currently-signed /go/ path+query for action/id
// using the fixture's own signer and clock, so tests exercise exactly the
// same shape the kiosk itself would render.
func (f *deepLinkFixture) mintURL(t *testing.T, action deeplinkdomain.Action, id string) string {
	t.Helper()
	path, err := action.Path(id)
	if err != nil {
		t.Fatalf("action.Path(%q): %v", id, err)
	}
	exp, sig := f.signer.Sign(path, f.clock.Now())
	return fmt.Sprintf("%s?exp=%d&sig=%s", path, exp, url.QueryEscape(sig))
}

// extractCSRFToken is reused from kiosk_handler_test.go (identical need: pull
// the csrf_token hidden field's value out of a rendered confirm-page body).

// seedInstance builds and stores a claimable pending chore instance for
// member's household, seeding a matching recurring task so its title
// resolves, and returns its id.
func (f *deepLinkFixture) seedInstance(member *household.Member, title string, assignee *household.MemberID) tasksdomain.TaskInstanceID {
	rt := &tasksdomain.RecurringTask{
		ID: tasksdomain.NewRecurringTaskID(), HouseholdID: member.HouseholdID,
		Title: title, Category: tasksdomain.ChoreCategory, Active: true,
	}
	f.recurring.seed(rt)

	due := f.clock.Now()
	inst := &tasksdomain.TaskInstance{
		ID: tasksdomain.NewTaskInstanceID(), RecurringTaskID: rt.ID, HouseholdID: member.HouseholdID,
		AssigneeID: assignee, DueOn: &due, Status: tasksdomain.StatusPending, Kind: tasksdomain.KindScheduled,
	}
	f.instances.seed(inst)
	return inst.ID
}

// seedReward builds and stores an active reward for member's household and
// returns its id.
func (f *deepLinkFixture) seedReward(member *household.Member, name string, costPoints int) tasksdomain.RewardID {
	reward := &tasksdomain.Reward{
		ID: tasksdomain.NewRewardID(), HouseholdID: member.HouseholdID,
		Name: name, CostPoints: costPoints, Active: true,
	}
	f.rewards.seed(reward)
	return reward.ID
}

// ---------------------------------------------------------------------------
// AC1: scan → confirm screen renders → confirming performs the action.
// ---------------------------------------------------------------------------

func TestDeepLinkClaim_ConfirmRendersThenPostClaims(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	instanceID := f.seedInstance(member, "Take out the trash", nil)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

	// GET renders the confirm screen without performing the action.
	getReq := httptest.NewRequest(http.MethodGet, link, nil)
	getReq.Header.Set("Cookie", cookie)
	getRec := httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm: status = %d, want 200; body: %s", getRec.Code, getRec.Body.String())
	}
	body := getRec.Body.String()
	if !strings.Contains(body, "Take out the trash") {
		t.Errorf("GET confirm body missing chore title: %s", body)
	}
	if !strings.Contains(body, "Claim") {
		t.Errorf("GET confirm body missing claim affordance: %s", body)
	}
	if inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID); err != nil || inst.AssigneeID != nil {
		t.Fatalf("GET must not perform the claim; assignee = %v, err = %v", inst.AssigneeID, err)
	}

	csrfToken := extractCSRFToken(body)
	if csrfToken == "" {
		t.Fatal("could not extract csrf_token from the confirm page")
	}

	// POST performs the claim, then PRG-redirects (303) to the shared "done"
	// page rather than rendering the confirmation directly.
	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST confirm: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	if loc := postRec.Header().Get("Location"); loc != "/go/claim-task/done" {
		t.Errorf("Location = %q, want /go/claim-task/done", loc)
	}
	inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
	if err != nil {
		t.Fatalf("Get after claim: %v", err)
	}
	if inst.AssigneeID == nil || *inst.AssigneeID != member.ID {
		t.Errorf("instance not claimed by member after POST confirm: AssigneeID = %v", inst.AssigneeID)
	}

	// Following the redirect renders the friendly confirmation.
	doneReq := httptest.NewRequest(http.MethodGet, postRec.Header().Get("Location"), nil)
	doneReq.Header.Set("Cookie", cookie)
	doneRec := httptest.NewRecorder()
	f.handler.ServeHTTP(doneRec, doneReq)
	if doneRec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", postRec.Header().Get("Location"), doneRec.Code)
	}
	if !strings.Contains(doneRec.Body.String(), "Chore claimed") {
		t.Errorf("done page missing confirmation message: %s", doneRec.Body.String())
	}
}

func TestDeepLinkComplete_ConfirmRendersThenPostCompletes(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	instanceID := f.seedInstance(member, "Water the plants", &member.ID)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionCompleteTask, instanceID.String())

	getReq := httptest.NewRequest(http.MethodGet, link, nil)
	getReq.Header.Set("Cookie", cookie)
	getRec := httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm: status = %d, want 200", getRec.Code)
	}
	csrfToken := extractCSRFToken(getRec.Body.String())

	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST confirm: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	if loc := postRec.Header().Get("Location"); loc != "/go/complete-task/done" {
		t.Errorf("Location = %q, want /go/complete-task/done", loc)
	}
	inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
	if err != nil {
		t.Fatalf("Get after complete: %v", err)
	}
	if inst.Status != tasksdomain.StatusDone {
		t.Errorf("instance status = %v, want done", inst.Status)
	}
	if inst.CompletedBy == nil || *inst.CompletedBy != member.ID {
		t.Errorf("CompletedBy = %v, want %v", inst.CompletedBy, member.ID)
	}
}

func TestDeepLinkRedeem_ConfirmRendersThenPostRedeems(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	rewardID := f.seedReward(member, "Movie night", 50)
	f.rewards.setBalance(member.ID, 100)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionRedeemReward, rewardID.String())

	getReq := httptest.NewRequest(http.MethodGet, link, nil)
	getReq.Header.Set("Cookie", cookie)
	getRec := httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm: status = %d, want 200", getRec.Code)
	}
	if !strings.Contains(getRec.Body.String(), "Movie night") {
		t.Errorf("GET confirm body missing reward name: %s", getRec.Body.String())
	}
	csrfToken := extractCSRFToken(getRec.Body.String())

	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST confirm: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	if loc := postRec.Header().Get("Location"); loc != "/go/redeem-reward/done" {
		t.Errorf("Location = %q, want /go/redeem-reward/done", loc)
	}
	f.rewards.mu.Lock()
	balance := f.rewards.balances[member.ID]
	f.rewards.mu.Unlock()
	if balance != 50 {
		t.Errorf("balance after redeem = %d, want 50", balance)
	}

	doneReq := httptest.NewRequest(http.MethodGet, postRec.Header().Get("Location"), nil)
	doneReq.Header.Set("Cookie", cookie)
	doneRec := httptest.NewRecorder()
	f.handler.ServeHTTP(doneRec, doneReq)
	if doneRec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", postRec.Header().Get("Location"), doneRec.Code)
	}
	if !strings.Contains(doneRec.Body.String(), "Reward redeemed") {
		t.Errorf("done page missing confirmation message: %s", doneRec.Body.String())
	}
}

// TestDeepLinkRedeem_ResubmittedPostDoesNotRedeemTwice is the regression
// test for the redeem idempotency finding: the signed link's signature and
// the member's CSRF token both remain individually valid for reuse within
// the link's own TTL, so nothing else stops a double-tap or a
// refresh-and-resend from reaching RewardService.Redeem a second time. A
// second POST of the EXACT SAME (still-unexpired, still-correctly-signed)
// request must be rejected with the friendly "already used" page, and the
// reward must be debited exactly once.
func TestDeepLinkRedeem_ResubmittedPostDoesNotRedeemTwice(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	rewardID := f.seedReward(member, "Movie night", 50)
	f.rewards.setBalance(member.ID, 100)
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionRedeemReward, rewardID.String())

	postForm := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}

	first := postForm()
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first POST: status = %d, want 303; body: %s", first.Code, first.Body.String())
	}

	second := postForm()
	if second.Code != http.StatusConflict {
		t.Fatalf("second (resubmitted) POST: status = %d, want 409; body: %s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "already been used") {
		t.Errorf("second POST body missing the already-used message: %s", second.Body.String())
	}

	f.rewards.mu.Lock()
	balance := f.rewards.balances[member.ID]
	redemptionCount := f.rewards.redeemCalls
	f.rewards.mu.Unlock()
	if balance != 50 {
		t.Errorf("balance after two POSTs of the same link = %d, want 50 (debited exactly once)", balance)
	}
	if redemptionCount != 1 {
		t.Errorf("RedeemWithDebit call count = %d, want exactly 1", redemptionCount)
	}
}

// TestDeepLinkRedeem_RefreshAfterSuccessDoesNotResubmit is the PRG
// correctness test: following the 303's Location (a plain GET, exactly what
// a browser does on refresh after a redirect) must never itself trigger a
// second redemption — only an actual resubmitted POST could, and that path
// is covered separately above.
func TestDeepLinkRedeem_RefreshAfterSuccessDoesNotResubmit(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	rewardID := f.seedReward(member, "Movie night", 50)
	f.rewards.setBalance(member.ID, 100)
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionRedeemReward, rewardID.String())

	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST confirm: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	doneLoc := postRec.Header().Get("Location")

	// Simulate the browser "refreshing" the post-redirect page: repeated GETs
	// of the redirect target, exactly what a real refresh does (browsers
	// never resubmit the original POST after following a 303).
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, doneLoc, nil)
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s (refresh %d): status = %d, want 200", doneLoc, i, rec.Code)
		}
	}

	f.rewards.mu.Lock()
	balance := f.rewards.balances[member.ID]
	redemptionCount := f.rewards.redeemCalls
	f.rewards.mu.Unlock()
	if balance != 50 {
		t.Errorf("balance after 3 refreshes of the done page = %d, want 50 (redeemed exactly once)", balance)
	}
	if redemptionCount != 1 {
		t.Errorf("RedeemWithDebit call count after refreshes = %d, want exactly 1", redemptionCount)
	}
}

// TestDeepLinkRedeem_FailedRedeemReleasesSignatureForRetry verifies the
// release() half of the guard: when RedeemWithDebit itself rejects the
// attempt (here, insufficient points), the link was never actually consumed
// by a successful redemption, so a legitimate retry with the SAME link
// (e.g. after the member earns more points) must not be blocked by a
// phantom "already used" state.
func TestDeepLinkRedeem_FailedRedeemReleasesSignatureForRetry(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	rewardID := f.seedReward(member, "Expensive treat", 500)
	f.rewards.setBalance(member.ID, 10) // insufficient
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionRedeemReward, rewardID.String())

	postForm := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}

	first := postForm()
	if first.Code != http.StatusConflict {
		t.Fatalf("first POST (insufficient points): status = %d, want 409; body: %s", first.Code, first.Body.String())
	}
	if strings.Contains(first.Body.String(), "already been used") {
		t.Fatalf("insufficient-points rejection must not be reported as already-used: %s", first.Body.String())
	}

	// The member earns enough points, then retries with the SAME link.
	f.rewards.setBalance(member.ID, 500)
	second := postForm()
	if second.Code != http.StatusSeeOther {
		t.Fatalf("retry POST after earning enough points: status = %d, want 303; body: %s", second.Code, second.Body.String())
	}
}

func TestDeepLinkAddChore_GetRendersConfirm_PostRedirectsToNewTaskForm(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionAddChore, "")

	getReq := httptest.NewRequest(http.MethodGet, link, nil)
	getReq.Header.Set("Cookie", cookie)
	getRec := httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm: status = %d, want 200", getRec.Code)
	}
	csrfToken := extractCSRFToken(getRec.Body.String())

	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST add-chore confirm: status = %d, want 303", postRec.Code)
	}
	if loc := postRec.Header().Get("Location"); loc != "/tasks/new" {
		t.Errorf("Location = %q, want /tasks/new", loc)
	}
}

// ---------------------------------------------------------------------------
// AC2: logged-out phone → login → lands back on the same confirm screen,
// without rescanning. Also closes out AC4 end to end (a crafted off-origin
// next is ignored even through the real POST /login handler).
// ---------------------------------------------------------------------------

func TestDeepLinkLogin_ContinuesToConfirmScreenAfterLogin(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	instanceID := f.seedInstance(member, "Feed the cat", nil)
	link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

	// 1. Unauthenticated GET is redirected to /login?next=<the signed link>.
	req := httptest.NewRequest(http.MethodGet, link, nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET %s: status = %d, want 303", link, rec.Code)
	}
	loginLoc := rec.Header().Get("Location")
	if !strings.HasPrefix(loginLoc, "/login?next=") {
		t.Fatalf("Location = %q, want /login?next=...", loginLoc)
	}
	var sessionCookie string
	for _, c := range rec.Result().Cookies() {
		sessionCookie = c.Name + "=" + c.Value
	}

	// 2. GET the login page (carrying the session cookie) to obtain a CSRF
	// token, then POST real credentials — exercising the actual Authenticator/
	// Login handler, not the member_id-stamping test shortcut.
	loginPageReq := httptest.NewRequest(http.MethodGet, loginLoc, nil)
	loginPageReq.Header.Set("Cookie", sessionCookie)
	loginPageRec := httptest.NewRecorder()
	f.handler.ServeHTTP(loginPageRec, loginPageReq)
	if loginPageRec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", loginLoc, loginPageRec.Code)
	}
	// GetCSRFToken (invoked by LoginPage) is what actually writes the session
	// for the first time in this flow — step 1's redirect never touched the
	// session, so scs may not have set a cookie there at all. Re-capture from
	// THIS response (falling back to the step-1 cookie if unchanged) so the
	// POST below carries the session the CSRF token was actually minted into.
	for _, c := range loginPageRec.Result().Cookies() {
		sessionCookie = c.Name + "=" + c.Value
	}
	csrfToken := extractCSRFToken(loginPageRec.Body.String())
	if csrfToken == "" {
		t.Fatal("could not extract csrf_token from the login page")
	}
	nextParam := strings.TrimPrefix(loginLoc, "/login?next=")

	form := url.Values{
		"csrf_token": {csrfToken},
		"email":      {"member@example.com"},
		"password":   {"correct horse battery staple"},
		"next":       {mustUnescape(t, nextParam)},
	}
	loginPostReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	loginPostReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginPostReq.Header.Set("Cookie", sessionCookie)
	loginPostRec := httptest.NewRecorder()
	f.handler.ServeHTTP(loginPostRec, loginPostReq)

	if loginPostRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login: status = %d, want 303; body: %s", loginPostRec.Code, loginPostRec.Body.String())
	}
	postLoginLoc := loginPostRec.Header().Get("Location")
	if postLoginLoc != link {
		t.Fatalf("post-login redirect = %q, want the original deep link %q", postLoginLoc, link)
	}
	// Login.RenewToken rotates the session token on privilege escalation (see
	// its doc comment), so the cookie changes here too — re-capture it before
	// following the redirect, or step 3 below would present a stale,
	// pre-authentication cookie and bounce right back to /login.
	for _, c := range loginPostRec.Result().Cookies() {
		sessionCookie = c.Name + "=" + c.Value
	}

	// 3. Following that redirect (now authenticated) lands on the SAME
	// confirm screen without rescanning.
	confirmReq := httptest.NewRequest(http.MethodGet, postLoginLoc, nil)
	confirmReq.Header.Set("Cookie", sessionCookie)
	confirmRec := httptest.NewRecorder()
	f.handler.ServeHTTP(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("GET %s after login: status = %d, want 200; body: %s", postLoginLoc, confirmRec.Code, confirmRec.Body.String())
	}
	if !strings.Contains(confirmRec.Body.String(), "Feed the cat") {
		t.Errorf("post-login confirm screen missing chore title: %s", confirmRec.Body.String())
	}
}

// TestDeepLinkLogin_CraftedOffOriginNextIsIgnored is the end-to-end
// counterpart to internal/auth/adapter's unit-level TestSanitizeNext (AC4):
// even through the real POST /login handler, a crafted off-origin next
// parameter accompanying what looks like a /go/ deep-link flow never
// redirects off Nestova's own origin.
func TestDeepLinkLogin_CraftedOffOriginNextIsIgnored(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)

	getReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	getRec := httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)
	var sessionCookie string
	for _, c := range getRec.Result().Cookies() {
		sessionCookie = c.Name + "=" + c.Value
	}
	csrfToken := extractCSRFToken(getRec.Body.String())

	form := url.Values{
		"csrf_token": {csrfToken},
		"email":      {"member@example.com"},
		"password":   {"correct horse battery staple"},
		"next":       {"https://evil.example/steal"},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", sessionCookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login: status = %d, want 303", postRec.Code)
	}
	if loc := postRec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want same-origin fallback %q", loc, "/")
	}
}

func mustUnescape(t *testing.T, s string) string {
	t.Helper()
	out, err := url.QueryUnescape(s)
	if err != nil {
		t.Fatalf("QueryUnescape(%q): %v", s, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// AC3: expired or tampered links are rejected with the friendly rescan page,
// never distinguishing which applies.
// ---------------------------------------------------------------------------

func TestDeepLinkSignature_ExpiredAndTamperedRejected(t *testing.T) {
	member := testMember()

	tests := []struct {
		name string
		link func(f *deepLinkFixture, instanceID tasksdomain.TaskInstanceID) string
	}{
		{
			name: "expired",
			link: func(f *deepLinkFixture, instanceID tasksdomain.TaskInstanceID) string {
				link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())
				f.clock.Advance(deeplinkapp.LinkTTL + time.Second)
				return link
			},
		},
		{
			name: "tampered id",
			link: func(f *deepLinkFixture, instanceID tasksdomain.TaskInstanceID) string {
				link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())
				return strings.Replace(link, instanceID.String(), tasksdomain.NewTaskInstanceID().String(), 1)
			},
		},
		{
			name: "tampered signature",
			link: func(f *deepLinkFixture, instanceID tasksdomain.TaskInstanceID) string {
				link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())
				return link + "XX"
			},
		},
		{
			name: "missing signature params",
			link: func(_ *deepLinkFixture, instanceID tasksdomain.TaskInstanceID) string {
				path, err := deeplinkdomain.ActionClaimTask.Path(instanceID.String())
				if err != nil {
					t.Fatalf("Path: %v", err)
				}
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := buildDeepLinkFixture(t, member)
			instanceID := f.seedInstance(member, "Mow the lawn", nil)
			cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
			link := tt.link(f, instanceID)

			req := httptest.NewRequest(http.MethodGet, link, nil)
			req.Header.Set("Cookie", cookie)
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("GET %s: status = %d, want 400; body: %s", link, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "rescan") {
				t.Errorf("body does not contain the friendly rescan message: %s", rec.Body.String())
			}

			// The instance must remain untouched — an invalid link never
			// reaches the domain layer.
			inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
			if err != nil || inst.AssigneeID != nil {
				t.Errorf("instance was mutated by a rejected link: assignee = %v, err = %v", inst.AssigneeID, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC5: the signature alone never authorizes — a signed URL for household A's
// chore hit by a member of household B fails the domain check.
// ---------------------------------------------------------------------------

func TestDeepLinkTenantIsolation_CrossHouseholdRejected(t *testing.T) {
	memberA := testMember()
	f := buildDeepLinkFixture(t, memberA)
	instanceID := f.seedInstance(memberA, "Household A's chore", nil)

	// A DIFFERENT member, in a DIFFERENT household, but authenticated against
	// the SAME server instance (as RequireMember requires — the deep link
	// grants no household context of its own).
	memberB := testMember()
	// authedHouseholdRepo resolves every session to the single member it was
	// constructed with (memberA here), so memberB is simulated by minting a
	// session for an id authedHouseholdRepo does not recognize as memberA —
	// the GetMember lookup returns ErrMemberNotFound, and Authenticate treats
	// the request as unauthenticated, which itself proves isolation one layer
	// earlier (no session ⇒ no access) but does not exercise the SERVICE-layer
	// tenant check AC5 targets. So this test instead builds its OWN fixture
	// for memberB and points it at memberA's fixture's signer/instance store,
	// reproducing "the same server, two households" precisely.
	fB := buildDeepLinkFixtureWithSharedStores(t, memberB, f)
	cookieB, csrfTokenB := seedAuthedSession(t, fB.handler, fB.sm, memberB.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

	req := httptest.NewRequest(http.MethodGet, link, nil)
	req.Header.Set("Cookie", cookieB)
	rec := httptest.NewRecorder()
	fB.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET %s as a member of a different household: status = %d, want 404; body: %s", link, rec.Code, rec.Body.String())
	}

	// Confirming (POST, with a valid CSRF token for memberB's own session)
	// must fail the same way — the signature verifies fine (it only proves
	// the link was minted by this server), but the domain lookup, scoped to
	// memberB's OWN household, still finds nothing.
	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfTokenB))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookieB)
	postRec := httptest.NewRecorder()
	fB.handler.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusNotFound {
		t.Fatalf("POST %s as a member of a different household: status = %d, want 404; body: %s", link, postRec.Code, postRec.Body.String())
	}

	// The instance itself must remain unclaimed regardless.
	inst, err := f.instances.Get(context.Background(), memberA.HouseholdID, instanceID)
	if err != nil || inst.AssigneeID != nil {
		t.Errorf("instance was mutated across households: assignee = %v, err = %v", inst.AssigneeID, err)
	}
}

// buildDeepLinkFixtureWithSharedStores builds a fixture for member but reuses
// shared's signer, clock, instance store, recurring-task store, and reward
// store — reproducing "one server, two households" for the tenant-isolation
// test above, where memberA's data must remain invisible to memberB even
// though both are authenticated against the exact same signer/service
// instances a real single-server deployment would share.
func buildDeepLinkFixtureWithSharedStores(t *testing.T, member *household.Member, shared *deepLinkFixture) *deepLinkFixture {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}

	taskService, err := tasksapp.NewTaskService(shared.recurring, shared.instances)
	if err != nil {
		t.Fatalf("NewTaskService: %v", err)
	}
	rewardService := tasksapp.NewRewardService(shared.rewards, householdRepo, fakeEnqueuer{}, logger)

	deepLinkHandlers := deeplinkadapter.NewWebHandlers(
		shared.signer, taskService, shared.recurring, shared.instances, rewardService, shared.rewards,
		sm, logger, shared.clock.Now,
	)

	authHandlers := authadapter.NewHandlers(sm, authapp.New(shared.credRepo), logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	mux.HandleFunc("POST /login", authHandlers.Login)
	registerDeepLinkPages(mux, sm, deepLinkHandlers)

	handler := sm.LoadAndSave(
		authadapter.Authenticate(sm, householdRepo)(mux),
	)

	return &deepLinkFixture{
		handler: handler, sm: sm, signer: shared.signer, clock: shared.clock,
		instances: shared.instances, recurring: shared.recurring, rewards: shared.rewards,
		credRepo: shared.credRepo, authHandlers: authHandlers,
	}
}

// ---------------------------------------------------------------------------
// Additional coverage: domain-rule rejections and the per-member rate limit.
// ---------------------------------------------------------------------------

func TestDeepLinkClaim_AlreadyClaimedByAnotherMemberIsConflict(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	other := household.NewMemberID()
	instanceID := f.seedInstance(member, "Vacuum the hallway", &other)
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

	req := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST claim on an already-claimed instance: status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
}

func TestDeepLinkRedeem_InsufficientPointsIsConflict(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	rewardID := f.seedReward(member, "Expensive treat", 500)
	f.rewards.setBalance(member.ID, 10)
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionRedeemReward, rewardID.String())

	req := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST redeem with insufficient points: status = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
}

func TestDeepLinkConfirm_BadCSRFRejected(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	instanceID := f.seedInstance(member, "Sweep the porch", nil)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

	req := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token=wrong-token"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST with a bad csrf_token: status = %d, want 403", rec.Code)
	}
}

func TestDeepLinkConfirm_UnknownActionNotFound(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/go/delete-household/abc-123?exp=1&sig=x", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET an unrecognized deep-link action: status = %d, want 404", rec.Code)
	}
}

// TestDeepLinkDone_NoDonePageForAddChoreOrUnknownAction verifies Done's own
// 404 guard: add-chore has no "done" page (it redirects straight to
// /tasks/new instead, never through Done), and a wholly unrecognized action
// obviously has none either.
func TestDeepLinkDone_NoDonePageForAddChoreOrUnknownAction(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())

	for _, action := range []string{"add-chore", "delete-household"} {
		req := httptest.NewRequest(http.MethodGet, "/go/"+action+"/done", nil)
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /go/%s/done: status = %d, want 404", action, rec.Code)
		}
	}
}

// TestDeepLinkConfirm_RateLimitExceeded verifies the per-member rate limit
// (NES-129) engages once its burst is exhausted: confirmRateBurst distinct
// claims succeed back-to-back (a member confirming several chores in a row is
// a normal flow), and the next one is throttled.
func TestDeepLinkConfirm_RateLimitExceeded(t *testing.T) {
	const burst = 5 // mirrors deeplink/adapter's confirmRateBurst
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	cookie, csrfToken := seedAuthedSession(t, f.handler, f.sm, member.ID.String())

	var lastCode int
	for i := 0; i < burst+1; i++ {
		instanceID := f.seedInstance(member, fmt.Sprintf("Chore %d", i), nil)
		link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

		req := httptest.NewRequest(http.MethodPost, link, strings.NewReader("csrf_token="+csrfToken))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		lastCode = rec.Code

		if i < burst && rec.Code != http.StatusSeeOther {
			t.Fatalf("claim %d (within burst): status = %d, want 303; body: %s", i, rec.Code, rec.Body.String())
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("claim beyond the burst: status = %d, want 429", lastCode)
	}
}
