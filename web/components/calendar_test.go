package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func calendarView() components.CalendarView {
	return components.CalendarView{
		RangeLabel: "July 2026",
		Items: []components.CalendarItemView{
			{Kind: "task", KindLabel: "Chore", Title: "Vacuum", When: "Jul 5, 2026", Color: "clay"},
			{Kind: "event", KindLabel: "Event", Title: "Dentist", When: "Jul 10, 9:00 AM", Color: ""},
			{Kind: "renewal", KindLabel: "Renewal", Title: "Streaming", When: "Jul 20, 2026", Color: "plum"},
		},
		Accounts:  []components.ConnectedAccountView{{Provider: "google", MemberName: "Alex"}},
		CSRFToken: "tok-cal",
	}
}

func TestCalendarPageRendersItemsAndConnect(t *testing.T) {
	out := renderString(t, components.CalendarPage(calendarView()))

	if !strings.Contains(out, "July 2026") {
		t.Errorf("missing range label: %q", out)
	}
	// Connect button posts to the OAuth connect route with the CSRF token.
	if !strings.Contains(out, `hx-post="/calendar/google/connect"`) {
		t.Errorf("missing connect hx-post: %q", out)
	}
	if !strings.Contains(out, `value="tok-cal"`) {
		t.Errorf("missing csrf token field: %q", out)
	}
	// All three kinds appear with their titles.
	for _, want := range []string{"Vacuum", "Dentist", "Streaming", "Chore", "Event", "Renewal"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in calendar output", want)
		}
	}
	// Member-attributed items carry their color tint.
	if !strings.Contains(out, "bg-member-clay-tint") || !strings.Contains(out, "bg-member-plum-tint") {
		t.Errorf("missing member color tints: %q", out)
	}
	// Connected account is listed.
	if !strings.Contains(out, "google") || !strings.Contains(out, "Alex") {
		t.Errorf("missing connected account: %q", out)
	}
}

func TestCalendarPageEmptyStates(t *testing.T) {
	view := calendarView()
	view.Items = nil
	view.Accounts = nil
	out := renderString(t, components.CalendarPage(view))
	if !strings.Contains(out, "Nothing scheduled") {
		t.Errorf("missing empty items state: %q", out)
	}
	if !strings.Contains(out, "No calendars connected") {
		t.Errorf("missing empty accounts state: %q", out)
	}
}
