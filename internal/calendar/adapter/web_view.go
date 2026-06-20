package adapter

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	"github.com/ericfisherdev/nestova/internal/calendar/app"
	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/render"
	"github.com/ericfisherdev/nestova/web/components"
)

// LayoutFunc wraps page content in the app shell; home.go provides it.
type LayoutFunc func(member *household.Member) func(templ.Component) templ.Component

// displayDateLayout / displayDateTimeLayout format calendar item times. Timed
// events carry the timezone name (the app stores all times in UTC and has no
// per-member timezone yet) so the displayed clock time is unambiguous.
const (
	displayDateLayout     = "Jan 2, 2006"
	displayDateTimeLayout = "Jan 2, 3:04 PM MST"
)

// kindLabels maps a CalendarItemKind to its display label.
var kindLabels = map[app.CalendarItemKind]string{
	app.KindEvent:   "Event",
	app.KindTask:    "Chore",
	app.KindRenewal: "Renewal",
}

// ViewHandlers serves the unified calendar page (GET /calendar). The OAuth
// connect/callback flow lives in WebHandlers; this renders the merged timeline
// and the connected-accounts list.
type ViewHandlers struct {
	unified    *app.UnifiedCalendarService
	accounts   domain.CalendarAccountRepository
	households household.HouseholdRepository
	sm         *scs.SessionManager
	logger     *slog.Logger
}

// NewViewHandlers constructs a ViewHandlers, panicking on a nil dependency.
func NewViewHandlers(unified *app.UnifiedCalendarService, accounts domain.CalendarAccountRepository, households household.HouseholdRepository, sm *scs.SessionManager, logger *slog.Logger) *ViewHandlers {
	switch {
	case unified == nil:
		panic("calendar/adapter: NewViewHandlers requires a non-nil UnifiedCalendarService")
	case accounts == nil:
		panic("calendar/adapter: NewViewHandlers requires a non-nil CalendarAccountRepository")
	case households == nil:
		panic("calendar/adapter: NewViewHandlers requires a non-nil HouseholdRepository")
	case sm == nil:
		panic("calendar/adapter: NewViewHandlers requires a non-nil session manager")
	case logger == nil:
		panic("calendar/adapter: NewViewHandlers requires a non-nil logger")
	}
	return &ViewHandlers{unified: unified, accounts: accounts, households: households, sm: sm, logger: logger}
}

// Page handles GET /calendar: the current month's merged events, chores, and
// renewals, plus the connected-accounts list and the connect button.
func (h *ViewHandlers) Page(layoutFn LayoutFunc, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		member, ok := authadapter.CurrentMember(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		view, err := h.buildView(r, member, now())
		if err != nil {
			h.logger.ErrorContext(r.Context(), "calendar: build view", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := render.Page(r.Context(), w, r, layoutFn(member), components.CalendarPage(view)); err != nil {
			h.logger.ErrorContext(r.Context(), "calendar: render page", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

func (h *ViewHandlers) buildView(r *http.Request, member *household.Member, now time.Time) (components.CalendarView, error) {
	utc := now.UTC()
	from := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Inclusive end of the current month: the last instant before the next
	// month begins, since the unified service compares the range inclusively.
	to := from.AddDate(0, 1, 0).Add(-time.Nanosecond)

	items, err := h.unified.List(r.Context(), member.HouseholdID, from, to)
	if err != nil {
		return components.CalendarView{}, err
	}
	itemViews := make([]components.CalendarItemView, 0, len(items))
	for _, it := range items {
		when := it.Start.Format(displayDateLayout)
		if !it.AllDay {
			// Format in UTC so the "MST" verb renders "UTC" (times are stored in UTC).
			when = it.Start.UTC().Format(displayDateTimeLayout)
		}
		itemViews = append(itemViews, components.CalendarItemView{
			Kind:      string(it.Kind),
			KindLabel: kindLabels[it.Kind],
			Title:     it.Title,
			When:      when,
			Color:     it.MemberColor,
		})
	}

	accounts, err := h.accounts.ListByHousehold(r.Context(), member.HouseholdID)
	if err != nil {
		return components.CalendarView{}, err
	}
	members, err := h.households.ListMembers(r.Context(), member.HouseholdID)
	if err != nil {
		return components.CalendarView{}, err
	}
	nameByID := make(map[household.MemberID]string, len(members))
	for _, m := range members {
		nameByID[m.ID] = m.DisplayName
	}
	accountViews := make([]components.ConnectedAccountView, 0, len(accounts))
	for _, a := range accounts {
		// The member is always present: calendar_account's tenant FK cascades on
		// member deletion, so an account cannot outlive its member.
		accountViews = append(accountViews, components.ConnectedAccountView{
			Provider:   a.Provider.String(),
			MemberName: nameByID[a.MemberID],
		})
	}

	return components.CalendarView{
		RangeLabel: from.Format("January 2006"),
		Items:      itemViews,
		Accounts:   accountViews,
		CSRFToken:  authadapter.GetCSRFToken(r.Context(), h.sm),
	}, nil
}
