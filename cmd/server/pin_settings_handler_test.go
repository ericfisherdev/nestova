package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// ---------------------------------------------------------------------------
// NES-165: the PIN section renders for every member, and its admin
// (set/reset a family member's PIN) sub-section is owner/adult-only —
// mirroring the MFA section's own AC1/AC5 coverage.
// ---------------------------------------------------------------------------

func TestSettingsPage_PINSection_VisibleToChild_AdminSubsectionHidden(t *testing.T) {
	child := adminTestChild()
	hhRepo := newMultiMemberHouseholdRepo(child)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings as a child: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Task PIN") {
		t.Error("a child member must see their own PIN section")
	}
	if strings.Contains(rec.Body.String(), "Set a family member") {
		t.Error("a child member must not see the owner/adult-only admin PIN section")
	}
}

func TestSettingsPage_PINSection_AdminSubsectionVisibleToOwner(t *testing.T) {
	owner := settingsTestOwner()
	child := settingsTestChildInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, child)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, owner.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings as an owner: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Set a family member") {
		t.Error("an owner must see the admin PIN section")
	}
	if !strings.Contains(body, child.DisplayName) {
		t.Error("the admin PIN section must list the other household member")
	}
}

// ---------------------------------------------------------------------------
// Self-service set/clear.
// ---------------------------------------------------------------------------

func TestPINSet_ThenClear_SelfService(t *testing.T) {
	adult := adminTestAdult()
	hhRepo := newMultiMemberHouseholdRepo(adult)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	setReq := httptest.NewRequest(http.MethodPost, "/settings/pin", strings.NewReader("csrf_token="+csrfToken+"&pin=1234"))
	setReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setReq.Header.Set("Cookie", cookie)
	setRec := httptest.NewRecorder()
	handler.ServeHTTP(setRec, setReq)
	if setRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/pin: status = %d, want 200, body: %s", setRec.Code, setRec.Body.String())
	}
	if !strings.Contains(setRec.Body.String(), "Remove PIN") {
		t.Error("after setting a PIN, the page must offer to remove it")
	}
	if !strings.Contains(setRec.Body.String(), "Change PIN") {
		t.Error("after setting a PIN, the set form must be relabeled to \"Change PIN\"")
	}

	clearReq := httptest.NewRequest(http.MethodPost, "/settings/pin/clear", strings.NewReader("csrf_token="+csrfToken))
	clearReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	clearReq.Header.Set("Cookie", cookie)
	clearRec := httptest.NewRecorder()
	handler.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/pin/clear: status = %d, want 200, body: %s", clearRec.Code, clearRec.Body.String())
	}
	if strings.Contains(clearRec.Body.String(), "Remove PIN") {
		t.Error("after clearing the PIN, the remove form must no longer render")
	}
}

func TestPINSet_InvalidFormatShowsInlineError(t *testing.T) {
	adult := adminTestAdult()
	hhRepo := newMultiMemberHouseholdRepo(adult)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/pin", strings.NewReader("csrf_token="+csrfToken+"&pin=12"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/pin with a 2-digit pin: status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), pinInvalidFormatErrorForTest) {
		t.Errorf("body must show the invalid-format message, got: %s", rec.Body.String())
	}
}

// pinInvalidFormatErrorForTest mirrors internal/auth/adapter's
// pinInvalidFormatError constant (unexported, different package) so this
// assertion does not silently pass if the message text drifts.
const pinInvalidFormatErrorForTest = "PINs must be 4 to 8 digits."

func TestPINSet_RejectsWrongCSRFToken(t *testing.T) {
	adult := adminTestAdult()
	hhRepo := newMultiMemberHouseholdRepo(adult)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/pin", strings.NewReader("csrf_token=wrong&pin=1234"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /settings/pin with a bad CSRF token: status = %d, want 403", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Owner/adult admin set + reset for another member.
// ---------------------------------------------------------------------------

func TestPINSetForMember_ByOwner_Succeeds(t *testing.T) {
	owner := settingsTestOwner()
	child := settingsTestChildInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, child)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/members/"+child.ID.String()+"/pin", strings.NewReader("csrf_token="+csrfToken+"&pin=4321"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/members/{id}/pin as owner: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "PIN set") {
		t.Error("after an owner sets a child's PIN, the admin member row must show \"PIN set\"")
	}
}

func TestPINSetForMember_ByChild_Forbidden(t *testing.T) {
	child := settingsTestChildInHousehold(household.NewHouseholdID())
	other := settingsTestChildInHousehold(child.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(child, other)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/settings/members/"+other.ID.String()+"/pin", strings.NewReader("csrf_token="+csrfToken+"&pin=4321"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /settings/members/{id}/pin as a child: status = %d, want 403", rec.Code)
	}
}

func TestPINResetForMember_ByOwner_ClearsPIN(t *testing.T) {
	owner := settingsTestOwner()
	child := settingsTestChildInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, child)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	setReq := httptest.NewRequest(http.MethodPost, "/settings/members/"+child.ID.String()+"/pin", strings.NewReader("csrf_token="+csrfToken+"&pin=4321"))
	setReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setReq.Header.Set("Cookie", cookie)
	setRec := httptest.NewRecorder()
	handler.ServeHTTP(setRec, setReq)
	if setRec.Code != http.StatusOK {
		t.Fatalf("seed: POST /settings/members/{id}/pin: status = %d, want 200", setRec.Code)
	}

	resetReq := httptest.NewRequest(http.MethodPost, "/settings/members/"+child.ID.String()+"/pin/reset", strings.NewReader("csrf_token="+csrfToken))
	resetReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resetReq.Header.Set("Cookie", cookie)
	resetRec := httptest.NewRecorder()
	handler.ServeHTTP(resetRec, resetReq)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/members/{id}/pin/reset: status = %d, want 200, body: %s", resetRec.Code, resetRec.Body.String())
	}
	if strings.Contains(resetRec.Body.String(), "PIN set") {
		t.Error("after an owner resets a child's PIN, the admin member row must no longer show \"PIN set\"")
	}
}
