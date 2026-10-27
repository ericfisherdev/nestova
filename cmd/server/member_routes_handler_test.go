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
	}{
		{household.RoleOwner, http.StatusOK},
		{household.RoleAdult, http.StatusOK},
		{household.RoleChild, http.StatusForbidden},
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

			// No CSRF token is sent: the route middleware must refuse a child
			// with its own "forbidden" body before the handler's CSRF check
			// ("invalid CSRF token") could ever run.
			postReq := httptest.NewRequest(http.MethodPost, "/members", strings.NewReader(url.Values{}.Encode()))
			postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			postReq.Header.Set("Cookie", cookie)
			postRec := httptest.NewRecorder()
			handler.ServeHTTP(postRec, postReq)
			gotMiddlewareRefusal := postRec.Code == http.StatusForbidden &&
				strings.TrimSpace(postRec.Body.String()) == "forbidden"
			if wantRefusal := tt.role == household.RoleChild; gotMiddlewareRefusal != wantRefusal {
				t.Errorf("POST /members middleware refusal = %v, want %v (status %d, body %q)",
					gotMiddlewareRefusal, wantRefusal, postRec.Code, postRec.Body.String())
			}
		})
	}
}
