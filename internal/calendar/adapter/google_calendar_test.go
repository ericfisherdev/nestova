package adapter

import (
	"testing"
	"time"

	"google.golang.org/api/calendar/v3"
)

func TestMapEventTimed(t *testing.T) {
	item := &calendar.Event{
		Id:      "evt-1",
		Summary: "Standup",
		ColorId: "5",
		Start:   &calendar.EventDateTime{DateTime: "2026-07-01T09:00:00Z"},
		End:     &calendar.EventDateTime{DateTime: "2026-07-01T09:30:00Z"},
	}
	ev, ok := mapEvent(item)
	if !ok {
		t.Fatal("mapEvent ok = false, want true")
	}
	if ev.ExternalID != "evt-1" || ev.Title != "Standup" || ev.Color != "5" || ev.AllDay || ev.Cancelled {
		t.Fatalf("mapEvent = %+v", ev)
	}
	if !ev.StartsAt.Equal(time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)) || !ev.EndsAt.Equal(time.Date(2026, 7, 1, 9, 30, 0, 0, time.UTC)) {
		t.Fatalf("mapEvent times = %s..%s", ev.StartsAt, ev.EndsAt)
	}
}

func TestMapEventAllDay(t *testing.T) {
	item := &calendar.Event{
		Id:    "evt-2",
		Start: &calendar.EventDateTime{Date: "2026-07-04"},
		End:   &calendar.EventDateTime{Date: "2026-07-05"}, // Google end date is exclusive
	}
	ev, ok := mapEvent(item)
	if !ok || !ev.AllDay {
		t.Fatalf("mapEvent all-day ok=%v allDay=%v", ok, ev.AllDay)
	}
	if !ev.StartsAt.Equal(time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("all-day start = %s, want 2026-07-04", ev.StartsAt)
	}
}

func TestMapEventCancelled(t *testing.T) {
	ev, ok := mapEvent(&calendar.Event{Id: "evt-3", Status: "cancelled"})
	if !ok || !ev.Cancelled || ev.ExternalID != "evt-3" {
		t.Fatalf("mapEvent cancelled = %+v ok=%v", ev, ok)
	}
}

func TestMapEventSkipsUnusable(t *testing.T) {
	cases := []struct {
		name string
		item *calendar.Event
	}{
		{"nil", nil},
		{"empty id", &calendar.Event{Id: ""}},
		{"no start/end", &calendar.Event{Id: "x"}},
		{"start without time or date", &calendar.Event{Id: "x", Start: &calendar.EventDateTime{}, End: &calendar.EventDateTime{}}},
		{"unparseable datetime", &calendar.Event{Id: "x", Start: &calendar.EventDateTime{DateTime: "nope"}, End: &calendar.EventDateTime{DateTime: "nope"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := mapEvent(tc.item); ok {
				t.Fatalf("mapEvent(%s) ok = true, want false", tc.name)
			}
		})
	}
}
