package adapter_test

import (
	"context"
	"errors"
	"testing"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	notifyadapter "github.com/ericfisherdev/nestova/internal/notify/adapter"
	"github.com/ericfisherdev/nestova/internal/notify/domain"
)

// fakeSMSSender is an in-memory domain.SMSSender that records its last call.
type fakeSMSSender struct {
	lastTo   domain.E164Phone
	lastBody string
	id       string
	err      error
}

func (f *fakeSMSSender) Send(_ context.Context, to domain.E164Phone, body string) (string, error) {
	f.lastTo, f.lastBody = to, body
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

// fakeContactDirectory2 is an in-memory domain.ContactDirectory for
// SMSNotificationSender tests specifically (a separate package from
// notify/app's own fakeContactDirectory, so it cannot be shared).
type fakeContactDirectory2 struct {
	contact *domain.MemberContact
	err     error
}

func (f *fakeContactDirectory2) GetContact(_ context.Context, memberID household.MemberID) (*domain.MemberContact, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.contact != nil {
		return f.contact, nil
	}
	return &domain.MemberContact{MemberID: memberID}, nil
}

func (f *fakeContactDirectory2) SetPhone(context.Context, household.MemberID, *domain.E164Phone) error {
	return nil
}

func (f *fakeContactDirectory2) SetOptedIn(context.Context, household.MemberID, bool) error {
	return nil
}

func readySMSContact(t *testing.T, memberID household.MemberID) *domain.MemberContact {
	t.Helper()
	phone, err := domain.ParseE164Phone("+15551234567")
	if err != nil {
		t.Fatalf("ParseE164Phone: %v", err)
	}
	return &domain.MemberContact{MemberID: memberID, Phone: &phone, SMSOptedIn: true}
}

func notificationForMember(memberID household.MemberID, title, body string) *domain.Notification {
	return &domain.Notification{
		ID:          domain.NewNotificationID(),
		HouseholdID: household.NewHouseholdID(),
		MemberID:    &memberID,
		Channel:     domain.ChannelSMS,
		Title:       title,
		Body:        body,
	}
}

func TestSMSNotificationSender_Channel(t *testing.T) {
	s := notifyadapter.NewSMSNotificationSender(&fakeSMSSender{}, &fakeContactDirectory2{})
	if s.Channel() != domain.ChannelSMS {
		t.Errorf("Channel() = %v, want ChannelSMS", s.Channel())
	}
}

func TestNewSMSNotificationSender_PanicsOnNilSMSSender(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSMSNotificationSender(nil SMSSender) did not panic")
		}
	}()
	notifyadapter.NewSMSNotificationSender(nil, &fakeContactDirectory2{})
}

func TestNewSMSNotificationSender_PanicsOnNilContactDirectory(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSMSNotificationSender(nil ContactDirectory) did not panic")
		}
	}()
	notifyadapter.NewSMSNotificationSender(&fakeSMSSender{}, nil)
}

func TestSend_NilNotification_ReturnsError(t *testing.T) {
	s := notifyadapter.NewSMSNotificationSender(&fakeSMSSender{}, &fakeContactDirectory2{})
	if err := s.Send(context.Background(), nil); err == nil {
		t.Error("Send(nil) error = nil, want non-nil")
	}
}

func TestSend_NoMemberID_ReturnsError(t *testing.T) {
	s := notifyadapter.NewSMSNotificationSender(&fakeSMSSender{}, &fakeContactDirectory2{})
	n := &domain.Notification{ID: domain.NewNotificationID(), Channel: domain.ChannelSMS, Title: "t"} // MemberID nil
	if err := s.Send(context.Background(), n); err == nil {
		t.Error("Send(no member id) error = nil, want non-nil")
	}
}

func TestSend_ContactLookupError_ReturnsWrappedError(t *testing.T) {
	wantErr := errors.New("db unavailable")
	contacts := &fakeContactDirectory2{err: wantErr}
	s := notifyadapter.NewSMSNotificationSender(&fakeSMSSender{}, contacts)

	memberID := household.NewMemberID()
	err := s.Send(context.Background(), notificationForMember(memberID, "Title", "Body"))
	if !errors.Is(err, wantErr) {
		t.Errorf("Send error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestSend_MemberNotReady_ReturnsErrMemberNotSMSReady(t *testing.T) {
	tests := []struct {
		name    string
		contact *domain.MemberContact
	}{
		{"no phone on file", &domain.MemberContact{}},
		{"phone but not opted in", func() *domain.MemberContact {
			c := readySMSContact(t, household.NewMemberID())
			c.SMSOptedIn = false
			return c
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sms := &fakeSMSSender{}
			contacts := &fakeContactDirectory2{contact: tt.contact}
			s := notifyadapter.NewSMSNotificationSender(sms, contacts)

			memberID := household.NewMemberID()
			err := s.Send(context.Background(), notificationForMember(memberID, "Title", "Body"))
			if !errors.Is(err, domain.ErrMemberNotSMSReady) {
				t.Fatalf("Send error = %v, want ErrMemberNotSMSReady", err)
			}
			if sms.lastBody != "" {
				t.Error("the underlying SMSSender must never be called when the member is not sms-ready")
			}
		})
	}
}

func TestSend_MemberReady_CallsSMSSenderWithPhoneAndCombinedBody(t *testing.T) {
	memberID := household.NewMemberID()
	sms := &fakeSMSSender{id: "provider-id"}
	contacts := &fakeContactDirectory2{contact: readySMSContact(t, memberID)}
	s := notifyadapter.NewSMSNotificationSender(sms, contacts)

	err := s.Send(context.Background(), notificationForMember(memberID, "Claim expiring soon", "Complete it soon."))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sms.lastTo.String() != "+15551234567" {
		t.Errorf("SMSSender.Send to = %q, want +15551234567", sms.lastTo.String())
	}
	wantBody := "Claim expiring soon: Complete it soon."
	if sms.lastBody != wantBody {
		t.Errorf("SMSSender.Send body = %q, want %q", sms.lastBody, wantBody)
	}
}

func TestSend_EmptyBody_UsesTitleOnly(t *testing.T) {
	memberID := household.NewMemberID()
	sms := &fakeSMSSender{}
	contacts := &fakeContactDirectory2{contact: readySMSContact(t, memberID)}
	s := notifyadapter.NewSMSNotificationSender(sms, contacts)

	if err := s.Send(context.Background(), notificationForMember(memberID, "Title only", "")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sms.lastBody != "Title only" {
		t.Errorf("SMSSender.Send body = %q, want %q (no trailing colon for an empty Body)", sms.lastBody, "Title only")
	}
}

func TestSend_SMSSenderError_Propagates(t *testing.T) {
	memberID := household.NewMemberID()
	wantErr := domain.ErrRecipientOptedOut
	sms := &fakeSMSSender{err: wantErr}
	contacts := &fakeContactDirectory2{contact: readySMSContact(t, memberID)}
	s := notifyadapter.NewSMSNotificationSender(sms, contacts)

	err := s.Send(context.Background(), notificationForMember(memberID, "Title", "Body"))
	if !errors.Is(err, wantErr) {
		t.Errorf("Send error = %v, want %v", err, wantErr)
	}
}
