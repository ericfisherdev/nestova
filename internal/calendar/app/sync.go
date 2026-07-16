package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ericfisherdev/nestova/internal/calendar/domain"
	"github.com/ericfisherdev/nestova/internal/platform/metrics"
)

// syncAccountStore is the slice of the account repository the sync engine needs
// (ISP): iterating all accounts and persisting each one's sync cursor.
type syncAccountStore interface {
	ListAll(ctx context.Context) ([]*domain.CalendarAccount, error)
	SetSyncToken(ctx context.Context, id domain.CalendarAccountID, syncToken *string) error
}

// accessTokenProvider supplies a currently-valid access token for an account
// (refreshing transparently). *AccountService satisfies it.
type accessTokenProvider interface {
	ValidAccessToken(ctx context.Context, id domain.CalendarAccountID) (string, error)
}

// SyncService pulls each connected account's events into the external-event
// cache. It syncs the account's primary (first) calendar incrementally via the
// stored sync token; per-calendar sync tokens for additional calendars are
// future work, so only the first calendar id is synced today.
type SyncService struct {
	accounts syncAccountStore
	events   domain.ExternalEventRepository
	source   domain.CalendarEventSource
	tokens   accessTokenProvider
	logger   *slog.Logger
	recorder metrics.SyncRecorder
}

// NewSyncService constructs the service with injected dependencies. recorder
// receives the per-account synced-event counts and account-level sync failures
// (NES-115); pass [metrics.NopSyncRecorder] when sync instrumentation is
// irrelevant. Returns an error when any dependency is nil.
func NewSyncService(accounts syncAccountStore, events domain.ExternalEventRepository, source domain.CalendarEventSource, tokens accessTokenProvider, logger *slog.Logger, recorder metrics.SyncRecorder) (*SyncService, error) {
	if accounts == nil {
		return nil, errors.New("calendar: NewSyncService requires a non-nil account store")
	}
	if events == nil {
		return nil, errors.New("calendar: NewSyncService requires a non-nil event repository")
	}
	if source == nil {
		return nil, errors.New("calendar: NewSyncService requires a non-nil event source")
	}
	if tokens == nil {
		return nil, errors.New("calendar: NewSyncService requires a non-nil token provider")
	}
	if logger == nil {
		return nil, errors.New("calendar: NewSyncService requires a non-nil logger")
	}
	if recorder == nil {
		return nil, errors.New("calendar: NewSyncService requires a non-nil sync recorder")
	}
	return &SyncService{accounts: accounts, events: events, source: source, tokens: tokens, logger: logger, recorder: recorder}, nil
}

// RunOnce syncs every connected account once, returning the number of events
// processed. A failure on one account is logged and recorded, but the rest of
// the batch still runs; the first error encountered is returned.
func (s *SyncService) RunOnce(ctx context.Context) (int, error) {
	accounts, err := s.accounts.ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("sync: list accounts: %w", err)
	}

	var (
		processed int
		firstErr  error
	)
	for _, account := range accounts {
		if account == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		n, syncErr := s.syncAccount(ctx, account)
		if syncErr != nil {
			s.recorder.IncAccountError()
			s.logger.ErrorContext(ctx, "sync: account failed",
				"account_id", account.ID.String(), "error", syncErr)
			if firstErr == nil {
				firstErr = syncErr
			}
			continue
		}
		// Record per successful account (not once per pass) so events synced
		// ahead of a later account's failure or a shutdown are still counted.
		s.recorder.AddEventsSynced(n)
		processed += n
	}
	return processed, firstErr
}

// syncAccount syncs an account's primary calendar: it obtains a valid access
// token, lists events incrementally (falling back to a full resync on an invalid
// sync token), upserts or deletes each event in the cache, and persists the new
// sync token. It returns the number of events processed.
func (s *SyncService) syncAccount(ctx context.Context, account *domain.CalendarAccount) (int, error) {
	if len(account.CalendarIDs) == 0 {
		return 0, nil
	}
	calendarID := account.CalendarIDs[0]

	accessToken, err := s.tokens.ValidAccessToken(ctx, account.ID)
	if err != nil {
		return 0, fmt.Errorf("obtain access token: %w", err)
	}

	storedSyncToken := ""
	if account.SyncToken != nil {
		storedSyncToken = *account.SyncToken
	}

	events, nextSyncToken, err := s.source.ListEvents(ctx, accessToken, calendarID, storedSyncToken)
	if errors.Is(err, domain.ErrSyncTokenInvalid) {
		// The stored cursor is stale; discard it and perform a full resync.
		s.logger.InfoContext(ctx, "sync: sync token invalid, doing a full resync", "account_id", account.ID.String())
		events, nextSyncToken, err = s.source.ListEvents(ctx, accessToken, calendarID, "")
	}
	if err != nil {
		return 0, fmt.Errorf("list events for calendar %q: %w", calendarID, err)
	}

	processed := 0
	for _, ev := range events {
		// Stop promptly if the cycle's context is cancelled (e.g. shutdown) rather
		// than pushing every remaining event through a dead context.
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		if err := s.applyEvent(ctx, account.ID, ev); err != nil {
			return processed, err
		}
		processed++
	}

	if err := s.accounts.SetSyncToken(ctx, account.ID, syncTokenPtr(nextSyncToken)); err != nil {
		return processed, fmt.Errorf("persist sync token: %w", err)
	}
	return processed, nil
}

// applyEvent upserts an event into the cache, or removes it when the provider
// reports it cancelled. A malformed event is logged and skipped rather than
// failing the whole account sync.
func (s *SyncService) applyEvent(ctx context.Context, accountID domain.CalendarAccountID, ev domain.SyncedEvent) error {
	if ev.Cancelled {
		if err := s.events.DeleteByExternalID(ctx, accountID, ev.ExternalID); err != nil {
			return fmt.Errorf("delete cancelled event %q: %w", ev.ExternalID, err)
		}
		return nil
	}
	event := &domain.ExternalEvent{
		ID:                domain.NewExternalEventID(),
		CalendarAccountID: accountID,
		ExternalID:        ev.ExternalID,
		Title:             ev.Title,
		StartsAt:          ev.StartsAt,
		EndsAt:            ev.EndsAt,
		AllDay:            ev.AllDay,
		Color:             ev.Color,
	}
	if err := event.Validate(); err != nil {
		s.logger.WarnContext(ctx, "sync: skipping malformed event",
			"account_id", accountID.String(), "external_id", ev.ExternalID, "error", err)
		return nil
	}
	if err := s.events.UpsertByExternalID(ctx, event); err != nil {
		return fmt.Errorf("upsert event %q: %w", ev.ExternalID, err)
	}
	return nil
}

// syncTokenPtr returns a *string for a sync token, or nil for an empty token so
// the column is cleared.
func syncTokenPtr(token string) *string {
	if token == "" {
		return nil
	}
	return &token
}
