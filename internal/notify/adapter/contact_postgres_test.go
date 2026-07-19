package adapter_test

import (
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

func testE164PhoneForContact(t *testing.T) domain.E164Phone {
	t.Helper()
	p, err := domain.ParseE164Phone("+15551234567")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}
	return p
}

func TestPostgresContactDirectory_GetContact_UnknownMember_ReturnsNotFound(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)

	if _, err := dir.GetContact(testCtx(t), household.NewMemberID()); !errors.Is(err, domain.ErrMemberContactNotFound) {
		t.Errorf("GetContact(unknown member) error = %v, want ErrMemberContactNotFound", err)
	}
}

func TestPostgresContactDirectory_FreshMember_HasNoContact(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	got, err := dir.GetContact(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got.Phone != nil || got.SMSOptedIn {
		t.Errorf("fresh member contact = (%v, %v), want (nil, false)", got.Phone, got.SMSOptedIn)
	}
}

func TestPostgresContactDirectory_SetPhone_ThenGetContact_RoundTrips(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	phone := testE164PhoneForContact(t)
	if err := dir.SetPhone(testCtx(t), memberID, &phone); err != nil {
		t.Fatalf("SetPhone: %v", err)
	}
	got, err := dir.GetContact(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got.Phone == nil || got.Phone.String() != phone.String() {
		t.Errorf("GetContact().Phone = %v, want %v", got.Phone, phone)
	}
	if got.SMSOptedIn {
		t.Error("setting a phone must not itself opt the member in")
	}
}

func TestPostgresContactDirectory_SetPhone_Nil_Clears(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	phone := testE164PhoneForContact(t)
	if err := dir.SetPhone(testCtx(t), memberID, &phone); err != nil {
		t.Fatalf("SetPhone: %v", err)
	}
	if err := dir.SetPhone(testCtx(t), memberID, nil); err != nil {
		t.Fatalf("SetPhone(nil): %v", err)
	}
	got, err := dir.GetContact(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got.Phone != nil {
		t.Errorf("GetContact().Phone = %v, want nil after clearing", got.Phone)
	}
}

func TestPostgresContactDirectory_SetPhone_SameNumber_PreservesOptIn(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	phone := testE164PhoneForContact(t)
	if err := dir.SetPhone(testCtx(t), memberID, &phone); err != nil {
		t.Fatalf("SetPhone: %v", err)
	}
	if err := dir.SetOptedIn(testCtx(t), memberID, true); err != nil {
		t.Fatalf("SetOptedIn: %v", err)
	}

	// Resubmitting the SAME number must not reset consent.
	if err := dir.SetPhone(testCtx(t), memberID, &phone); err != nil {
		t.Fatalf("SetPhone (resubmit same number): %v", err)
	}
	got, err := dir.GetContact(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if !got.SMSOptedIn {
		t.Error("resubmitting the same phone number must preserve existing opt-in consent")
	}
}

func TestPostgresContactDirectory_SetPhone_DifferentNumber_ResetsOptIn(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	phone := testE164PhoneForContact(t)
	if err := dir.SetPhone(testCtx(t), memberID, &phone); err != nil {
		t.Fatalf("SetPhone: %v", err)
	}
	if err := dir.SetOptedIn(testCtx(t), memberID, true); err != nil {
		t.Fatalf("SetOptedIn: %v", err)
	}

	other, err := domain.ParseE164Phone("+447911123456")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}
	if err := dir.SetPhone(testCtx(t), memberID, &other); err != nil {
		t.Fatalf("SetPhone (different number): %v", err)
	}
	got, err := dir.GetContact(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got.SMSOptedIn {
		t.Error("changing to a DIFFERENT phone number must reset opt-in consent — fresh consent is required for a new number")
	}
}

func TestPostgresContactDirectory_SetOptedIn_RequiresPhone(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	err := dir.SetOptedIn(testCtx(t), memberID, true)
	if !errors.Is(err, domain.ErrPhoneRequiredForOptIn) {
		t.Errorf("SetOptedIn(true, no phone) error = %v, want ErrPhoneRequiredForOptIn", err)
	}
}

func TestPostgresContactDirectory_SetOptedIn_TrueThenFalse(t *testing.T) {
	pool := newTestPool(t)
	dir := notifyadapter.NewPostgresContactDirectory(pool)
	_, memberID := seedHouseholdAndMember(t, pool)

	phone := testE164PhoneForContact(t)
	if err := dir.SetPhone(testCtx(t), memberID, &phone); err != nil {
		t.Fatalf("SetPhone: %v", err)
	}
	if err := dir.SetOptedIn(testCtx(t), memberID, true); err != nil {
		t.Fatalf("SetOptedIn(true): %v", err)
	}
	got, err := dir.GetContact(testCtx(t), memberID)
	if err != nil || !got.SMSOptedIn {
		t.Fatalf("GetContact after opt-in = (%v, %v), want SMSOptedIn=true", got, err)
	}

	if err := dir.SetOptedIn(testCtx(t), memberID, false); err != nil {
		t.Fatalf("SetOptedIn(false): %v", err)
	}
	got, err = dir.GetContact(testCtx(t), memberID)
	if err != nil {
		t.Fatalf("GetContact after opt-out: %v", err)
	}
	if got.SMSOptedIn {
		t.Error("SetOptedIn(false) must clear opt-in state")
	}
	if got.Phone == nil {
		t.Error("opting out must not clear the phone number itself")
	}
}
