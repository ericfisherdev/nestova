package adapter_test

import (
	"errors"
	"testing"

	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

func TestPostgresPreferenceRepository_Get_NoRow_ReturnsNotFound(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewPostgresPreferenceRepository(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	_, err := repo.Get(testCtx(t), memberID, domain.EventTypeClaimExpiring)
	if !errors.Is(err, domain.ErrPreferenceNotFound) {
		t.Errorf("Get(no row) error = %v, want ErrPreferenceNotFound", err)
	}
}

func TestPostgresPreferenceRepository_Set_ThenGet_RoundTrips(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewPostgresPreferenceRepository(pool)
	hhID, memberID := seedHouseholdAndMember(t, pool)

	pref := domain.MemberPreference{
		HouseholdID: hhID,
		MemberID:    memberID,
		EventType:   domain.EventTypeClaimExpiring,
		Channel:     domain.ChannelSMS,
	}
	if err := repo.Set(testCtx(t), pref); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := repo.Get(testCtx(t), memberID, domain.EventTypeClaimExpiring)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != domain.ChannelSMS {
		t.Errorf("Get() = %v, want ChannelSMS", got)
	}
}

func TestPostgresPreferenceRepository_Set_Upsert_ReplacesChannel(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewPostgresPreferenceRepository(pool)
	hhID, memberID := seedHouseholdAndMember(t, pool)

	base := domain.MemberPreference{HouseholdID: hhID, MemberID: memberID, EventType: domain.EventTypeClaimExpiring}
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: base.HouseholdID, MemberID: base.MemberID, EventType: base.EventType, Channel: domain.ChannelSMS}); err != nil {
		t.Fatalf("Set (sms): %v", err)
	}
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: base.HouseholdID, MemberID: base.MemberID, EventType: base.EventType, Channel: domain.ChannelInApp}); err != nil {
		t.Fatalf("Set (inapp, same key): %v", err)
	}
	got, err := repo.Get(testCtx(t), memberID, domain.EventTypeClaimExpiring)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != domain.ChannelInApp {
		t.Errorf("Get() after upsert = %v, want ChannelInApp (the second Set must replace, not duplicate)", got)
	}
}

func TestPostgresPreferenceRepository_ListForMember(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewPostgresPreferenceRepository(pool)
	hhID, memberID := seedHouseholdAndMember(t, pool)

	// A member with no preferences at all gets an empty slice, not an error.
	prefs, err := repo.ListForMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListForMember (empty): %v", err)
	}
	if len(prefs) != 0 {
		t.Errorf("ListForMember (empty) = %v, want empty", prefs)
	}

	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: hhID, MemberID: memberID, EventType: domain.EventTypeClaimExpiring, Channel: domain.ChannelSMS}); err != nil {
		t.Fatalf("Set (claim expiring): %v", err)
	}
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: hhID, MemberID: memberID, EventType: domain.EventTypeTaskOverdue, Channel: domain.ChannelInApp}); err != nil {
		t.Fatalf("Set (task overdue): %v", err)
	}

	prefs, err = repo.ListForMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListForMember: %v", err)
	}
	if len(prefs) != 2 {
		t.Fatalf("ListForMember len = %d, want 2", len(prefs))
	}
	byEventType := make(map[domain.EventType]domain.Channel, len(prefs))
	for _, p := range prefs {
		byEventType[p.EventType] = p.Channel
	}
	if byEventType[domain.EventTypeClaimExpiring] != domain.ChannelSMS {
		t.Errorf("claim_expiring channel = %v, want sms", byEventType[domain.EventTypeClaimExpiring])
	}
	if byEventType[domain.EventTypeTaskOverdue] != domain.ChannelInApp {
		t.Errorf("task_overdue channel = %v, want inapp", byEventType[domain.EventTypeTaskOverdue])
	}
}

// TestPostgresPreferenceRepository_DowngradeChannel_ReplacesOnlyMatchingRows
// is the NES-141 regression test for bounce handling: DowngradeChannel
// must replace every row currently set to `from` with `to`, scoped to the
// given member, and must leave rows already on a DIFFERENT channel (and
// rows belonging to a DIFFERENT member) untouched.
func TestPostgresPreferenceRepository_DowngradeChannel_ReplacesOnlyMatchingRows(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewPostgresPreferenceRepository(pool)
	hhID, memberID := seedHouseholdAndMember(t, pool)
	otherHHID, otherMemberID := seedHouseholdAndMember(t, pool)

	// memberID: two email preferences, one already inapp.
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: hhID, MemberID: memberID, EventType: domain.EventTypeClaimExpiring, Channel: domain.ChannelEmail}); err != nil {
		t.Fatalf("Set (claim expiring, email): %v", err)
	}
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: hhID, MemberID: memberID, EventType: domain.EventTypeTaskOverdue, Channel: domain.ChannelEmail}); err != nil {
		t.Fatalf("Set (task overdue, email): %v", err)
	}
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: hhID, MemberID: memberID, EventType: domain.EventTypeChoreTradeProposed, Channel: domain.ChannelInApp}); err != nil {
		t.Fatalf("Set (chore trade, inapp): %v", err)
	}
	// A different member in a different household with their OWN email
	// preference must be untouched.
	if err := repo.Set(testCtx(t), domain.MemberPreference{HouseholdID: otherHHID, MemberID: otherMemberID, EventType: domain.EventTypeClaimExpiring, Channel: domain.ChannelEmail}); err != nil {
		t.Fatalf("Set (other member, email): %v", err)
	}

	if err := repo.DowngradeChannel(testCtx(t), memberID, domain.ChannelEmail, domain.ChannelInApp); err != nil {
		t.Fatalf("DowngradeChannel: %v", err)
	}

	prefs, err := repo.ListForMember(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("ListForMember: %v", err)
	}
	for _, p := range prefs {
		if p.Channel != domain.ChannelInApp {
			t.Errorf("preference for %s = %v, want inapp (every preference must have been downgraded or already been inapp)", p.EventType, p.Channel)
		}
	}

	otherChannel, err := repo.Get(testCtx(t), otherMemberID, domain.EventTypeClaimExpiring)
	if err != nil {
		t.Fatalf("Get (other member): %v", err)
	}
	if otherChannel != domain.ChannelEmail {
		t.Errorf("other member's preference = %v, want email (DowngradeChannel must be scoped to the given member only)", otherChannel)
	}
}

// TestPostgresPreferenceRepository_DowngradeChannel_NoMatchingRows_IsNotAnError
// confirms a member with no from-channel preference rows at all is a
// normal outcome, not an error — see the port's own doc.
func TestPostgresPreferenceRepository_DowngradeChannel_NoMatchingRows_IsNotAnError(t *testing.T) {
	pool := newTestPool(t)
	repo := notifyadapter.NewPostgresPreferenceRepository(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	if err := repo.DowngradeChannel(testCtx(t), memberID, domain.ChannelEmail, domain.ChannelInApp); err != nil {
		t.Errorf("DowngradeChannel (no matching rows): %v, want nil", err)
	}
}
