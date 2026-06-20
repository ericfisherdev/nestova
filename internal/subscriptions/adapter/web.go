package adapter

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/internal/subscriptions/app"
	"github.com/ericfisherdev/nestova/internal/subscriptions/domain"
	"github.com/ericfisherdev/nestova/web/components"
)

// subscriptionsPath is the canonical page path; mutations redirect back here.
const subscriptionsPath = "/subscriptions"

// dateLayout is the YYYY-MM-DD layout of the next-renewal date input.
const dateLayout = "2006-01-02"

// displayDateLayout is the human-readable date layout shown in the UI.
const displayDateLayout = "Jan 2, 2006"

// maxAmountUnits caps the major-unit amount so amount*100 cannot overflow int64
// (math.MaxInt64 / 100).
const maxAmountUnits = 92233720368547758.0

// mixedCurrencyLabel is shown for the monthly rollup when a household's active
// subscriptions span more than one currency (no single total exists).
const mixedCurrencyLabel = "Mixed currencies"

// LayoutFunc wraps page content in the app shell; home.go provides it.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// WebHandlers serves the /subscriptions UI: the list with a monthly cost rollup
// and the add/edit/deactivate actions.
type WebHandlers struct {
	subs       *app.SubscriptionService
	cost       *app.CostService
	households household.HouseholdRepository
	sm         *scs.SessionManager
	logger     *slog.Logger
}

// NewWebHandlers constructs a WebHandlers, panicking on a nil dependency.
func NewWebHandlers(subs *app.SubscriptionService, cost *app.CostService, households household.HouseholdRepository, sm *scs.SessionManager, logger *slog.Logger) *WebHandlers {
	switch {
	case subs == nil:
		panic("subscriptions/adapter: NewWebHandlers requires a non-nil SubscriptionService")
	case cost == nil:
		panic("subscriptions/adapter: NewWebHandlers requires a non-nil CostService")
	case households == nil:
		panic("subscriptions/adapter: NewWebHandlers requires a non-nil HouseholdRepository")
	case sm == nil:
		panic("subscriptions/adapter: NewWebHandlers requires a non-nil session manager")
	case logger == nil:
		panic("subscriptions/adapter: NewWebHandlers requires a non-nil logger")
	}
	return &WebHandlers{subs: subs, cost: cost, households: households, sm: sm, logger: logger}
}

// Page handles GET /subscriptions: the active list, the monthly cost rollup, and
// the add form.
func (h *WebHandlers) Page(layoutFn LayoutFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		view, err := h.buildView(r, member)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "subscriptions: build view", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := render.Page(r.Context(), w, r, layoutFn(member), components.SubscriptionsPage(view)); err != nil {
			h.logger.ErrorContext(r.Context(), "subscriptions: render page", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

// Add handles POST /subscriptions.
func (h *WebHandlers) Add(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	in, err := parseSubscriptionInput(r)
	if err != nil {
		http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := h.subs.Add(r.Context(), member.HouseholdID, in); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, subscriptionsPath)
}

// Edit handles POST /subscriptions/{id}.
func (h *WebHandlers) Edit(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseSubscriptionID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid subscription id", http.StatusBadRequest)
		return
	}
	in, err := parseSubscriptionInput(r)
	if err != nil {
		http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.subs.Edit(r.Context(), member.HouseholdID, id, in); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, subscriptionsPath)
}

// Deactivate handles POST /subscriptions/{id}/deactivate.
func (h *WebHandlers) Deactivate(w http.ResponseWriter, r *http.Request) {
	member, ok := h.beginMutation(w, r)
	if !ok {
		return
	}
	id, err := domain.ParseSubscriptionID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid subscription id", http.StatusBadRequest)
		return
	}
	if err := h.subs.Deactivate(r.Context(), member.HouseholdID, id); err != nil {
		h.handleMutationError(w, r, err)
		return
	}
	respondAfterMutation(w, r, subscriptionsPath)
}

func (h *WebHandlers) buildView(r *http.Request, member *household.Member) (components.SubscriptionsView, error) {
	subs, err := h.subs.ListActive(r.Context(), member.HouseholdID)
	if err != nil {
		return components.SubscriptionsView{}, fmt.Errorf("list active: %w", err)
	}
	// A mixed-currency household has no single monthly total; show a label and
	// still render the subscriptions rather than failing the whole page.
	monthlyLabel := mixedCurrencyLabel
	switch monthly, err := h.cost.MonthlyCost(r.Context(), member.HouseholdID); {
	case err == nil:
		monthlyLabel = monthly.String()
	case errors.Is(err, household.ErrCurrencyMismatch):
		// keep the mixed-currency label
	default:
		return components.SubscriptionsView{}, fmt.Errorf("monthly cost: %w", err)
	}
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return components.SubscriptionsView{}, fmt.Errorf("list members: %w", err)
	}

	memberName := make(map[household.MemberID]*household.Member, len(members))
	memberOptions := make([]components.SubscriptionMemberOption, 0, len(members))
	for _, m := range members {
		memberName[m.ID] = m
		memberOptions = append(memberOptions, components.SubscriptionMemberOption{ID: m.ID.String(), Name: m.DisplayName, Color: m.Color.String()})
	}

	rows := make([]components.SubscriptionRow, 0, len(subs))
	for _, s := range subs {
		row := components.SubscriptionRow{
			ID:               s.ID.String(),
			Name:             s.Name,
			AmountLabel:      s.Amount.String(),
			CycleLabel:       s.Cycle.String(),
			NextRenewal:      s.NextRenewalOn.Format(displayDateLayout),
			Category:         s.Category,
			AmountValue:      fmt.Sprintf("%d.%02d", s.Amount.Cents/100, s.Amount.Cents%100),
			CurrencyValue:    s.Amount.Currency,
			CycleValue:       s.Cycle.String(),
			NextRenewalValue: s.NextRenewalOn.Format(dateLayout),
			ReminderLeadDays: s.ReminderLeadDays,
		}
		if s.PayerID != nil {
			row.PayerValue = s.PayerID.String()
			if m, ok := memberName[*s.PayerID]; ok {
				row.PayerName = m.DisplayName
				row.PayerColor = m.Color.String()
			}
		}
		rows = append(rows, row)
	}

	cycles := make([]components.SubscriptionCycleOption, 0, len(domain.Cycles()))
	for _, c := range domain.Cycles() {
		cycles = append(cycles, components.SubscriptionCycleOption{Value: c.String(), Label: c.String()})
	}

	return components.SubscriptionsView{
		MonthlyCost:   monthlyLabel,
		Subscriptions: rows,
		Members:       memberOptions,
		Cycles:        cycles,
		CSRFToken:     authadapter.GetCSRFToken(r.Context(), h.sm),
	}, nil
}

// parseSubscriptionInput parses the subscription form into a service input.
func parseSubscriptionInput(r *http.Request) (app.SubscriptionInput, error) {
	cents, err := parseAmountCents(r.FormValue("amount"))
	if err != nil {
		return app.SubscriptionInput{}, err
	}
	currency := strings.ToUpper(strings.TrimSpace(r.FormValue("currency")))
	if currency == "" {
		currency = "USD"
	}
	amount, err := household.NewMoney(cents, currency)
	if err != nil {
		return app.SubscriptionInput{}, err
	}
	cycle, err := domain.ParseCycle(strings.TrimSpace(r.FormValue("cycle")))
	if err != nil {
		return app.SubscriptionInput{}, err
	}
	next, err := time.Parse(dateLayout, strings.TrimSpace(r.FormValue("next_renewal_on")))
	if err != nil {
		return app.SubscriptionInput{}, fmt.Errorf("invalid next renewal date")
	}
	lead := 0
	if v := strings.TrimSpace(r.FormValue("reminder_lead_days")); v != "" {
		lead, err = strconv.Atoi(v)
		if err != nil {
			return app.SubscriptionInput{}, fmt.Errorf("invalid reminder lead days")
		}
	}
	var payer *household.MemberID
	if v := strings.TrimSpace(r.FormValue("payer_id")); v != "" {
		id, err := household.ParseMemberID(v)
		if err != nil {
			return app.SubscriptionInput{}, fmt.Errorf("invalid payer")
		}
		payer = &id
	}
	return app.SubscriptionInput{
		Name:             strings.TrimSpace(r.FormValue("name")),
		Amount:           amount,
		Cycle:            cycle,
		NextRenewalOn:    next,
		PayerID:          payer,
		Category:         strings.TrimSpace(r.FormValue("category")),
		ReminderLeadDays: lead,
	}, nil
}

// parseAmountCents parses a decimal money string (e.g. "9.99") into integer cents.
func parseAmountCents(s string) (int64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount")
	}
	// Reject NaN/±Inf, which ParseFloat accepts and which would slip past the
	// range checks below and corrupt the cents conversion.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid amount")
	}
	if f < 0 {
		return 0, fmt.Errorf("amount must not be negative")
	}
	// Guard the cents conversion against int64 overflow so a huge value fails
	// with a clear error rather than wrapping to a negative amount.
	if f > maxAmountUnits {
		return 0, fmt.Errorf("amount is too large")
	}
	return int64(math.Round(f * 100)), nil
}

func (h *WebHandlers) beginMutation(w http.ResponseWriter, r *http.Request) (*household.Member, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	if !authadapter.VerifyCSRF(r, h.sm) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return nil, false
	}
	member, ok := authadapter.CurrentMember(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return member, true
}

// respondAfterMutation refreshes the page: HX-Redirect for HTMX, a 303 otherwise.
func respondAfterMutation(w http.ResponseWriter, r *http.Request, target string) {
	if render.IsHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleMutationError maps domain errors to HTTP status codes.
func (h *WebHandlers) handleMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrSubscriptionNotFound),
		errors.Is(err, household.ErrHouseholdNotFound),
		errors.Is(err, household.ErrMemberNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, domain.ErrInvalidSubscription),
		errors.Is(err, household.ErrInvalidMoney):
		http.Error(w, "invalid subscription", http.StatusBadRequest)
	default:
		h.logger.ErrorContext(r.Context(), "subscriptions: mutation failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
