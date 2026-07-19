package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	subscriptionsadapter "github.com/ericfisherdev/nestova/internal/subscriptions/adapter"
	subscriptionsapp "github.com/ericfisherdev/nestova/internal/subscriptions/app"
	subscriptionsdomain "github.com/ericfisherdev/nestova/internal/subscriptions/domain"
)

// fakeSubscriptionRepo is an in-memory subscriptions repository for handler tests.
type fakeSubscriptionRepo struct {
	created     []*subscriptionsdomain.Subscription
	deactivated []subscriptionsdomain.SubscriptionID
	getResult   *subscriptionsdomain.Subscription
	active      []*subscriptionsdomain.Subscription
}

func (f *fakeSubscriptionRepo) Create(_ context.Context, s *subscriptionsdomain.Subscription) error {
	f.created = append(f.created, s)
	return nil
}

func (f *fakeSubscriptionRepo) Get(context.Context, subscriptionsdomain.SubscriptionID) (*subscriptionsdomain.Subscription, error) {
	if f.getResult != nil {
		return f.getResult, nil
	}
	return nil, subscriptionsdomain.ErrSubscriptionNotFound
}

func (f *fakeSubscriptionRepo) Update(context.Context, *subscriptionsdomain.Subscription) error {
	return nil
}

func (f *fakeSubscriptionRepo) Deactivate(_ context.Context, id subscriptionsdomain.SubscriptionID) error {
	f.deactivated = append(f.deactivated, id)
	return nil
}

func (f *fakeSubscriptionRepo) ListActiveByHousehold(context.Context, household.HouseholdID) ([]*subscriptionsdomain.Subscription, error) {
	return f.active, nil
}

func (f *fakeSubscriptionRepo) ListDueForRenewal(context.Context, time.Time) ([]*subscriptionsdomain.Subscription, error) {
	return nil, nil
}

func buildSubscriptionsTestHandler(t *testing.T, member *household.Member, repo *fakeSubscriptionRepo) (http.Handler, *scs.SessionManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	householdRepo := authedHouseholdRepo{member: member}

	authn := authapp.New(testCredRepo{})
	authHandlers := authadapter.NewHandlers(sm, authn, nil, nil, nil, logger)

	subService, err := subscriptionsapp.NewSubscriptionService(repo)
	if err != nil {
		t.Fatalf("NewSubscriptionService: %v", err)
	}
	costService := subscriptionsapp.NewCostService(repo)
	subsHandlers := subscriptionsadapter.NewWebHandlers(subService, costService, householdRepo, sm, logger)

	// A trivial layout that renders the content unwrapped — enough to exercise Page.
	layoutFn := func(*household.Member) func(templ.Component) templ.Component {
		return func(c templ.Component) templ.Component { return c }
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	requireMember := authadapter.RequireMember(sm)
	mux.Handle("GET /subscriptions", requireMember(http.HandlerFunc(subsHandlers.Page(layoutFn))))
	mux.Handle("POST /subscriptions", requireMember(http.HandlerFunc(subsHandlers.Add)))
	mux.Handle("POST /subscriptions/{id}/deactivate", requireMember(http.HandlerFunc(subsHandlers.Deactivate)))

	return sm.LoadAndSave(authadapter.Authenticate(sm, householdRepo)(mux)), sm
}

func validSubscriptionForm(csrf string) string {
	form := url.Values{
		"csrf_token":         {csrf},
		"name":               {"Streaming Plus"},
		"amount":             {"9.99"},
		"currency":           {"USD"},
		"cycle":              {"monthly"},
		"next_renewal_on":    {"2026-07-20"},
		"reminder_lead_days": {"3"},
	}
	return form.Encode()
}

func TestSubscriptionAddRejectsBadCSRF(t *testing.T) {
	member := testMember()
	repo := &fakeSubscriptionRepo{}
	handler, sm := buildSubscriptionsTestHandler(t, member, repo)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader(validSubscriptionForm("wrong")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("add with bad CSRF: status = %d, want 403", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Errorf("bad-CSRF add must not persist, got %d", len(repo.created))
	}
}

func TestSubscriptionAddRequiresMember(t *testing.T) {
	member := testMember()
	handler, _ := buildSubscriptionsTestHandler(t, member, &fakeSubscriptionRepo{})

	// No session cookie -> unauthenticated. An HTMX request gets a 401 (a full
	// navigation would be redirected to /login instead).
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader("csrf_token=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated HTMX add: status = %d, want 401", rec.Code)
	}
}

func TestSubscriptionAddPersistsAndRedirects(t *testing.T) {
	member := testMember()
	repo := &fakeSubscriptionRepo{}
	handler, sm := buildSubscriptionsTestHandler(t, member, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader(validSubscriptionForm(csrf)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("add: status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("HX-Redirect") != "/subscriptions" {
		t.Fatalf("add HX-Redirect = %q, want /subscriptions", rec.Header().Get("HX-Redirect"))
	}
	if len(repo.created) != 1 {
		t.Fatalf("add persisted %d subscriptions, want 1", len(repo.created))
	}
	got := repo.created[0]
	if got.Name != "Streaming Plus" || got.Amount.Cents != 999 || got.HouseholdID != member.HouseholdID {
		t.Fatalf("persisted subscription = %+v", got)
	}
}

func TestSubscriptionDeactivate(t *testing.T) {
	member := testMember()
	id := subscriptionsdomain.NewSubscriptionID()
	// The subscription belongs to the member's household, so the ownership check passes.
	repo := &fakeSubscriptionRepo{getResult: &subscriptionsdomain.Subscription{ID: id, HouseholdID: member.HouseholdID}}
	handler, sm := buildSubscriptionsTestHandler(t, member, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+id.String()+"/deactivate", strings.NewReader("csrf_token="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deactivate: status = %d, want 303", rec.Code)
	}
	if len(repo.deactivated) != 1 || repo.deactivated[0] != id {
		t.Fatalf("deactivate recorded %v, want [%s]", repo.deactivated, id)
	}
}

func TestSubscriptionAddRejectsNaNAmount(t *testing.T) {
	member := testMember()
	repo := &fakeSubscriptionRepo{}
	handler, sm := buildSubscriptionsTestHandler(t, member, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	form := url.Values{
		"csrf_token": {csrf}, "name": {"X"}, "amount": {"NaN"}, "currency": {"USD"},
		"cycle": {"monthly"}, "next_renewal_on": {"2026-07-20"},
	}
	req := httptest.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("NaN amount: status = %d, want 400", rec.Code)
	}
	if len(repo.created) != 0 {
		t.Fatalf("NaN amount must not persist, got %d", len(repo.created))
	}
}

func TestSubscriptionsPageMixedCurrencyDoesNotError(t *testing.T) {
	member := testMember()
	usd, _ := household.NewMoney(100, "USD")
	eur, _ := household.NewMoney(100, "EUR")
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	// Two active subscriptions in different currencies: the rollup cannot sum, but
	// the page must still render rather than 500.
	repo := &fakeSubscriptionRepo{active: []*subscriptionsdomain.Subscription{
		{ID: subscriptionsdomain.NewSubscriptionID(), HouseholdID: member.HouseholdID, Name: "USD sub", Amount: usd, Cycle: subscriptionsdomain.CycleMonthly, NextRenewalOn: day, Active: true},
		{ID: subscriptionsdomain.NewSubscriptionID(), HouseholdID: member.HouseholdID, Name: "EUR sub", Amount: eur, Cycle: subscriptionsdomain.CycleMonthly, NextRenewalOn: day, Active: true},
	}}
	handler, sm := buildSubscriptionsTestHandler(t, member, repo)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/subscriptions", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("mixed-currency page: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Mixed currencies") {
		t.Fatalf("mixed-currency page missing fallback label")
	}
}

func TestSubscriptionDeactivateRejectsOtherHousehold(t *testing.T) {
	member := testMember()
	id := subscriptionsdomain.NewSubscriptionID()
	// The subscription belongs to a DIFFERENT household: the request must be
	// rejected as not-found and not deactivated.
	repo := &fakeSubscriptionRepo{getResult: &subscriptionsdomain.Subscription{ID: id, HouseholdID: household.NewHouseholdID()}}
	handler, sm := buildSubscriptionsTestHandler(t, member, repo)
	cookie, csrf := seedAuthedSession(t, handler, sm, member.ID.String())

	req := httptest.NewRequest(http.MethodPost, "/subscriptions/"+id.String()+"/deactivate", strings.NewReader("csrf_token="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-household deactivate: status = %d, want 404", rec.Code)
	}
	if len(repo.deactivated) != 0 {
		t.Fatalf("cross-household deactivate must not deactivate, recorded %v", repo.deactivated)
	}
}
