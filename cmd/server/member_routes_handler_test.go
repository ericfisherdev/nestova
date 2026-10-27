package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func newRoleTestMember(role household.Role) *household.Member {
	return &household.Member{
		ID:          household.NewMemberID(),
		HouseholdID: household.NewHouseholdID(),
		DisplayName: "Caller",
		Role:        role,
		Color:       household.ColorSage,
	}
}

// TestMemberRoutes_RequireAdministrator drives the production route table
// (registerWebRoutes) so removing RequireAdministrator from the member routes
// fails here, independent of the handlers' own defence-in-depth checks
// (NES-85).
func TestMemberRoutes_RequireAdministrator(t *testing.T) {
	tests := []struct {
		role     household.Role
		wantPage int
		// wantPostBody distinguishes who refused the token-less POST: the
		// route middleware ("forbidden") or, for an administrator that got
		// past it, the handler's own CSRF check.
		wantPostBody string
	}{
		{household.RoleOwner, http.StatusOK, "invalid CSRF token"},
		{household.RoleAdult, http.StatusOK, "invalid CSRF token"},
		{household.RoleChild, http.StatusForbidden, "forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.role.String(), func(t *testing.T) {
			member := newRoleTestMember(tt.role)
			handler, sm := buildCreateTaskHandler(&recordingTaskRepo{}, authedHouseholdRepo{member: member})
			cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

			getReq := httptest.NewRequest(http.MethodGet, "/members/new", nil)
			getReq.Header.Set("Cookie", cookie)
			getRec := httptest.NewRecorder()
			handler.ServeHTTP(getRec, getReq)
			if getRec.Code != tt.wantPage {
				t.Errorf("GET /members/new status = %d, want %d", getRec.Code, tt.wantPage)
			}

			// No CSRF token is sent, so the POST is always refused with 403; the
			// body says which layer refused it. A child must be stopped by the
			// route middleware before the handler's CSRF check could run, and
			// an administrator must reach the handler (a dropped route or a
			// misfiring middleware would produce another status or body).
			postReq := httptest.NewRequest(http.MethodPost, "/members", strings.NewReader(url.Values{}.Encode()))
			postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			postReq.Header.Set("Cookie", cookie)
			postRec := httptest.NewRecorder()
			handler.ServeHTTP(postRec, postReq)
			if postRec.Code != http.StatusForbidden {
				t.Errorf("POST /members status = %d, want 403", postRec.Code)
			}
			if got := strings.TrimSpace(postRec.Body.String()); got != tt.wantPostBody {
				t.Errorf("POST /members body = %q, want %q", got, tt.wantPostBody)
			}
		})
	}
}
