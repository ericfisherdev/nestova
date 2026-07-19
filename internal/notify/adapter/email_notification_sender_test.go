package adapter_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// fakeEmailSender is an in-memory domain.EmailSender that records its last
// call.
type fakeEmailSender struct {
	lastTo, lastSubject, lastHTML, lastText string
	id                                      string
	err                                     error
}

func (f *fakeEmailSender) Send(_ context.Context, to, subject, htmlBody, textBody string) (string, error) {
	f.lastTo, f.lastSubject, f.lastHTML, f.lastText = to, subject, htmlBody, textBody
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

// fakeEmailResolver is an in-memory domain.MemberEmailResolver.
type fakeEmailResolver struct {
	email string
	err   error
}

func (f *fakeEmailResolver) ResolveEmail(context.Context, household.MemberID) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.email, nil
}

// fakePreferenceRepo is an in-memory domain.PreferenceRepository for
// EmailNotificationSender tests, recording DowngradeChannel calls.
type fakePreferenceRepo struct {
	downgradeCalls []struct {
		memberID household.MemberID
		from, to domain.Channel
	}
	downgradeErr error
}

func (f *fakePreferenceRepo) Get(context.Context, household.MemberID, domain.EventType) (domain.Channel, error) {
	return "", domain.ErrPreferenceNotFound
}

func (f *fakePreferenceRepo) Set(context.Context, domain.MemberPreference) error { return nil }

func (f *fakePreferenceRepo) ListForMember(context.Context, household.MemberID) ([]domain.MemberPreference, error) {
	return nil, nil
}

func (f *fakePreferenceRepo) DowngradeChannel(_ context.Context, memberID household.MemberID, from, to domain.Channel) error {
	f.downgradeCalls = append(f.downgradeCalls, struct {
		memberID household.MemberID
		from, to domain.Channel
	}{memberID, from, to})
	return f.downgradeErr
}

// fakeMemberLister is an in-memory emailMemberLister (structurally
// satisfied — that type is unexported in package adapter, so this fake is
// never asserted against it by name, only passed where it is expected).
type fakeMemberLister struct {
	members []*household.Member
	err     error
}

func (f *fakeMemberLister) ListMembers(context.Context, household.HouseholdID) ([]*household.Member, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.members, nil
}

// fakeEnqueuer is an in-memory domain.Enqueuer, recording every enqueued
// notification.
type fakeEnqueuer struct {
	enqueued []*domain.Notification
	err      error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, n *domain.Notification) error {
	if f.err != nil {
		return f.err
	}
	f.enqueued = append(f.enqueued, n)
	return nil
}

func discardEmailLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func emailNotificationForMember(memberID household.MemberID, householdID household.HouseholdID, title, body string) *domain.Notification {
	return &domain.Notification{
		ID:          domain.NewNotificationID(),
		HouseholdID: householdID,
		MemberID:    &memberID,
		Channel:     domain.ChannelEmail,
		Title:       title,
		Body:        body,
	}
}

func TestEmailNotificationSender_Channel(t *testing.T) {
	s := notifyadapter.NewEmailNotificationSender(&fakeEmailSender{}, &fakeEmailResolver{}, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger())
	if s.Channel() != domain.ChannelEmail {
		t.Errorf("Channel() = %v, want ChannelEmail", s.Channel())
	}
}

func TestNewEmailNotificationSender_PanicsOnNilDependency(t *testing.T) {
	valid := func() (domain.EmailSender, domain.MemberEmailResolver, domain.PreferenceRepository, *fakeMemberLister, domain.Enqueuer, *slog.Logger) {
		return &fakeEmailSender{}, &fakeEmailResolver{}, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger()
	}

	t.Run("nil email sender", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("did not panic")
			}
		}()
		_, resolver, prefs, members, enqueuer, logger := valid()
		notifyadapter.NewEmailNotificationSender(nil, resolver, prefs, members, enqueuer, logger)
	})
	t.Run("nil resolver", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("did not panic")
			}
		}()
		email, _, prefs, members, enqueuer, logger := valid()
		notifyadapter.NewEmailNotificationSender(email, nil, prefs, members, enqueuer, logger)
	})
	t.Run("nil preferences", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("did not panic")
			}
		}()
		email, resolver, _, members, enqueuer, logger := valid()
		notifyadapter.NewEmailNotificationSender(email, resolver, nil, members, enqueuer, logger)
	})
	t.Run("nil members", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("did not panic")
			}
		}()
		email, resolver, prefs, _, enqueuer, logger := valid()
		notifyadapter.NewEmailNotificationSender(email, resolver, prefs, nil, enqueuer, logger)
	})
	t.Run("nil enqueuer", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("did not panic")
			}
		}()
		email, resolver, prefs, members, _, logger := valid()
		notifyadapter.NewEmailNotificationSender(email, resolver, prefs, members, nil, logger)
	})
	t.Run("nil logger", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("did not panic")
			}
		}()
		email, resolver, prefs, members, enqueuer, _ := valid()
		notifyadapter.NewEmailNotificationSender(email, resolver, prefs, members, enqueuer, nil)
	})
}

func TestEmailNotificationSenderSend_NilNotification_ReturnsError(t *testing.T) {
	s := notifyadapter.NewEmailNotificationSender(&fakeEmailSender{}, &fakeEmailResolver{}, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger())
	if err := s.Send(context.Background(), nil); err == nil {
		t.Error("Send(nil) error = nil, want non-nil")
	}
}

func TestEmailNotificationSenderSend_NoMemberID_ReturnsError(t *testing.T) {
	s := notifyadapter.NewEmailNotificationSender(&fakeEmailSender{}, &fakeEmailResolver{}, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger())
	n := &domain.Notification{ID: domain.NewNotificationID(), Channel: domain.ChannelEmail, Title: "t"} // MemberID nil
	if err := s.Send(context.Background(), n); err == nil {
		t.Error("Send(no member id) error = nil, want non-nil")
	}
}

func TestEmailNotificationSenderSend_ResolveEmailError_ReturnsWrappedError(t *testing.T) {
	wantErr := errors.New("no email on file")
	resolver := &fakeEmailResolver{err: wantErr}
	s := notifyadapter.NewEmailNotificationSender(&fakeEmailSender{}, resolver, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger())

	memberID := household.NewMemberID()
	err := s.Send(context.Background(), emailNotificationForMember(memberID, household.NewHouseholdID(), "Title", "Body"))
	if !errors.Is(err, wantErr) {
		t.Errorf("Send error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestEmailNotificationSenderSend_MemberReady_CallsEmailSenderWithResolvedAddress(t *testing.T) {
	memberID := household.NewMemberID()
	email := &fakeEmailSender{id: "provider-id"}
	resolver := &fakeEmailResolver{email: "member@example.com"}
	s := notifyadapter.NewEmailNotificationSender(email, resolver, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger())

	err := s.Send(context.Background(), emailNotificationForMember(memberID, household.NewHouseholdID(), "Claim expiring soon", "Complete it soon."))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if email.lastTo != "member@example.com" {
		t.Errorf("EmailSender.Send to = %q, want %q", email.lastTo, "member@example.com")
	}
	if email.lastSubject != "Claim expiring soon" {
		t.Errorf("EmailSender.Send subject = %q, want %q", email.lastSubject, "Claim expiring soon")
	}
	if email.lastHTML == "" {
		t.Error("EmailSender.Send htmlBody is empty, want a rendered HTML document")
	}
	if email.lastText == "" {
		t.Error("EmailSender.Send textBody is empty, want a rendered plain-text body")
	}
	// Both parts must carry the actual notification content.
	if !strings.Contains(email.lastHTML, "Claim expiring soon") || !strings.Contains(email.lastHTML, "Complete it soon.") {
		t.Errorf("htmlBody = %q, want it to contain the title and body", email.lastHTML)
	}
	if !strings.Contains(email.lastText, "Claim expiring soon") || !strings.Contains(email.lastText, "Complete it soon.") {
		t.Errorf("textBody = %q, want it to contain the title and body", email.lastText)
	}
}

func TestEmailNotificationSenderSend_EmailSenderError_Propagates(t *testing.T) {
	memberID := household.NewMemberID()
	wantErr := errors.New("provider unavailable")
	email := &fakeEmailSender{err: wantErr}
	resolver := &fakeEmailResolver{email: "member@example.com"}
	s := notifyadapter.NewEmailNotificationSender(email, resolver, &fakePreferenceRepo{}, &fakeMemberLister{}, &fakeEnqueuer{}, discardEmailLogger())

	err := s.Send(context.Background(), emailNotificationForMember(memberID, household.NewHouseholdID(), "Title", "Body"))
	if !errors.Is(err, wantErr) {
		t.Errorf("Send error = %v, want %v", err, wantErr)
	}
}

// ---------------------------------------------------------------------------
// Bounce handling (NES-141): a domain.ErrRecipientRejected send failure
// additionally downgrades the member's email preferences and warns owners.
// ---------------------------------------------------------------------------

func TestEmailNotificationSenderSend_Rejected_DowngradesPreferenceAndWarnsOwners(t *testing.T) {
	householdID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	ownerID := household.NewMemberID()
	nonOwnerID := household.NewMemberID()

	email := &fakeEmailSender{err: domain.ErrRecipientRejected}
	resolver := &fakeEmailResolver{email: "bounced@example.com"}
	prefs := &fakePreferenceRepo{}
	members := &fakeMemberLister{members: []*household.Member{
		{ID: memberID, HouseholdID: householdID, DisplayName: "Bounced Member", Role: household.RoleAdult},
		{ID: ownerID, HouseholdID: householdID, DisplayName: "Owner Member", Role: household.RoleOwner},
		{ID: nonOwnerID, HouseholdID: householdID, DisplayName: "Other Adult", Role: household.RoleAdult},
	}}
	enqueuer := &fakeEnqueuer{}
	s := notifyadapter.NewEmailNotificationSender(email, resolver, prefs, members, enqueuer, discardEmailLogger())

	err := s.Send(context.Background(), emailNotificationForMember(memberID, householdID, "Title", "Body"))
	if !errors.Is(err, domain.ErrRecipientRejected) {
		t.Fatalf("Send error = %v, want ErrRecipientRejected", err)
	}

	// The preference downgrade ran for the AFFECTED member, email -> inapp.
	if len(prefs.downgradeCalls) != 1 {
		t.Fatalf("DowngradeChannel calls = %d, want 1", len(prefs.downgradeCalls))
	}
	call := prefs.downgradeCalls[0]
	if call.memberID != memberID || call.from != domain.ChannelEmail || call.to != domain.ChannelInApp {
		t.Errorf("DowngradeChannel call = %+v, want member=%v from=email to=inapp", call, memberID)
	}

	// Exactly one warning was enqueued, addressed to the OWNER only (not the
	// non-owner adult), naming the affected member.
	if len(enqueuer.enqueued) != 1 {
		t.Fatalf("enqueued warnings = %d, want 1 (owner only)", len(enqueuer.enqueued))
	}
	warning := enqueuer.enqueued[0]
	if warning.MemberID == nil || *warning.MemberID != ownerID {
		t.Errorf("warning.MemberID = %v, want the owner %v", warning.MemberID, ownerID)
	}
	if warning.Channel != domain.ChannelInApp {
		t.Errorf("warning.Channel = %v, want ChannelInApp", warning.Channel)
	}
	if warning.HouseholdID != householdID {
		t.Error("warning.HouseholdID does not match the original notification's household")
	}
	if !strings.Contains(warning.Body, "Bounced Member") {
		t.Errorf("warning.Body = %q, want it to name the affected member", warning.Body)
	}
}

func TestEmailNotificationSenderSend_NonRejectionFailure_NoBounceHandling(t *testing.T) {
	memberID := household.NewMemberID()
	email := &fakeEmailSender{err: errors.New("transient provider error")}
	resolver := &fakeEmailResolver{email: "member@example.com"}
	prefs := &fakePreferenceRepo{}
	enqueuer := &fakeEnqueuer{}
	s := notifyadapter.NewEmailNotificationSender(email, resolver, prefs, &fakeMemberLister{}, enqueuer, discardEmailLogger())

	if err := s.Send(context.Background(), emailNotificationForMember(memberID, household.NewHouseholdID(), "Title", "Body")); err == nil {
		t.Fatal("Send error = nil, want non-nil")
	}
	if len(prefs.downgradeCalls) != 0 {
		t.Errorf("DowngradeChannel calls = %d, want 0 for a non-rejection failure", len(prefs.downgradeCalls))
	}
	if len(enqueuer.enqueued) != 0 {
		t.Errorf("enqueued warnings = %d, want 0 for a non-rejection failure", len(enqueuer.enqueued))
	}
}

func TestEmailNotificationSenderSend_Rejected_BounceSideEffectFailuresDoNotChangeReturnedError(t *testing.T) {
	memberID := household.NewMemberID()
	email := &fakeEmailSender{err: domain.ErrRecipientRejected}
	resolver := &fakeEmailResolver{email: "bounced@example.com"}
	prefs := &fakePreferenceRepo{downgradeErr: errors.New("db unavailable")}
	members := &fakeMemberLister{err: errors.New("db unavailable")}
	enqueuer := &fakeEnqueuer{}
	s := notifyadapter.NewEmailNotificationSender(email, resolver, prefs, members, enqueuer, discardEmailLogger())

	err := s.Send(context.Background(), emailNotificationForMember(memberID, household.NewHouseholdID(), "Title", "Body"))
	if !errors.Is(err, domain.ErrRecipientRejected) {
		t.Errorf("Send error = %v, want it to still be ErrRecipientRejected even though the best-effort bounce side effects both failed", err)
	}
}

func TestEmailNotificationSenderSend_Rejected_NoOwners_NoWarningEnqueued(t *testing.T) {
	householdID := household.NewHouseholdID()
	memberID := household.NewMemberID()
	email := &fakeEmailSender{err: domain.ErrRecipientRejected}
	resolver := &fakeEmailResolver{email: "bounced@example.com"}
	prefs := &fakePreferenceRepo{}
	members := &fakeMemberLister{members: []*household.Member{
		{ID: memberID, HouseholdID: householdID, DisplayName: "Bounced Member", Role: household.RoleAdult},
	}}
	enqueuer := &fakeEnqueuer{}
	s := notifyadapter.NewEmailNotificationSender(email, resolver, prefs, members, enqueuer, discardEmailLogger())

	if err := s.Send(context.Background(), emailNotificationForMember(memberID, householdID, "Title", "Body")); !errors.Is(err, domain.ErrRecipientRejected) {
		t.Fatalf("Send error = %v, want ErrRecipientRejected", err)
	}
	if len(enqueuer.enqueued) != 0 {
		t.Errorf("enqueued warnings = %d, want 0 (no owner-role member in the household)", len(enqueuer.enqueued))
	}
}
