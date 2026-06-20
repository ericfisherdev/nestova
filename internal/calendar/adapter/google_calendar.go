package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/ericfisherdev/nestova/internal/calendar/domain"
)

// fullSyncLookback is how far back a full sync fetches events. Incremental syncs
// then track changes from the captured sync token, so this only bounds the
// initial backfill window.
const fullSyncLookback = 31 * 24 * time.Hour

// GoogleCalendarClient implements domain.CalendarEventSource against the Google
// Calendar API. It is constructed per call from a member's access token.
type GoogleCalendarClient struct {
	// now returns the reference time for the full-sync window; injected for tests.
	now func() time.Time
}

// Compile-time assurance the client satisfies the port.
var _ domain.CalendarEventSource = (*GoogleCalendarClient)(nil)

// NewGoogleCalendarClient constructs the client.
func NewGoogleCalendarClient() *GoogleCalendarClient {
	return &GoogleCalendarClient{now: time.Now}
}

// ListEvents fetches the calendar's events. With an empty syncToken it does a
// full sync (events from fullSyncLookback ago, with deletions enabled so later
// incremental syncs report them); otherwise it does an incremental sync from the
// token. A 410 from Google means the token is stale: it returns
// domain.ErrSyncTokenInvalid so the caller retries with a full sync.
func (c *GoogleCalendarClient) ListEvents(ctx context.Context, accessToken, calendarID, syncToken string) ([]domain.SyncedEvent, string, error) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken})
	svc, err := calendar.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		return nil, "", fmt.Errorf("create calendar service: %w", err)
	}

	base := svc.Events.List(calendarID)
	if syncToken != "" {
		// Incremental: the sync token encodes the original request's settings, so
		// no other filters are passed.
		base = base.SyncToken(syncToken)
	} else {
		base = base.ShowDeleted(true).SingleEvents(true).
			TimeMin(c.now().Add(-fullSyncLookback).Format(time.RFC3339))
	}

	var (
		out       []domain.SyncedEvent
		nextSync  string
		pageToken string
	)
	for {
		call := base
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Context(ctx).Do()
		if err != nil {
			var gerr *googleapi.Error
			if errors.As(err, &gerr) && gerr.Code == http.StatusGone {
				return nil, "", domain.ErrSyncTokenInvalid
			}
			return nil, "", fmt.Errorf("list events: %w", err)
		}
		for _, item := range resp.Items {
			if ev, ok := mapEvent(item); ok {
				out = append(out, ev)
			}
		}
		if resp.NextPageToken != "" {
			pageToken = resp.NextPageToken
			continue
		}
		nextSync = resp.NextSyncToken
		break
	}
	return out, nextSync, nil
}

// mapEvent converts a Google event to a domain.SyncedEvent. A cancelled event
// maps to a deletion. An event with neither a dateTime nor a date start is
// skipped (ok=false) since it cannot be cached.
func mapEvent(item *calendar.Event) (domain.SyncedEvent, bool) {
	if item == nil || item.Id == "" {
		return domain.SyncedEvent{}, false
	}
	if item.Status == "cancelled" {
		return domain.SyncedEvent{ExternalID: item.Id, Cancelled: true}, true
	}
	if item.Start == nil || item.End == nil {
		return domain.SyncedEvent{}, false
	}

	ev := domain.SyncedEvent{ExternalID: item.Id, Title: item.Summary, Color: item.ColorId}
	switch {
	case item.Start.DateTime != "":
		start, err := time.Parse(time.RFC3339, item.Start.DateTime)
		if err != nil {
			return domain.SyncedEvent{}, false
		}
		end, err := time.Parse(time.RFC3339, item.End.DateTime)
		if err != nil {
			return domain.SyncedEvent{}, false
		}
		ev.StartsAt, ev.EndsAt, ev.AllDay = start, end, false
	case item.Start.Date != "":
		// All-day dates are parsed at UTC midnight to match the app-wide date-only
		// convention (task due dates and subscription renewals are likewise stored
		// as UTC midnight), keeping the unified calendar's day boundaries aligned.
		start, err := time.ParseInLocation(dateOnlyLayout, item.Start.Date, time.UTC)
		if err != nil {
			return domain.SyncedEvent{}, false
		}
		end, err := time.ParseInLocation(dateOnlyLayout, item.End.Date, time.UTC)
		if err != nil {
			return domain.SyncedEvent{}, false
		}
		ev.StartsAt, ev.EndsAt, ev.AllDay = start, end, true
	default:
		return domain.SyncedEvent{}, false
	}
	return ev, true
}

// dateOnlyLayout is Google's all-day event date format (YYYY-MM-DD).
const dateOnlyLayout = "2006-01-02"
