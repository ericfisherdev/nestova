package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// FulfillRedemption — POST /admin/rewards/redemptions/{id}/fulfill (NES-127)
// ---------------------------------------------------------------------------

func TestFulfillRedemptionForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+id+"/fulfill", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST fulfill as a child: status = %d, want 403", rec.Code)
	}
	if len(repo.fulfillCalls) != 0 {
		t.Errorf("Fulfill called %d times for a forbidden child, want 0", len(repo.fulfillCalls))
	}
}

func TestFulfillRedemptionCSRFRejected(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=wrong-token"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+id+"/fulfill", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST fulfill with wrong CSRF: status = %d, want 403", rec.Code)
	}
	if len(repo.fulfillCalls) != 0 {
		t.Errorf("Fulfill called %d times on rejected CSRF, want 0", len(repo.fulfillCalls))
	}
}

func TestFulfillRedemptionSuccessRedirects(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	redemptionID := tasksdomain.NewRewardRedemptionID()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+redemptionID.String()+"/fulfill", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful fulfil: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/rewards" {
		t.Errorf("Location = %q, want /admin/rewards", loc)
	}
	if len(repo.fulfillCalls) != 1 {
		t.Fatalf("Fulfill called %d times, want 1", len(repo.fulfillCalls))
	}
	if repo.fulfillCalls[0] != redemptionID {
		t.Errorf("Fulfill called with %v, want %v", repo.fulfillCalls[0], redemptionID)
	}
}

func TestFulfillRedemptionNotPendingReturnsConflict(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{fulfillErr: tasksdomain.ErrRedemptionNotPending}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+id+"/fulfill", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("fulfil non-pending redemption: status = %d, want 409", rec.Code)
	}
}

func TestFulfillRedemptionNotFound(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{fulfillErr: tasksdomain.ErrRedemptionNotFound}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+id+"/fulfill", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("fulfil unknown redemption: status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// DenyRedemption — POST /admin/rewards/redemptions/{id}/deny (NES-127)
// ---------------------------------------------------------------------------

func TestDenyRedemptionForbiddenForChild(t *testing.T) {
	child := adminTestChild()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+id+"/deny", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST deny as a child: status = %d, want 403", rec.Code)
	}
	if len(repo.denyCalls) != 0 {
		t.Errorf("Deny called %d times for a forbidden child, want 0", len(repo.denyCalls))
	}
}

func TestDenyRedemptionSuccessPassesReasonAndRedirects(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	redemptionID := tasksdomain.NewRewardRedemptionID()
	const wantReason = "out of stock"
	form := "csrf_token=" + csrfToken + "&reason=" + "out+of+stock"
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+redemptionID.String()+"/deny", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful deny: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/rewards" {
		t.Errorf("Location = %q, want /admin/rewards", loc)
	}
	if len(repo.denyCalls) != 1 {
		t.Fatalf("Deny calls = %d, want 1", len(repo.denyCalls))
	}
	got := repo.denyCalls[0]
	if got.id != redemptionID {
		t.Errorf("Deny id = %v, want %v", got.id, redemptionID)
	}
	if got.reason != wantReason {
		t.Errorf("Deny reason = %q, want %q (the submitted form value must reach the service unchanged)", got.reason, wantReason)
	}
}

func TestDenyRedemptionNotPendingReturnsConflict(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{denyErr: tasksdomain.ErrRedemptionNotPending}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/admin/rewards/redemptions/"+id+"/deny", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("deny non-pending redemption: status = %d, want 409", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// CancelRedemption — POST /rewards/redemptions/{id}/cancel (NES-127)
// ---------------------------------------------------------------------------

// TestCancelRedemptionAnyMemberSucceeds verifies that a child member (not
// just a parent) can cancel their own pending redemption — Cancel is
// member-scoped self-service, unlike Fulfill/Deny which are parent-only.
func TestCancelRedemptionAnyMemberSucceeds(t *testing.T) {
	child := adminTestChild()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, child)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	redemptionID := tasksdomain.NewRewardRedemptionID()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/rewards/redemptions/"+redemptionID.String()+"/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("successful cancel: status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/rewards" {
		t.Errorf("Location = %q, want /rewards", loc)
	}
	if len(repo.cancelCalls) != 1 {
		t.Fatalf("Cancel calls = %d, want 1", len(repo.cancelCalls))
	}
	got := repo.cancelCalls[0]
	if got.id != redemptionID {
		t.Errorf("Cancel id = %v, want %v", got.id, redemptionID)
	}
	// The cancelling member id must come from the AUTHENTICATED session
	// (child.ID), never from anything the client could submit in the
	// request body — Cancel has no member_id form field at all.
	if got.memberID != child.ID {
		t.Errorf("Cancel memberID = %v, want %v (the authenticated member)", got.memberID, child.ID)
	}
}

func TestCancelRedemptionCSRFRejected(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=wrong-token"
	req := httptest.NewRequest(http.MethodPost, "/rewards/redemptions/"+id+"/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST cancel with wrong CSRF: status = %d, want 403", rec.Code)
	}
	if len(repo.cancelCalls) != 0 {
		t.Errorf("Cancel called %d times on rejected CSRF, want 0", len(repo.cancelCalls))
	}
}

func TestCancelRedemptionNotPendingReturnsConflict(t *testing.T) {
	adult := adminTestAdult()
	repo := &configurableRewardRepo{cancelErr: tasksdomain.ErrRedemptionNotPending}
	handler, sm := buildGamificationTestHandler(repo, adult)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	id := tasksdomain.NewRewardRedemptionID().String()
	body := "csrf_token=" + csrfToken
	req := httptest.NewRequest(http.MethodPost, "/rewards/redemptions/"+id+"/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel non-pending redemption: status = %d, want 409", rec.Code)
	}
}
