package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

func serveThroughRequireMember(t *testing.T, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	handler := RequireMember(scs.New())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/rewards", nil)
	if authenticated {
		req = req.WithContext(context.WithValue(req.Context(), currentMemberKey, &household.Member{}))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRequireMemberSendsNoStoreToAuthenticatedMembers(t *testing.T) {
	rec := serveThroughRequireMember(t, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want it to contain no-store", got)
	}
}

func TestRequireMemberRedirectsAnonymousRequestsWithoutCacheHeader(t *testing.T) {
	rec := serveThroughRequireMember(t, false)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control = %q on a redirect, want none", got)
	}
}
