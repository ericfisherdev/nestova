package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/crypto/cryptotest"

	"github.com/alexedwards/scs/v2"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	notifyapp "github.com/ericfisherdev/nestova/internal/notify/app"
	notifydomain "github.com/ericfisherdev/nestova/internal/notify/domain"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// ---------------------------------------------------------------------------
// Stateful notify fakes (NES-139) — deliberately separate from home_test.go's
// inert fakeContactDirectory/fakePreferenceRepository, which many OTHER
// tests share and rely on staying inert. These track real per-member state
// so this file can exercise an actual set-then-read round trip over HTTP.
// ---------------------------------------------------------------------------

type statefulContactDirectory struct {
	mu       sync.Mutex
	contacts map[household.MemberID]*notifydomain.MemberContact
}

func newStatefulContactDirectory() *statefulContactDirectory {
	return &statefulContactDirectory{contacts: make(map[household.MemberID]*notifydomain.MemberContact)}
}

func (d *statefulContactDirectory) GetContact(_ context.Context, memberID household.MemberID) (*notifydomain.MemberContact, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.contacts[memberID]; ok {
		cp := *c
		return &cp, nil
	}
	return &notifydomain.MemberContact{MemberID: memberID}, nil
}

func (d *statefulContactDirectory) SetPhone(_ context.Context, memberID household.MemberID, phone *notifydomain.E164Phone) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	existing, ok := d.contacts[memberID]
	sameNumber := ok && existing.Phone != nil && phone != nil && existing.Phone.String() == phone.String()
	optedIn := ok && existing.SMSOptedIn && sameNumber
	d.contacts[memberID] = &notifydomain.MemberContact{MemberID: memberID, Phone: phone, SMSOptedIn: optedIn}
	return nil
}

func (d *statefulContactDirectory) SetOptedIn(_ context.Context, memberID household.MemberID, optIn bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.contacts[memberID]
	if !ok {
		c = &notifydomain.MemberContact{MemberID: memberID}
		d.contacts[memberID] = c
	}
	if optIn && c.Phone == nil {
		return notifydomain.ErrPhoneRequiredForOptIn
	}
	c.SMSOptedIn = optIn
	return nil
}

type statefulPreferenceRepo struct {
	mu    sync.Mutex
	prefs map[string]notifydomain.MemberPreference
}

func newStatefulPreferenceRepo() *statefulPreferenceRepo {
	return &statefulPreferenceRepo{prefs: make(map[string]notifydomain.MemberPreference)}
}

func notifyPrefKey(memberID household.MemberID, eventType notifydomain.EventType) string {
	return memberID.String() + "|" + eventType.String()
}

func (r *statefulPreferenceRepo) Get(_ context.Context, memberID household.MemberID, eventType notifydomain.EventType) (notifydomain.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.prefs[notifyPrefKey(memberID, eventType)]
	if !ok {
		return "", notifydomain.ErrPreferenceNotFound
	}
	return p.Channel, nil
}

func (r *statefulPreferenceRepo) Set(_ context.Context, pref notifydomain.MemberPreference) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefs[notifyPrefKey(pref.MemberID, pref.EventType)] = pref
	return nil
}

func (r *statefulPreferenceRepo) ListForMember(_ context.Context, memberID household.MemberID) ([]notifydomain.MemberPreference, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notifydomain.MemberPreference
	for _, p := range r.prefs {
		if p.MemberID == memberID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (r *statefulPreferenceRepo) DowngradeChannel(_ context.Context, memberID household.MemberID, from, to notifydomain.Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, p := range r.prefs {
		if p.MemberID == memberID && p.Channel == from {
			p.Channel = to
			r.prefs[key] = p
		}
	}
	return nil
}

// buildNotifySettingsTestHandler mirrors buildSettingsTestHandler's
// construction, but wires STATEFUL notify fakes so this file's tests can
// exercise real set-then-read behavior over HTTP, returning them alongside
// the handler for direct assertions.
func buildNotifySettingsTestHandler(t *testing.T, hhRepo *multiMemberHouseholdRepo) (http.Handler, *scs.SessionManager, *statefulContactDirectory, *statefulPreferenceRepo) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()
	credRepo := newFakeMemberCredRepo()

	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	kioskSvc, err := kioskapp.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	settingsHandlers := kioskadapter.NewSettingsWebHandlers(kioskSvc, sm, logger)

	cipher, err := crypto.NewCipher([]byte("notify-settings-test-harness-mfa"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	mfaService, err := authapp.NewMFAService(newFakeMFARepo(), cipher, totp.NewProvider(), credRepo, hhRepo, cryptotest.Hasher(), logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	mfaHandlers := authadapter.NewMFAWebHandlers(mfaService, hhRepo, sm, logger)

	contacts := newStatefulContactDirectory()
	prefs := newStatefulPreferenceRepo()
	settingsService := notifyapp.NewSettingsService(contacts, prefs, hhRepo)
	notifyHandlers := notifyadapter.NewNotifyWebHandlers(settingsService, sm, logger)

	authHandlers := authadapter.NewHandlers(sm, authapp.New(credRepo, cryptotest.Hasher()), nil, nil, nil, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	registerSettingsPage(mux, logger, sm, hhRepo, settingsHandlers, mfaHandlers, mfaService, nil, nil, notifyHandlers)

	handler := sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm, contacts, prefs
}

// ---------------------------------------------------------------------------
// Phone entry
// ---------------------------------------------------------------------------

func TestNotifySettings_UpdatePhone_ValidNumber(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, contacts, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/phone", cookie, "csrf_token="+csrfToken+"&phone=%2B15551234567")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/phone (valid): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	contact, err := contacts.GetContact(context.Background(), member.ID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if contact.Phone == nil || contact.Phone.String() != "+15551234567" {
		t.Errorf("stored phone = %v, want +15551234567", contact.Phone)
	}
}

func TestNotifySettings_UpdatePhone_InvalidFormat_ShowsInlineError(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/phone", cookie, "csrf_token="+csrfToken+"&phone=not-a-phone")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/notify/phone (invalid): status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "valid phone number") {
		t.Errorf("response missing inline validation message: %s", rec.Body.String())
	}
}

func TestNotifySettings_UpdatePhone_MissingCSRF_Forbidden(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, _ := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/phone", cookie, "phone=%2B15551234567")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/notify/phone (no csrf): status = %d, want 403", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Opt-in
// ---------------------------------------------------------------------------

func TestNotifySettings_OptIn_WithoutPhone_ShowsInlineError(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/opt-in", cookie, "csrf_token="+csrfToken+"&opted_in=on")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/notify/opt-in (no phone): status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Add a phone number") {
		t.Errorf("response missing inline validation message: %s", rec.Body.String())
	}
}

func TestNotifySettings_OptIn_WithPhone_Succeeds(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, contacts, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	doForm(t, handler, http.MethodPost, "/settings/notify/phone", cookie, "csrf_token="+csrfToken+"&phone=%2B15551234567")
	rec := doForm(t, handler, http.MethodPost, "/settings/notify/opt-in", cookie, "csrf_token="+csrfToken+"&opted_in=on")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/opt-in (with phone): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	contact, err := contacts.GetContact(context.Background(), member.ID)
	if err != nil || !contact.SMSOptedIn {
		t.Fatalf("GetContact after opt-in = (%v, %v), want SMSOptedIn=true", contact, err)
	}
}

func TestNotifySettings_OptIn_CheckboxAbsent_OptsOut(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, contacts, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	doForm(t, handler, http.MethodPost, "/settings/notify/phone", cookie, "csrf_token="+csrfToken+"&phone=%2B15551234567")
	doForm(t, handler, http.MethodPost, "/settings/notify/opt-in", cookie, "csrf_token="+csrfToken+"&opted_in=on")
	// A second submit with the checkbox NOT included (unchecked) must opt out.
	rec := doForm(t, handler, http.MethodPost, "/settings/notify/opt-in", cookie, "csrf_token="+csrfToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/opt-in (unchecked): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	contact, err := contacts.GetContact(context.Background(), member.ID)
	if err != nil || contact.SMSOptedIn {
		t.Fatalf("GetContact after opt-out = (%v, %v), want SMSOptedIn=false", contact, err)
	}
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

func TestNotifySettings_SetPreference_SMSWithoutOptIn_ShowsInlineError(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/preferences", cookie,
		"csrf_token="+csrfToken+"&pref_"+notifydomain.EventTypeClaimExpiring.String()+"=sms")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/notify/preferences (sms, no opt-in): status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Opt in to text messages") {
		t.Errorf("response missing inline validation message: %s", rec.Body.String())
	}
}

func TestNotifySettings_SetPreference_SMSWithOptIn_Succeeds(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, prefs := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	doForm(t, handler, http.MethodPost, "/settings/notify/phone", cookie, "csrf_token="+csrfToken+"&phone=%2B15551234567")
	doForm(t, handler, http.MethodPost, "/settings/notify/opt-in", cookie, "csrf_token="+csrfToken+"&opted_in=on")

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/preferences", cookie,
		"csrf_token="+csrfToken+"&pref_"+notifydomain.EventTypeClaimExpiring.String()+"=sms")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/preferences (sms, opted in): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got, err := prefs.Get(context.Background(), member.ID, notifydomain.EventTypeClaimExpiring)
	if err != nil || got != notifydomain.ChannelSMS {
		t.Errorf("stored preference = (%v, %v), want (sms, nil)", got, err)
	}
}

// TestNotifySettings_SetPreference_UndeliverableChannel_Rejected is the
// end-to-end regression test for CodeRabbit round 3 (major finding #2): a
// crafted POST choosing channel=push — never offered by the settings
// UI's own <select> (in-app, SMS, and — since NES-141 — email) — must be
// rejected server-side, and persist nothing.
func TestNotifySettings_SetPreference_UndeliverableChannel_Rejected(t *testing.T) {
	for _, channel := range []string{"push"} {
		t.Run(channel, func(t *testing.T) {
			member := settingsTestAdultInHousehold(household.NewHouseholdID())
			hhRepo := newMultiMemberHouseholdRepo(member)
			handler, sm, _, prefs := buildNotifySettingsTestHandler(t, hhRepo)
			cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

			rec := doForm(t, handler, http.MethodPost, "/settings/notify/preferences", cookie,
				"csrf_token="+csrfToken+"&pref_"+notifydomain.EventTypeClaimExpiring.String()+"="+channel)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /settings/notify/preferences (%s): status = %d, want 400; body: %s", channel, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "available yet") {
				t.Errorf("response missing inline validation message: %s", rec.Body.String())
			}
			if _, err := prefs.Get(context.Background(), member.ID, notifydomain.EventTypeClaimExpiring); !errors.Is(err, notifydomain.ErrPreferenceNotFound) {
				t.Errorf("a rejected %s preference must not be persisted", channel)
			}
		})
	}
}

func TestNotifySettings_SetPreference_InApp_Succeeds(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, prefs := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/preferences", cookie,
		"csrf_token="+csrfToken+"&pref_"+notifydomain.EventTypeTaskOverdue.String()+"=inapp")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/preferences (inapp): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got, err := prefs.Get(context.Background(), member.ID, notifydomain.EventTypeTaskOverdue)
	if err != nil || got != notifydomain.ChannelInApp {
		t.Errorf("stored preference = (%v, %v), want (inapp, nil)", got, err)
	}
}

// TestNotifySettings_SetPreference_Email_Succeeds is the NES-141
// end-to-end regression test: unlike sms (which requires opt-in
// consent), submitting channel=email needs no readiness precondition and
// must succeed and persist directly.
func TestNotifySettings_SetPreference_Email_Succeeds(t *testing.T) {
	member := settingsTestAdultInHousehold(household.NewHouseholdID())
	hhRepo := newMultiMemberHouseholdRepo(member)
	handler, sm, _, prefs := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, member.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/preferences", cookie,
		"csrf_token="+csrfToken+"&pref_"+notifydomain.EventTypeTaskOverdue.String()+"=email")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/preferences (email): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got, err := prefs.Get(context.Background(), member.ID, notifydomain.EventTypeTaskOverdue)
	if err != nil || got != notifydomain.ChannelEmail {
		t.Errorf("stored preference = (%v, %v), want (email, nil)", got, err)
	}
}

// ---------------------------------------------------------------------------
// Quiet hours (owner-only)
// ---------------------------------------------------------------------------

func TestNotifySettings_QuietHours_ForbiddenForNonOwner(t *testing.T) {
	owner := settingsTestOwner()
	adult := settingsTestAdultInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, adult)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/quiet-hours", cookie,
		"csrf_token="+csrfToken+"&quiet_enabled=on&quiet_start=22:00&quiet_end=07:00")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/notify/quiet-hours as a non-owner adult: status = %d, want 403", rec.Code)
	}
}

func TestNotifySettings_QuietHours_OwnerSetsValidWindow(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/quiet-hours", cookie,
		"csrf_token="+csrfToken+"&quiet_enabled=on&quiet_start=22:00&quiet_end=07:00")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/quiet-hours (valid window): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got, err := hhRepo.GetHousehold(context.Background(), owner.HouseholdID)
	if err != nil {
		t.Fatalf("GetHousehold: %v", err)
	}
	if got.QuietHoursStart == nil || got.QuietHoursEnd == nil {
		t.Fatalf("quiet hours = (%v, %v), want both set", got.QuietHoursStart, got.QuietHoursEnd)
	}
}

func TestNotifySettings_QuietHours_OnlyOneTimeFilled_ShowsInlineError(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	rec := doForm(t, handler, http.MethodPost, "/settings/notify/quiet-hours", cookie,
		"csrf_token="+csrfToken+"&quiet_enabled=on&quiet_start=22:00")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /settings/notify/quiet-hours (only start filled): status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Enter both a start and end time") {
		t.Errorf("response missing inline validation message: %s", rec.Body.String())
	}
}

func TestNotifySettings_QuietHours_OwnerDisables(t *testing.T) {
	owner := settingsTestOwner()
	hhRepo := newMultiMemberHouseholdRepo(owner)
	handler, sm, _, _ := buildNotifySettingsTestHandler(t, hhRepo)
	cookie, csrfToken := seedAuthedSession(t, handler, sm, owner.ID.String())

	doForm(t, handler, http.MethodPost, "/settings/notify/quiet-hours", cookie,
		"csrf_token="+csrfToken+"&quiet_enabled=on&quiet_start=22:00&quiet_end=07:00")
	rec := doForm(t, handler, http.MethodPost, "/settings/notify/quiet-hours", cookie, "csrf_token="+csrfToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /settings/notify/quiet-hours (disable): status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	got, err := hhRepo.GetHousehold(context.Background(), owner.HouseholdID)
	if err != nil {
		t.Fatalf("GetHousehold: %v", err)
	}
	if got.QuietHoursStart != nil || got.QuietHoursEnd != nil {
		t.Errorf("quiet hours = (%v, %v), want both nil after disabling", got.QuietHoursStart, got.QuietHoursEnd)
	}
}
