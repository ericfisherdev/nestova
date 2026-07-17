package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	pquernatotp "github.com/pquerna/otp/totp"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	authapp "github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	kioskadapter "github.com/ericfisherdev/nestova/internal/kiosk/adapter"
	kioskapp "github.com/ericfisherdev/nestova/internal/kiosk/app"
	"github.com/ericfisherdev/nestova/internal/platform/crypto"
	"github.com/ericfisherdev/nestova/internal/platform/totp"
)

// ---------------------------------------------------------------------------
// Fakes local to the /settings MFA test harness. buildKioskTestHandler's
// authedHouseholdRepo only ever resolves a single fixed member, which cannot
// exercise the household-owner admin reset flow (it needs a SECOND, distinct
// member in the same household) — multiMemberHouseholdRepo fills that gap.
// ---------------------------------------------------------------------------

type multiMemberHouseholdRepo struct {
	testHouseholdRepo
	members map[household.MemberID]*household.Member
}

func newMultiMemberHouseholdRepo(members ...*household.Member) *multiMemberHouseholdRepo {
	r := &multiMemberHouseholdRepo{members: make(map[household.MemberID]*household.Member)}
	for _, m := range members {
		r.members[m.ID] = m
	}
	return r
}

func (r *multiMemberHouseholdRepo) GetMember(_ context.Context, id household.MemberID) (*household.Member, error) {
	m, ok := r.members[id]
	if !ok {
		return nil, household.ErrMemberNotFound
	}
	return m, nil
}

func (r *multiMemberHouseholdRepo) ListMembers(_ context.Context, householdID household.HouseholdID) ([]*household.Member, error) {
	var out []*household.Member
	for _, m := range r.members {
		if m.HouseholdID == householdID {
			out = append(out, m)
		}
	}
	return out, nil
}

var _ household.HouseholdRepository = (*multiMemberHouseholdRepo)(nil)

// fakeMemberCredRepo is an in-memory authdomain.CredentialRepository keyed by
// member id, used for the owner-reauth flow's password verification.
type fakeMemberCredRepo struct {
	hashes map[household.MemberID]string
}

func newFakeMemberCredRepo() *fakeMemberCredRepo {
	return &fakeMemberCredRepo{hashes: make(map[household.MemberID]string)}
}

func (r *fakeMemberCredRepo) seedPassword(t *testing.T, memberID household.MemberID, password string) {
	t.Helper()
	hash, err := crypto.Hash(password)
	if err != nil {
		t.Fatalf("crypto.Hash: %v", err)
	}
	r.hashes[memberID] = hash
}

func (r *fakeMemberCredRepo) FindByEmail(_ context.Context, _ string) (*authdomain.Credential, error) {
	return nil, authdomain.ErrInvalidCredentials
}

func (r *fakeMemberCredRepo) FindByMemberID(_ context.Context, memberID household.MemberID) (*authdomain.Credential, error) {
	hash, ok := r.hashes[memberID]
	if !ok {
		return nil, authdomain.ErrInvalidCredentials
	}
	return &authdomain.Credential{MemberID: memberID, PasswordHash: hash}, nil
}

func (r *fakeMemberCredRepo) SetPassword(_ context.Context, memberID household.MemberID, _, hash string) error {
	r.hashes[memberID] = hash
	return nil
}

var _ authdomain.CredentialRepository = (*fakeMemberCredRepo)(nil)

// buildSettingsTestHandler wires the /settings route surface (kiosk device
// section + MFA section) against in-memory fakes, mirroring
// buildKioskTestHandler's approach but scoped to just what the MFA section
// needs, with a household repo that supports MULTIPLE members (required for
// the owner admin-reset flow).
func buildSettingsTestHandler(t *testing.T, hhRepo household.HouseholdRepository, credRepo authdomain.CredentialRepository) (http.Handler, *scs.SessionManager) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := newTestSessionManager()

	devices := newFakeKioskDeviceRepo()
	codes := newFakeActivationCodeRepo(devices)
	kioskSvc, err := kioskapp.NewKioskService(devices, codes, nil)
	if err != nil {
		t.Fatalf("NewKioskService: %v", err)
	}
	settingsHandlers := kioskadapter.NewSettingsWebHandlers(kioskSvc, sm, logger)

	cipher, err := crypto.NewCipher([]byte("settings-test-harness-mfa-cipher"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	mfaService, err := authapp.NewMFAService(newFakeMFARepo(), cipher, totp.NewProvider(), credRepo, logger)
	if err != nil {
		t.Fatalf("NewMFAService: %v", err)
	}
	mfaHandlers := authadapter.NewMFAWebHandlers(mfaService, hhRepo, sm, logger)

	authHandlers := authadapter.NewHandlers(sm, authapp.New(credRepo), logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", authHandlers.LoginPage)
	registerSettingsPage(mux, logger, sm, hhRepo, settingsHandlers, mfaHandlers)

	handler := sm.LoadAndSave(authadapter.Authenticate(sm, hhRepo)(mux))
	return handler, sm
}

func settingsTestOwner() *household.Member {
	return &household.Member{
		ID: household.NewMemberID(), HouseholdID: household.NewHouseholdID(),
		DisplayName: "Owner", Role: household.RoleOwner, Color: household.ColorClay,
	}
}

func settingsTestAdultInHousehold(householdID household.HouseholdID) *household.Member {
	return &household.Member{
		ID: household.NewMemberID(), HouseholdID: householdID,
		DisplayName: "Alice", Role: household.RoleAdult, Color: household.ColorSage,
	}
}

func settingsTestChildInHousehold(householdID household.HouseholdID) *household.Member {
	return &household.Member{
		ID: household.NewMemberID(), HouseholdID: householdID,
		DisplayName: "Kiddo", Role: household.RoleChild, Color: household.ColorOchre,
	}
}

// extractManualEntrySecret pulls the base32 TOTP secret out of the
// mfa-manual-secret input's value attribute in a rendered settings page body.
func extractManualEntrySecret(body string) string {
	return extractInputValue(body, "mfa-manual-secret")
}

// computeTOTPCode generates a currently-valid 6-digit code for secret, using
// the real pquerna/otp math (the same library internal/platform/totp wraps),
// so these tests exercise the actual RFC 6238 round trip end to end.
func computeTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := pquernatotp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("pquerna/otp GenerateCode: %v", err)
	}
	return code
}

// ---------------------------------------------------------------------------
// AC1 + AC5: every member reaches /settings for their own MFA section;
// the kiosk section is parent-only within the page.
// ---------------------------------------------------------------------------

func TestSettingsPage_MFASection_VisibleToChild_KioskSectionHidden(t *testing.T) {
	child := adminTestChild()
	hhRepo := newMultiMemberHouseholdRepo(child)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, child.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings as a child: status = %d, want 200 (NES-134 opens the page to every member)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Two-factor authentication") {
		t.Error("a child member must see their own MFA section")
	}
	if strings.Contains(rec.Body.String(), "Kiosk display") {
		t.Error("a child member must not see the parent-only kiosk section")
	}
}

func TestSettingsPage_MFASection_VisibleToParent_AlongsideKioskSection(t *testing.T) {
	adult := adminTestAdult()
	hhRepo := newMultiMemberHouseholdRepo(adult)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, _ := seedAuthedSession(t, handler, sm, adult.ID.String())

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings as a parent: status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Two-factor authentication") {
		t.Error("a parent member must see the MFA section")
	}
	if !strings.Contains(rec.Body.String(), "Kiosk display") {
		t.Error("a parent member must still see the kiosk section")
	}
}

// ---------------------------------------------------------------------------
// AC1 + AC2: enroll → confirm (with the real TOTP math) → recovery codes
// shown once → disenroll.
// ---------------------------------------------------------------------------

func TestMFAEnrollConfirmDisenroll_FullFlow(t *testing.T) {
	adult := adminTestAdult()
	hhRepo := newMultiMemberHouseholdRepo(adult)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	// 1. Enroll: reveals a QR + manual-entry secret.
	enrollReq := httptest.NewRequest(http.MethodPost, "/settings/mfa/enroll", strings.NewReader("csrf_token="+csrfToken))
	enrollReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	enrollReq.Header.Set("Cookie", cookie)
	enrollRec := httptest.NewRecorder()
	handler.ServeHTTP(enrollRec, enrollReq)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/mfa/enroll: status = %d, want 200; body: %s", enrollRec.Code, enrollRec.Body.String())
	}
	secret := extractManualEntrySecret(enrollRec.Body.String())
	if secret == "" {
		t.Fatal("could not extract the manual-entry secret from the enroll response")
	}
	if !strings.Contains(enrollRec.Body.String(), "data:image/png;base64,") {
		t.Error("enroll response missing the QR code reveal")
	}

	// 2. Confirm with a real, currently-valid TOTP code: reveals ten
	// recovery codes exactly once, with no-store caching.
	code := computeTOTPCode(t, secret)
	confirmReq := httptest.NewRequest(http.MethodPost, "/settings/mfa/confirm", strings.NewReader("csrf_token="+csrfToken+"&code="+code))
	confirmReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmReq.Header.Set("Cookie", cookie)
	confirmRec := httptest.NewRecorder()
	handler.ServeHTTP(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/mfa/confirm: status = %d, want 200; body: %s", confirmRec.Code, confirmRec.Body.String())
	}
	if cc := confirmRec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("confirm response Cache-Control = %q, want no-store (it reveals recovery codes)", cc)
	}
	if !strings.Contains(confirmRec.Body.String(), "Save these recovery codes") {
		t.Error("confirm response missing the recovery codes reveal panel")
	}
	if !strings.Contains(confirmRec.Body.String(), "Two-factor authentication is active") {
		t.Error("confirm response must show the active status")
	}

	// 3. A later GET must not re-display the secret or the recovery codes.
	followUp := httptest.NewRequest(http.MethodGet, "/settings", nil)
	followUp.Header.Set("Cookie", cookie)
	followUpRec := httptest.NewRecorder()
	handler.ServeHTTP(followUpRec, followUp)
	if strings.Contains(followUpRec.Body.String(), secret) {
		t.Error("a later GET /settings must not re-display the enrollment secret")
	}
	if strings.Contains(followUpRec.Body.String(), "Save these recovery codes") {
		t.Error("a later GET /settings must not re-display the recovery codes reveal")
	}

	// 4. Disenroll with a fresh valid code: redirects, and the member is
	// back to "not enrolled" (able to log in with password only — no active
	// enrollment remains).
	disenrollCode := computeTOTPCode(t, secret)
	disenrollReq := httptest.NewRequest(http.MethodPost, "/settings/mfa/disenroll", strings.NewReader("csrf_token="+csrfToken+"&totp_code="+disenrollCode))
	disenrollReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	disenrollReq.Header.Set("Cookie", cookie)
	disenrollRec := httptest.NewRecorder()
	handler.ServeHTTP(disenrollRec, disenrollReq)
	if disenrollRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/mfa/disenroll: status = %d, want 303; body: %s", disenrollRec.Code, disenrollRec.Body.String())
	}

	afterDisenroll := httptest.NewRequest(http.MethodGet, "/settings", nil)
	afterDisenroll.Header.Set("Cookie", cookie)
	afterDisenrollRec := httptest.NewRecorder()
	handler.ServeHTTP(afterDisenrollRec, afterDisenroll)
	if !strings.Contains(afterDisenrollRec.Body.String(), `action="/settings/mfa/enroll"`) {
		t.Error("after disenroll, the member must be back to the not-enrolled state (enroll form shown)")
	}
}

func TestMFAConfirm_WrongCode_ShowsGenericInlineError(t *testing.T) {
	adult := adminTestAdult()
	hhRepo := newMultiMemberHouseholdRepo(adult)
	handler, sm := buildSettingsTestHandler(t, hhRepo, newFakeMemberCredRepo())
	cookie, csrfToken := seedAuthedSession(t, handler, sm, adult.ID.String())

	enrollReq := httptest.NewRequest(http.MethodPost, "/settings/mfa/enroll", strings.NewReader("csrf_token="+csrfToken))
	enrollReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	enrollReq.Header.Set("Cookie", cookie)
	enrollRec := httptest.NewRecorder()
	handler.ServeHTTP(enrollRec, enrollReq)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("POST /settings/mfa/enroll: status = %d, want 200", enrollRec.Code)
	}

	confirmReq := httptest.NewRequest(http.MethodPost, "/settings/mfa/confirm", strings.NewReader("csrf_token="+csrfToken+"&code=000000"))
	confirmReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmReq.Header.Set("Cookie", cookie)
	confirmRec := httptest.NewRecorder()
	handler.ServeHTTP(confirmRec, confirmReq)

	if confirmRec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /settings/mfa/confirm with a wrong code: status = %d, want 401", confirmRec.Code)
	}
	if !strings.Contains(confirmRec.Body.String(), "could not be verified") {
		t.Errorf("wrong-code confirm response missing the inline error: %s", confirmRec.Body.String())
	}
	if strings.Contains(confirmRec.Body.String(), "Save these recovery codes") {
		t.Error("a rejected confirm must not reveal recovery codes")
	}
	if !strings.Contains(confirmRec.Body.String(), `action="/settings/mfa/confirm"`) {
		t.Error("a rejected confirm must re-show the confirm form (enrollment stays pending)")
	}
}

// ---------------------------------------------------------------------------
// AC3: household owner reset (with owner re-auth); wrong password and
// non-owner rejected.
// ---------------------------------------------------------------------------

func TestMFAAdminReset_OwnerCanResetAnotherMembersMFA(t *testing.T) {
	owner := settingsTestOwner()
	adult := settingsTestAdultInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, adult)
	credRepo := newFakeMemberCredRepo()
	credRepo.seedPassword(t, owner.ID, "owner-correct-password")
	handler, sm := buildSettingsTestHandler(t, hhRepo, credRepo)

	// The adult enrolls and confirms their own MFA first.
	adultCookie, adultCSRF := seedAuthedSession(t, handler, sm, adult.ID.String())
	enrollRec := doForm(t, handler, http.MethodPost, "/settings/mfa/enroll", adultCookie, "csrf_token="+adultCSRF)
	secret := extractManualEntrySecret(enrollRec.Body.String())
	if secret == "" {
		t.Fatal("could not extract the adult's enrollment secret")
	}
	confirmRec := doForm(t, handler, http.MethodPost, "/settings/mfa/confirm", adultCookie, "csrf_token="+adultCSRF+"&code="+computeTOTPCode(t, secret))
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("adult confirm: status = %d, want 200; body: %s", confirmRec.Code, confirmRec.Body.String())
	}

	// The owner resets the adult's MFA using their OWN password.
	ownerCookie, ownerCSRF := seedAuthedSession(t, handler, sm, owner.ID.String())
	resetRec := doForm(t, handler, http.MethodPost, "/settings/mfa/reset", ownerCookie,
		"csrf_token="+ownerCSRF+"&member_id="+adult.ID.String()+"&owner_password=owner-correct-password")
	if resetRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /settings/mfa/reset: status = %d, want 303; body: %s", resetRec.Code, resetRec.Body.String())
	}

	// The adult can now log in with password only — no active enrollment.
	afterReset := doForm(t, handler, http.MethodGet, "/settings", adultCookie, "")
	if !strings.Contains(afterReset.Body.String(), `action="/settings/mfa/enroll"`) {
		t.Error("after an owner reset, the target member must be back to the not-enrolled state")
	}
}

func TestMFAAdminReset_WrongPasswordRejected(t *testing.T) {
	owner := settingsTestOwner()
	adult := settingsTestAdultInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, adult)
	credRepo := newFakeMemberCredRepo()
	credRepo.seedPassword(t, owner.ID, "owner-correct-password")
	handler, sm := buildSettingsTestHandler(t, hhRepo, credRepo)

	adultCookie, adultCSRF := seedAuthedSession(t, handler, sm, adult.ID.String())
	enrollRec := doForm(t, handler, http.MethodPost, "/settings/mfa/enroll", adultCookie, "csrf_token="+adultCSRF)
	secret := extractManualEntrySecret(enrollRec.Body.String())
	doForm(t, handler, http.MethodPost, "/settings/mfa/confirm", adultCookie, "csrf_token="+adultCSRF+"&code="+computeTOTPCode(t, secret))

	ownerCookie, ownerCSRF := seedAuthedSession(t, handler, sm, owner.ID.String())
	resetRec := doForm(t, handler, http.MethodPost, "/settings/mfa/reset", ownerCookie,
		"csrf_token="+ownerCSRF+"&member_id="+adult.ID.String()+"&owner_password=totally-wrong-password")
	if resetRec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /settings/mfa/reset with a wrong owner password: status = %d, want 401; body: %s", resetRec.Code, resetRec.Body.String())
	}

	// The target's enrollment must survive a failed reset attempt.
	afterFailedReset := doForm(t, handler, http.MethodGet, "/settings", adultCookie, "")
	if !strings.Contains(afterFailedReset.Body.String(), "Two-factor authentication is active") {
		t.Error("a failed reset (wrong owner password) must not remove the target's enrollment")
	}
}

func TestMFAAdminReset_NonOwnerRejected(t *testing.T) {
	owner := settingsTestOwner()
	otherAdult := settingsTestAdultInHousehold(owner.HouseholdID)
	target := settingsTestChildInHousehold(owner.HouseholdID)
	hhRepo := newMultiMemberHouseholdRepo(owner, otherAdult, target)
	credRepo := newFakeMemberCredRepo()
	handler, sm := buildSettingsTestHandler(t, hhRepo, credRepo)

	// otherAdult is a parent (IsParent() is true) but NOT the owner — the
	// ticket requires the household OWNER specifically.
	cookie, csrfToken := seedAuthedSession(t, handler, sm, otherAdult.ID.String())
	resetRec := doForm(t, handler, http.MethodPost, "/settings/mfa/reset", cookie,
		"csrf_token="+csrfToken+"&member_id="+target.ID.String()+"&owner_password=irrelevant")
	if resetRec.Code != http.StatusForbidden {
		t.Fatalf("POST /settings/mfa/reset as a non-owner adult: status = %d, want 403", resetRec.Code)
	}
}

// doForm issues a form-encoded request (GET when body is empty, POST
// otherwise) against handler with cookie attached, returning the recorder.
func doForm(t *testing.T, handler http.Handler, method, path, cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if method == http.MethodGet {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
