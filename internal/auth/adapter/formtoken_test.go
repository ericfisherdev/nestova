package adapter_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"github.com/ericfisherdev/nestova/internal/auth/adapter"
)

// formTokenSession drives requests through one session so a token issued by
// one request is visible to the next, as it is for a browser.
type formTokenSession struct {
	t       *testing.T
	sm      *scs.SessionManager
	cookies []*http.Cookie
}

func newFormTokenSession(t *testing.T) *formTokenSession {
	t.Helper()
	return &formTokenSession{t: t, sm: newOnboardingSessionManager()}
}

// do runs fn inside a request that carries the session cookie, and keeps the
// cookie the response sets.
func (s *formTokenSession) do(form url.Values, fn func(r *http.Request)) {
	s.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range s.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.sm.LoadAndSave(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			s.t.Fatalf("ParseForm: %v", err)
		}
		fn(r)
	})).ServeHTTP(rec, req)
	if cookies := rec.Result().Cookies(); len(cookies) > 0 {
		s.cookies = cookies
	}
}

func (s *formTokenSession) issue() string {
	s.t.Helper()
	var token string
	s.do(nil, func(r *http.Request) { token = adapter.IssueFormToken(r.Context(), s.sm) })
	return token
}

func (s *formTokenSession) has(token string) bool {
	s.t.Helper()
	var got bool
	s.do(url.Values{adapter.FormTokenField: {token}}, func(r *http.Request) { got = adapter.HasFormToken(r, s.sm) })
	return got
}

func (s *formTokenSession) consume(token string) {
	s.t.Helper()
	s.do(url.Values{adapter.FormTokenField: {token}}, func(r *http.Request) { adapter.ConsumeFormToken(r, s.sm) })
}

func TestFormToken_IssuedTokenIsAccepted(t *testing.T) {
	s := newFormTokenSession(t)
	token := s.issue()

	if len(token) != 32 {
		t.Errorf("token length = %d, want 32", len(token))
	}
	if !s.has(token) {
		t.Error("HasFormToken = false for a freshly issued token")
	}
}

func TestFormToken_ConsumedTokenIsRejected(t *testing.T) {
	s := newFormTokenSession(t)
	token := s.issue()

	s.consume(token)

	if s.has(token) {
		t.Error("HasFormToken = true after ConsumeFormToken, want a spent token rejected")
	}
}

func TestFormToken_HasDoesNotConsume(t *testing.T) {
	s := newFormTokenSession(t)
	token := s.issue()

	s.has(token)

	if !s.has(token) {
		t.Error("HasFormToken consumed the token; a submission that fails validation could not be retried")
	}
}

func TestFormToken_UnknownOrMissingTokenIsRejected(t *testing.T) {
	s := newFormTokenSession(t)
	s.issue()

	for name, token := range map[string]string{"unknown": strings.Repeat("a", 32), "missing": ""} {
		if s.has(token) {
			t.Errorf("HasFormToken = true for %s token", name)
		}
	}
}

func TestFormToken_ConsumingOneLeavesOtherFormsValid(t *testing.T) {
	s := newFormTokenSession(t)
	first, second := s.issue(), s.issue()

	s.consume(first)

	if !s.has(second) {
		t.Error("consuming one token invalidated a token issued for another open form")
	}
}

func TestFormToken_OldestTokenIsDroppedAtTheCap(t *testing.T) {
	s := newFormTokenSession(t)
	oldest := s.issue()
	var newest string
	for i := 0; i < 16; i++ {
		newest = s.issue()
	}

	if s.has(oldest) {
		t.Error("oldest token still valid after the pending cap was exceeded")
	}
	if !s.has(newest) {
		t.Error("newest token rejected")
	}
}
