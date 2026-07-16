package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// Test member helpers
// ---------------------------------------------------------------------------

func adminTestChild() *household.Member {
	return &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Kiddo", Role: household.RoleChild, Color: household.ColorSage,
	}
}

func adminTestAdult() *household.Member {
	return &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
}

func adminTestOwner() *household.Member {
	return &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Owner", Role: household.RoleOwner, Color: household.ColorClay,
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /admin/rewards — role gate (NES-126 AC1)
// ---------------------------------------------------------------------------

func TestRewardsAdminForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, child)
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /admin/rewards as a child: status = %d, want 403", rec.Code)
	}
}

func TestRewardsAdminAllowedForAdult(t *testing.T) {
	adult := adminTestAdult()
	reward := &tasksdomain.Reward{
		ID: tasksdomain.NewRewardID(), HouseholdID: adult.HouseholdID,
		Name: "Movie night", CostPoints: 20, Active: true,
	}
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{reward: reward}, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/rewards as an adult: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Movie night") {
		t.Errorf("admin list missing reward name: %q", rec.Body.String())
	}
}

func TestRewardsAdminAllowedForOwner(t *testing.T) {
	owner := adminTestOwner()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, owner)
	cookie, _ := seedAuthedSession(t, handler, sm, owner.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/rewards as an owner: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /admin/rewards/new — role gate
// ---------------------------------------------------------------------------

func TestNewRewardPageForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, child)
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards/new", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /admin/rewards/new as a child: status = %d, want 403", rec.Code)
	}
}

func TestNewRewardPageAllowedForAdult(t *testing.T) {
	adult := adminTestAdult()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards/new", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/rewards/new as an adult: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `name="name"`) {
		t.Errorf("new reward page missing name field: %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /admin/rewards — create (NES-126 AC1)
// ---------------------------------------------------------------------------

func TestCreateRewardForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	body := "csrf_token=" + csrfToken + "&name=Toy&cost_points=10"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /admin/rewards as a child: status = %d, want 403", rec.Code)
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateReward called %d times for a forbidden child, want 0", len(repo.createCalls))
	}
}

func TestCreateRewardCSRFRejected(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	body := "csrf_token=wrong-token&name=Toy&cost_points=10"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /admin/rewards with wrong CSRF: status = %d, want 403", rec.Code)
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateReward called %d times on rejected CSRF, want 0", len(repo.createCalls))
	}
}

func TestCreateRewardSuccessRedirects(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	body := "csrf_token=" + csrfToken +
		"&name=Extra+screen+time&description=30+minutes&cost_points=20&image_ref=%F0%9F%8E%AE&quantity_available=5"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful create: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/rewards" {
		t.Errorf("Location = %q, want /admin/rewards", loc)
	}
	if len(repo.createCalls) != 1 {
		t.Fatalf("CreateReward called %d times, want 1", len(repo.createCalls))
	}
	created := repo.createCalls[0]
	if created.Name != "Extra screen time" {
		t.Errorf("created reward Name = %q, want %q", created.Name, "Extra screen time")
	}
	if created.CostPoints != 20 {
		t.Errorf("created reward CostPoints = %d, want 20", created.CostPoints)
	}
	if created.HouseholdID != adult.HouseholdID {
		t.Errorf("created reward HouseholdID = %v, want %v", created.HouseholdID, adult.HouseholdID)
	}
	if !created.Active {
		t.Error("created reward Active = false, want true")
	}
	if created.QuantityAvailable == nil || *created.QuantityAvailable != 5 {
		t.Errorf("created reward QuantityAvailable = %v, want 5", created.QuantityAvailable)
	}
}

func TestCreateRewardValidationFailureRerenders(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	// Missing name.
	body := "csrf_token=" + csrfToken + "&cost_points=10"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing-name create: status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Name is required") {
		t.Errorf("422 response missing validation message: %q", rec.Body.String())
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("CreateReward called %d times on validation failure, want 0", len(repo.createCalls))
	}
}

// ---------------------------------------------------------------------------
// Tests: GET /admin/rewards/{id}/edit
// ---------------------------------------------------------------------------

func TestEditRewardPageForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{}, child)
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards/"+tasksdomain.NewRewardID().String()+"/edit", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET edit reward as a child: status = %d, want 403", rec.Code)
	}
}

func TestEditRewardPageShowsExistingValues(t *testing.T) {
	adult := adminTestAdult()
	reward := &tasksdomain.Reward{
		ID: tasksdomain.NewRewardID(), HouseholdID: adult.HouseholdID,
		Name: "Choose dinner", Description: "Pick dinner", CostPoints: 30, Active: true,
	}
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{reward: reward}, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards/"+reward.ID.String()+"/edit", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET edit reward: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Choose dinner") {
		t.Errorf("edit form missing existing name: %q", rec.Body.String())
	}
}

func TestEditRewardPageNotFound(t *testing.T) {
	adult := adminTestAdult()
	handler, sm := buildGamificationTestHandler(&configurableRewardRepo{getErr: tasksdomain.ErrRewardNotFound}, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/admin/rewards/"+tasksdomain.NewRewardID().String()+"/edit", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET edit unknown reward: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /admin/rewards/{id} — update
// ---------------------------------------------------------------------------

func TestUpdateRewardForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	id := tasksdomain.NewRewardID().String()
	body := "csrf_token=" + csrfToken + "&name=Toy&cost_points=10"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST update reward as a child: status = %d, want 403", rec.Code)
	}
	if len(repo.updateCalls) != 0 {
		t.Errorf("UpdateReward called %d times for a forbidden child, want 0", len(repo.updateCalls))
	}
}

func TestUpdateRewardSuccessRedirects(t *testing.T) {
	adult := adminTestAdult()
	reward := &tasksdomain.Reward{
		ID: tasksdomain.NewRewardID(), HouseholdID: adult.HouseholdID,
		Name: "Old name", CostPoints: 10, Active: true,
	}
	repo := &configurableRewardRepo{reward: reward}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	body := "csrf_token=" + csrfToken + "&name=New+name&cost_points=15"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+reward.ID.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful update: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/rewards" {
		t.Errorf("Location = %q, want /admin/rewards", loc)
	}
	if len(repo.updateCalls) != 1 {
		t.Fatalf("UpdateReward called %d times, want 1", len(repo.updateCalls))
	}
	if got := repo.updateCalls[0].Name; got != "New name" {
		t.Errorf("updated reward Name = %q, want %q", got, "New name")
	}
	if got := repo.updateCalls[0].CostPoints; got != 15 {
		t.Errorf("updated reward CostPoints = %d, want 15", got)
	}
}

func TestUpdateRewardNotFound(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{getErr: tasksdomain.ErrRewardNotFound}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardID().String()
	body := "csrf_token=" + csrfToken + "&name=New+name&cost_points=15"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("update unknown reward: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Tests: POST /admin/rewards/{id}/archive (NES-126 AC1/AC5)
// ---------------------------------------------------------------------------

func TestArchiveRewardForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	id := tasksdomain.NewRewardID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+id+"/archive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST archive as a child: status = %d, want 403", rec.Code)
	}
	if len(repo.archiveCalls) != 0 {
		t.Errorf("ArchiveReward called %d times for a forbidden child, want 0", len(repo.archiveCalls))
	}
}

func TestArchiveRewardCSRFRejected(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardID().String()
	body := "csrf_token=wrong-token"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+id+"/archive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST archive with wrong CSRF: status = %d, want 403", rec.Code)
	}
	if len(repo.archiveCalls) != 0 {
		t.Errorf("ArchiveReward called %d times on rejected CSRF, want 0", len(repo.archiveCalls))
	}
}

func TestArchiveRewardSuccessRedirects(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	rewardID := tasksdomain.NewRewardID()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+rewardID.String()+"/archive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful archive: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/rewards" {
		t.Errorf("Location = %q, want /admin/rewards", loc)
	}
	if len(repo.archiveCalls) != 1 {
		t.Fatalf("ArchiveReward called %d times, want 1", len(repo.archiveCalls))
	}
	got := repo.archiveCalls[0]
	if got.householdID != adult.HouseholdID {
		t.Errorf("ArchiveReward householdID = %v, want %v", got.householdID, adult.HouseholdID)
	}
	if got.rewardID != rewardID {
		t.Errorf("ArchiveReward rewardID = %v, want %v", got.rewardID, rewardID)
	}
}

func TestArchiveRewardNotFound(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{archiveErr: tasksdomain.ErrRewardNotFound}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/"+id+"/archive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("archive unknown reward: status = %d, want 404", rec.Code)
	}
}
