package setup_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alexedwards/scs/v2"

	"github.com/ericfisherdev/nestova/internal/platform/setup"
)

// testSetupToken is the wizard token every harness is gated with; postSetup
// submits it unless the form already carries a setup_token field.
const testSetupToken = "test-setup-token"

type fakeApplier struct {
	err   error
	calls int32

	mu       sync.Mutex
	gotInput setup.Input
}

func (f *fakeApplier) Apply(_ context.Context, in setup.Input) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.gotInput = in
	f.mu.Unlock()
	return f.err
}

// input returns the last applied Input under lock, so reads in test assertions
// are race-free against the server goroutine that writes it.
func (f *fakeApplier) input() setup.Input {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotInput
}

type harness struct {
	server    *httptest.Server
	client    *http.Client
	applier   *fakeApplier
	completed *atomic.Bool
}

func newHarness(t *testing.T, applier *fakeApplier, setupToken string) *harness {
	t.Helper()
	sm := scs.New()
	var completed atomic.Bool
	onComplete := func() { completed.Store(true) }
	handlers := setup.NewHandlers(applier, sm, slog.New(slog.DiscardHandler), onComplete, setupToken)

	mux := http.NewServeMux()
	handlers.Register(mux)
	server := httptest.NewServer(sm.LoadAndSave(mux))
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	// Do not auto-follow redirects so the catch-all/redirect behaviour is observable.
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &harness{server: server, client: client, applier: applier, completed: &completed}
}

var csrfRe = regexp.MustCompile(`name="csrf_token"\s+value="([^"]*)"`)

// getCSRF performs the initial GET to obtain a session cookie (stored in the jar)
// and the embedded CSRF token.
func (h *harness) getCSRF(t *testing.T) string {
	t.Helper()
	resp, err := h.client.Get(h.server.URL + "/setup")
	if err != nil {
		t.Fatalf("GET /setup: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	m := csrfRe.FindSubmatch(body)
	if m == nil || len(m[1]) == 0 {
		t.Fatalf("no CSRF token in form:\n%s", body)
	}
	return string(m[1])
}

// postSetup submits form with the harness's setup token filled in, unless the
// form already sets one, so tests only spell out the token when it is the subject.
func (h *harness) postSetup(t *testing.T, form url.Values) (*http.Response, string) {
	t.Helper()
	if !form.Has("setup_token") {
		form.Set("setup_token", testSetupToken)
	}
	return h.postSetupRaw(t, form)
}

// postSetupRaw submits form exactly as given.
func (h *harness) postSetupRaw(t *testing.T, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := h.client.PostForm(h.server.URL+"/setup", form)
	if err != nil {
		t.Fatalf("POST /setup: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestGetSetup_RendersForm(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, testSetupToken)
	resp, err := h.client.Get(h.server.URL + "/setup")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `action="/setup"`) {
		t.Fatalf("form action missing:\n%s", body)
	}
	if !csrfRe.Match(body) {
		t.Fatal("CSRF token field missing")
	}
	if !strings.Contains(string(body), `name="setup_token"`) {
		t.Fatalf("setup_token input missing:\n%s", body)
	}
}

func TestNewHandlers_PanicsOnEmptyToken(t *testing.T) {
	for _, token := range []string{"", "   "} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewHandlers did not panic for token %q", token)
				}
			}()
			setup.NewHandlers(&fakeApplier{}, scs.New(), slog.New(slog.DiscardHandler), func() {}, token)
		}()
	}
}

func TestCatchAll_RedirectsToSetup(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, testSetupToken)
	resp, err := h.client.Get(h.server.URL + "/something/else")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/setup" {
		t.Fatalf("Location = %q, want /setup", loc)
	}
}

func TestPostSetup_InvalidCSRF_Forbidden(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, testSetupToken)
	// No prior GET and no csrf_token field -> rejected before any work.
	resp, _ := h.postSetup(t, url.Values{"host": {"localhost"}, "database": {"db"}, "user": {"u"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if n := atomic.LoadInt32(&h.applier.calls); n != 0 {
		t.Fatalf("service called %d times on CSRF failure, want 0", n)
	}
}

func TestPostSetup_Success_AppliesAndSignalsComplete(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, testSetupToken)
	csrf := h.getCSRF(t)
	resp, body := h.postSetup(t, url.Values{
		"csrf_token": {csrf},
		"host":       {"localhost"},
		"port":       {"5434"},
		"database":   {"nestova_test"},
		"user":       {"nestova"},
		"password":   {"nestova_test_pw"},
		"sslmode":    {"disable"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "/onboarding") {
		t.Fatalf("completion page should point to /onboarding:\n%s", body)
	}
	if n := atomic.LoadInt32(&h.applier.calls); n != 1 {
		t.Fatalf("service called %d times, want 1", n)
	}
	got := h.applier.input()
	if got.Host != "localhost" || got.Password != "nestova_test_pw" {
		t.Fatalf("service got unexpected input: %+v", got)
	}
	if !h.completed.Load() {
		t.Fatal("onComplete was not invoked on success")
	}
}

func TestPostSetup_PlumbsProviderFields(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, testSetupToken)
	csrf := h.getCSRF(t)
	resp, body := h.postSetup(t, url.Values{
		"csrf_token":    {csrf},
		"provider":      {"supabase"},
		"host":          {"db.supabase.co"},
		"port":          {"6543"},
		"database":      {"postgres"},
		"user":          {"postgres"},
		"password":      {"pw"},
		"sslmode":       {"require"},
		"pool_mode":     {"transaction"},
		"ssl_root_cert": {"/etc/ssl/ca.crt"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	in := h.applier.input()
	if in.Provider != "supabase" || in.PoolMode != "transaction" || in.SSLRootCert != "/etc/ssl/ca.crt" {
		t.Fatalf("provider fields not plumbed into Input: %+v", in)
	}
}

func TestPostSetup_ConnectFailure_422_NoPasswordEcho(t *testing.T) {
	h := newHarness(t, &fakeApplier{err: fmt.Errorf("%w: dial tcp", setup.ErrConnect)}, testSetupToken)
	csrf := h.getCSRF(t)
	resp, body := h.postSetup(t, url.Values{
		"csrf_token": {csrf},
		"host":       {"localhost"},
		"database":   {"db"},
		"user":       {"u"},
		"password":   {"sup3r-secret-pw"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "Could not connect") {
		t.Fatalf("expected connect error message:\n%s", body)
	}
	if strings.Contains(body, "sup3r-secret-pw") {
		t.Fatal("password was echoed back into the form")
	}
	if h.completed.Load() {
		t.Fatal("onComplete must not fire on failure")
	}
}

func TestPostSetup_RawDSNCredentialsNotEchoed(t *testing.T) {
	h := newHarness(t, &fakeApplier{err: fmt.Errorf("%w: dial tcp", setup.ErrConnect)}, testSetupToken)
	csrf := h.getCSRF(t)
	resp, body := h.postSetup(t, url.Values{
		"csrf_token": {csrf},
		"raw_dsn":    {"postgres://admin:raw-dsn-secret@db.example:5432/app?sslmode=require"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	// A raw DSN can embed credentials; they must never be reflected into the form.
	if strings.Contains(body, "raw-dsn-secret") {
		t.Fatal("raw DSN credentials were echoed back into the form")
	}
}

func TestPostSetup_SetupTokenGate(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, "secret-token")

	// Wrong token -> 403, service not called.
	csrf := h.getCSRF(t)
	resp, body := h.postSetup(t, url.Values{
		"csrf_token":  {csrf},
		"setup_token": {"wrong"},
		"host":        {"localhost"},
		"database":    {"db"},
		"user":        {"u"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "Incorrect setup token") {
		t.Fatalf("expected token error:\n%s", body)
	}
	if n := atomic.LoadInt32(&h.applier.calls); n != 0 {
		t.Fatalf("service called %d times with wrong token, want 0", n)
	}

	// Correct token -> proceeds to Apply.
	csrf = h.getCSRF(t)
	resp, body = h.postSetup(t, url.Values{
		"csrf_token":  {csrf},
		"setup_token": {"secret-token"},
		"host":        {"localhost"},
		"database":    {"db"},
		"user":        {"u"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct token status = %d, want 200:\n%s", resp.StatusCode, body)
	}
	if n := atomic.LoadInt32(&h.applier.calls); n != 1 {
		t.Fatalf("service called %d times with correct token, want 1", n)
	}
}

func TestPostSetup_MissingToken_Forbidden_NoApply(t *testing.T) {
	h := newHarness(t, &fakeApplier{}, testSetupToken)
	csrf := h.getCSRF(t)
	// No setup_token field at all, as a scripted prober would send.
	resp, body := h.postSetupRaw(t, url.Values{
		"csrf_token": {csrf},
		"host":       {"192.0.2.1"},
		"port":       {"5432"},
		"database":   {"db"},
		"user":       {"u"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "Incorrect setup token") {
		t.Fatalf("expected token error:\n%s", body)
	}
	if n := atomic.LoadInt32(&h.applier.calls); n != 0 {
		t.Fatalf("service called %d times without a token, want 0", n)
	}
}

// TestPostSetup_ConnectFailures_IdenticalResponse guards against the wizard
// working as a probing oracle: a refused connection, a timeout and a rejected
// password must be indistinguishable in the response, with the cause only in the
// server log.
func TestPostSetup_ConnectFailures_IdenticalResponse(t *testing.T) {
	causes := []string{
		"dial tcp 192.0.2.1:5432: connect: connection refused",
		"dial tcp 192.0.2.1:5432: i/o timeout",
		"FATAL: password authentication failed for user \"u\" (SQLSTATE 28P01)",
	}
	var wantStatus int
	var wantBody string
	for i, cause := range causes {
		h := newHarness(t, &fakeApplier{err: fmt.Errorf("%w: %s", setup.ErrConnect, cause)}, testSetupToken)
		csrf := h.getCSRF(t)
		resp, body := h.postSetup(t, url.Values{
			"csrf_token": {csrf},
			"host":       {"192.0.2.1"},
			"port":       {"5432"},
			"database":   {"db"},
			"user":       {"u"},
			"password":   {"pw"},
		})
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("cause %q: status = %d, want 422", cause, resp.StatusCode)
		}
		for _, leak := range []string{"refused", "timeout", "authentication", "SQLSTATE"} {
			if strings.Contains(body, leak) {
				t.Fatalf("cause %q: response leaks %q:\n%s", cause, leak, body)
			}
		}
		if i == 0 {
			wantStatus, wantBody = resp.StatusCode, normalizeCSRF(body)
			continue
		}
		if resp.StatusCode != wantStatus {
			t.Fatalf("cause %q: status = %d, want %d", cause, resp.StatusCode, wantStatus)
		}
		if got := normalizeCSRF(body); got != wantBody {
			t.Fatalf("cause %q: body differs from the first failure's:\n%s\n--- vs ---\n%s", cause, got, wantBody)
		}
	}
}

// normalizeCSRF blanks the per-session CSRF token, the only part of the form
// that legitimately differs between two sessions.
func normalizeCSRF(body string) string {
	return csrfRe.ReplaceAllString(body, `name="csrf_token" value=""`)
}
