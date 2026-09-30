package app_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ericfisherdev/nestova/internal/auth/app"
	authdomain "github.com/ericfisherdev/nestova/internal/auth/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// fakeRememberedDeviceRepo is an in-memory authdomain.RememberedDeviceRepository
// with the real repository's MarkUsed semantics (owner + unexpired only).
type fakeRememberedDeviceRepo struct {
	devices   []*authdomain.RememberedDevice
	createErr error
	markErr   error
}

func (f *fakeRememberedDeviceRepo) Create(_ context.Context, d *authdomain.RememberedDevice) error {
	if f.createErr != nil {
		return f.createErr
	}
	cp := *d
	f.devices = append(f.devices, &cp)
	return nil
}

func (f *fakeRememberedDeviceRepo) MarkUsed(_ context.Context, memberID household.MemberID, tokenHash []byte, now time.Time) error {
	if f.markErr != nil {
		return f.markErr
	}
	for _, d := range f.devices {
		if d.MemberID == memberID && string(d.TokenHash) == string(tokenHash) && d.ExpiresAt.After(now) {
			d.LastUsedAt = now
			return nil
		}
	}
	return authdomain.ErrRememberedDeviceNotFound
}

func (f *fakeRememberedDeviceRepo) RevokeAllForMember(_ context.Context, memberID household.MemberID) error {
	kept := f.devices[:0]
	for _, d := range f.devices {
		if d.MemberID != memberID {
			kept = append(kept, d)
		}
	}
	f.devices = kept
	return nil
}

func newRememberService(t *testing.T) (*app.RememberDeviceService, *fakeRememberedDeviceRepo) {
	t.Helper()
	repo := &fakeRememberedDeviceRepo{}
	svc, err := app.NewRememberDeviceService(repo)
	if err != nil {
		t.Fatalf("NewRememberDeviceService: %v", err)
	}
	return svc, repo
}

func TestNewRememberDeviceService_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := app.NewRememberDeviceService(nil); err == nil {
		t.Error("NewRememberDeviceService(nil) must return an error")
	}
}

func TestRememberDevice_IssueThenIsRemembered(t *testing.T) {
	t.Parallel()
	svc, repo := newRememberService(t)
	ctx := context.Background()
	member := household.NewMemberID()
	now := time.Unix(1_700_000_000, 0)

	token, err := svc.Issue(ctx, member, "Mozilla/5.0", now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := svc.IsRemembered(ctx, member, token, now.Add(time.Hour))
	if err != nil || !got {
		t.Fatalf("IsRemembered(fresh token) = (%v, %v), want (true, nil)", got, err)
	}
	if want := now.Add(app.RememberDeviceTTL); !repo.devices[0].ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v (30 days)", repo.devices[0].ExpiresAt, want)
	}
	if !repo.devices[0].LastUsedAt.Equal(now.Add(time.Hour)) {
		t.Errorf("LastUsedAt = %v, want the check time", repo.devices[0].LastUsedAt)
	}
}

func TestRememberDevice_StoresOnlyTheHash(t *testing.T) {
	t.Parallel()
	svc, repo := newRememberService(t)
	token, err := svc.Issue(context.Background(), household.NewMemberID(), "", time.Now())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("token carries %d bytes, want 32", len(raw))
	}
	if string(repo.devices[0].TokenHash) == string(raw) || len(repo.devices[0].TokenHash) != 32 {
		t.Error("the stored value must be a 32-byte hash of the token, not the token")
	}
}

func TestRememberDevice_TokensAreUnique(t *testing.T) {
	t.Parallel()
	svc, _ := newRememberService(t)
	member := household.NewMemberID()
	a, errA := svc.Issue(context.Background(), member, "", time.Now())
	b, errB := svc.Issue(context.Background(), member, "", time.Now())
	if errA != nil || errB != nil {
		t.Fatalf("Issue: %v, %v", errA, errB)
	}
	if a == b {
		t.Error("two issued tokens must differ")
	}
}

func TestRememberDevice_IsRemembered_Rejections(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name  string
		token func(t *testing.T, svc *app.RememberDeviceService, owner household.MemberID) string
		as    func(owner household.MemberID) household.MemberID
		at    time.Time
	}{
		{
			name:  "expired",
			token: issueAt(now),
			as:    func(o household.MemberID) household.MemberID { return o },
			at:    now.Add(app.RememberDeviceTTL + time.Second),
		},
		{
			name:  "another member's token",
			token: issueAt(now),
			as:    func(household.MemberID) household.MemberID { return household.NewMemberID() },
			at:    now,
		},
		{
			name: "never issued",
			token: func(*testing.T, *app.RememberDeviceService, household.MemberID) string {
				return base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			},
			as: func(o household.MemberID) household.MemberID { return o },
			at: now,
		},
		{
			name:  "not base64",
			token: func(*testing.T, *app.RememberDeviceService, household.MemberID) string { return "%%%not-a-token%%%" },
			as:    func(o household.MemberID) household.MemberID { return o },
			at:    now,
		},
		{
			name: "wrong length",
			token: func(*testing.T, *app.RememberDeviceService, household.MemberID) string {
				return base64.RawURLEncoding.EncodeToString([]byte("short"))
			},
			as: func(o household.MemberID) household.MemberID { return o },
			at: now,
		},
		{
			name:  "empty",
			token: func(*testing.T, *app.RememberDeviceService, household.MemberID) string { return "" },
			as:    func(o household.MemberID) household.MemberID { return o },
			at:    now,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newRememberService(t)
			owner := household.NewMemberID()
			token := tc.token(t, svc, owner)
			got, err := svc.IsRemembered(context.Background(), tc.as(owner), token, tc.at)
			if err != nil || got {
				t.Errorf("IsRemembered = (%v, %v), want (false, nil)", got, err)
			}
		})
	}
}

func issueAt(now time.Time) func(*testing.T, *app.RememberDeviceService, household.MemberID) string {
	return func(t *testing.T, svc *app.RememberDeviceService, owner household.MemberID) string {
		t.Helper()
		token, err := svc.Issue(context.Background(), owner, "", now)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		return token
	}
}

func TestRememberDevice_RevokeAll_InvalidatesOnlyThatMember(t *testing.T) {
	t.Parallel()
	svc, _ := newRememberService(t)
	ctx := context.Background()
	now := time.Now()
	alice, bob := household.NewMemberID(), household.NewMemberID()
	aliceToken, err := svc.Issue(ctx, alice, "", now)
	if err != nil {
		t.Fatalf("Issue alice: %v", err)
	}
	bobToken, err := svc.Issue(ctx, bob, "", now)
	if err != nil {
		t.Fatalf("Issue bob: %v", err)
	}

	if err := svc.RevokeAll(ctx, alice); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if got, _ := svc.IsRemembered(ctx, alice, aliceToken, now); got {
		t.Error("a revoked token must no longer be remembered")
	}
	if got, _ := svc.IsRemembered(ctx, bob, bobToken, now); !got {
		t.Error("revoking one member must not affect another")
	}
}

func TestRememberDevice_IsRemembered_LookupErrorSurfaces(t *testing.T) {
	t.Parallel()
	svc, repo := newRememberService(t)
	repo.markErr = errors.New("db down")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	got, err := svc.IsRemembered(context.Background(), household.NewMemberID(), token, time.Now())
	if err == nil || got {
		t.Errorf("IsRemembered = (%v, %v), want (false, error)", got, err)
	}
}

func TestRememberDevice_Issue_TruncatesAndSanitizesUserAgent(t *testing.T) {
	t.Parallel()
	svc, repo := newRememberService(t)
	long := strings.Repeat("é", authdomain.MaxUserAgentLength+50) + "\xff"
	if _, err := svc.Issue(context.Background(), household.NewMemberID(), long, time.Now()); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ua := repo.devices[0].UserAgent
	if n := utf8.RuneCountInString(ua); n != authdomain.MaxUserAgentLength {
		t.Errorf("stored user agent = %d runes, want %d", n, authdomain.MaxUserAgentLength)
	}
	if !utf8.ValidString(ua) {
		t.Error("stored user agent must be valid UTF-8")
	}
}

func TestRememberDevice_Issue_RepoErrorSurfaces(t *testing.T) {
	t.Parallel()
	svc, repo := newRememberService(t)
	repo.createErr = household.ErrMemberNotFound
	token, err := svc.Issue(context.Background(), household.NewMemberID(), "", time.Now())
	if !errors.Is(err, household.ErrMemberNotFound) || token != "" {
		t.Errorf("Issue = (%q, %v), want (\"\", ErrMemberNotFound)", token, err)
	}
}
