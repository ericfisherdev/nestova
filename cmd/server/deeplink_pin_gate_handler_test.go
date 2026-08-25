package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	deeplinkdomain "github.com/ericfisherdev/nestova/internal/deeplink/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	tasksdomain "github.com/ericfisherdev/nestova/internal/tasks/domain"
)

// ---------------------------------------------------------------------------
// NES-166: the complete-task deep link enforces the same PIN gate the /tasks
// chore row does — a notification link must not be a way around it.
// ---------------------------------------------------------------------------

// enrolPIN gives memberID a PIN in the fixture's own PIN store.
func (f *deepLinkFixture) enrolPIN(t *testing.T, member *household.Member, pin string) {
	t.Helper()
	if err := f.pinService.Set(context.Background(), member.ID, member.HouseholdID, pin); err != nil {
		t.Fatalf("enrol PIN: %v", err)
	}
}

// confirmCompleteWithPIN walks the real flow — GET the signed confirm screen,
// take its CSRF token, POST back to the same signed URL — submitting pin when
// non-empty. It returns both responses so a test can assert on the rendered
// screen and the action's outcome.
func (f *deepLinkFixture) confirmCompleteWithPIN(
	t *testing.T,
	member *household.Member,
	instanceID tasksdomain.TaskInstanceID,
	pin string,
) (getRec, postRec *httptest.ResponseRecorder) {
	t.Helper()
	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionCompleteTask, instanceID.String())

	getReq := httptest.NewRequest(http.MethodGet, link, nil)
	getReq.Header.Set("Cookie", cookie)
	getRec = httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm: status = %d, want 200; body: %s", getRec.Code, getRec.Body.String())
	}

	form := "csrf_token=" + extractCSRFToken(getRec.Body.String())
	if pin != "" {
		form += "&pin=" + pin
	}
	postReq := httptest.NewRequest(http.MethodPost, link, strings.NewReader(form))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec = httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)
	return getRec, postRec
}

// TestDeepLinkComplete_EnrolledAssignee_CorrectPINCompletes proves the
// confirm screen offers a PIN field for a gated chore and that the
// assignee's own PIN completes it, crediting the assignee.
func TestDeepLinkComplete_EnrolledAssignee_CorrectPINCompletes(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	f.enrolPIN(t, member, "4821")
	instanceID := f.seedInstance(member, "Water the plants", &member.ID)

	getRec, postRec := f.confirmCompleteWithPIN(t, member, instanceID, "4821")

	if !strings.Contains(getRec.Body.String(), `data-testid="deeplink-pin-input"`) {
		t.Errorf("confirm screen missing the PIN field for a gated chore: %s", getRec.Body.String())
	}
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST confirm: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
	if err != nil {
		t.Fatalf("Get after complete: %v", err)
	}
	if inst.Status != tasksdomain.StatusDone {
		t.Errorf("instance status = %v, want done", inst.Status)
	}
	if inst.CompletedBy == nil || *inst.CompletedBy != member.ID {
		t.Errorf("CompletedBy = %v, want the assignee %v", inst.CompletedBy, member.ID)
	}
}

// TestDeepLinkComplete_EnrolledAssignee_WrongPINCompletesNothing proves a
// refused PIN re-renders the confirm screen at 403 with a non-disclosing
// message and leaves the chore pending — following the link alone, or
// confirming it without the PIN, still completes nothing.
func TestDeepLinkComplete_EnrolledAssignee_WrongPINCompletesNothing(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	f.enrolPIN(t, member, "4821")
	instanceID := f.seedInstance(member, "Water the plants", &member.ID)

	for _, tc := range []struct{ name, pin string }{
		{"wrong pin", "9999"},
		{"no pin", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, postRec := f.confirmCompleteWithPIN(t, member, instanceID, tc.pin)

			if postRec.Code != http.StatusForbidden {
				t.Fatalf("POST confirm: status = %d, want 403; body: %s", postRec.Code, postRec.Body.String())
			}
			body := postRec.Body.String()
			if !strings.Contains(body, "That PIN could not be verified.") {
				t.Errorf("response missing the non-disclosing PIN error: %s", body)
			}
			if !strings.Contains(body, `data-testid="deeplink-pin-input"`) {
				t.Errorf("response is not the re-rendered confirm screen: %s", body)
			}
			inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
			if err != nil {
				t.Fatalf("Get after refused confirm: %v", err)
			}
			if inst.Status != tasksdomain.StatusPending {
				t.Errorf("instance status = %v, want pending — a refused PIN must complete nothing", inst.Status)
			}
		})
	}
}

// TestDeepLinkComplete_UnenrolledAssignee_NoPINFieldAndUnchangedFlow is the
// upgrade path: with no PIN enrolled the confirm screen is byte-for-byte the
// pre-NES-166 one and the completion succeeds without any PIN.
func TestDeepLinkComplete_UnenrolledAssignee_NoPINFieldAndUnchangedFlow(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	instanceID := f.seedInstance(member, "Water the plants", &member.ID)

	getRec, postRec := f.confirmCompleteWithPIN(t, member, instanceID, "")

	if strings.Contains(getRec.Body.String(), `data-testid="deeplink-pin-input"`) {
		t.Errorf("confirm screen rendered a PIN field for an unenrolled assignee: %s", getRec.Body.String())
	}
	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST confirm: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
	if err != nil {
		t.Fatalf("Get after complete: %v", err)
	}
	if inst.Status != tasksdomain.StatusDone {
		t.Errorf("instance status = %v, want done", inst.Status)
	}
}

// TestDeepLinkClaim_IsNotPINGated pins the scope boundary: claiming an
// unassigned chore is deliberately out of scope — there is no owner to
// impersonate, and the claim itself is what creates ownership.
func TestDeepLinkClaim_IsNotPINGated(t *testing.T) {
	member := testMember()
	f := buildDeepLinkFixture(t, member)
	f.enrolPIN(t, member, "4821")
	instanceID := f.seedInstance(member, "Sweep the porch", nil)

	cookie, _ := seedAuthedSession(t, f.handler, f.sm, member.ID.String())
	link := f.mintURL(t, deeplinkdomain.ActionClaimTask, instanceID.String())

	getReq := httptest.NewRequest(http.MethodGet, link, nil)
	getReq.Header.Set("Cookie", cookie)
	getRec := httptest.NewRecorder()
	f.handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET confirm: status = %d, want 200", getRec.Code)
	}
	if strings.Contains(getRec.Body.String(), `data-testid="deeplink-pin-input"`) {
		t.Errorf("claim confirm screen must not ask for a PIN: %s", getRec.Body.String())
	}

	postReq := httptest.NewRequest(http.MethodPost, link,
		strings.NewReader("csrf_token="+extractCSRFToken(getRec.Body.String())))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Cookie", cookie)
	postRec := httptest.NewRecorder()
	f.handler.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST claim: status = %d, want 303; body: %s", postRec.Code, postRec.Body.String())
	}
	inst, err := f.instances.Get(context.Background(), member.HouseholdID, instanceID)
	if err != nil {
		t.Fatalf("Get after claim: %v", err)
	}
	if inst.AssigneeID == nil || *inst.AssigneeID != member.ID {
		t.Errorf("claim did not assign the instance: assignee = %v", inst.AssigneeID)
	}
}
