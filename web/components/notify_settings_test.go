package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func TestSettingsPage_NotifySection_NoPhone_HidesOptInForm(t *testing.T) {
	view := components.SettingsView{
		Notify:    components.NotifySettingsView{Phone: "", SMSAvailable: true, CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `action="/settings/notify/phone"`) {
		t.Errorf("notify section missing the phone entry form: %q", out)
	}
	if strings.Contains(out, `action="/settings/notify/opt-in"`) {
		t.Errorf("notify section must not show the opt-in form before a phone is on file: %q", out)
	}
}

func TestSettingsPage_NotifySection_WithPhone_ShowsOptInForm(t *testing.T) {
	view := components.SettingsView{
		Notify:    components.NotifySettingsView{Phone: "+15551234567", SMSAvailable: true, CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `action="/settings/notify/opt-in"`) {
		t.Errorf("notify section missing the opt-in form once a phone is on file: %q", out)
	}
	if !strings.Contains(out, "+15551234567") {
		t.Errorf("notify section missing the current phone value: %q", out)
	}
}

func TestSettingsPage_NotifySection_OptedIn_SMSOptionSelectable(t *testing.T) {
	view := components.SettingsView{
		Notify: components.NotifySettingsView{
			Phone:        "+15551234567",
			OptedIn:      true,
			SMSAvailable: true,
			CSRFToken:    "csrf-test",
			Preferences: []components.NotifyPreferenceRow{
				{EventType: "claim_expiring", Label: "Claim expiring soon", Channel: "sms"},
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if strings.Contains(out, `value="sms" disabled`) {
		t.Errorf("the sms option must not be disabled once the member is opted in: %q", out)
	}
	if !strings.Contains(out, `value="sms" selected`) {
		t.Errorf("the claim_expiring row must show sms selected: %q", out)
	}
}

func TestSettingsPage_NotifySection_NotOptedIn_SMSOptionDisabled(t *testing.T) {
	// NES-139 AC: "Preferences UI prevents enabling SMS without a valid
	// opted-in phone number" — the sms <option> must be disabled whenever
	// OptedIn is false, regardless of Phone.
	view := components.SettingsView{
		Notify: components.NotifySettingsView{
			Phone:        "+15551234567",
			OptedIn:      false,
			SMSAvailable: true,
			CSRFToken:    "csrf-test",
			Preferences: []components.NotifyPreferenceRow{
				{EventType: "claim_expiring", Label: "Claim expiring soon", Channel: "inapp"},
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `value="sms" disabled`) {
		t.Errorf("the sms option must be disabled when the member is not opted in: %q", out)
	}
}

// TestSettingsPage_NotifySection_EmailOption_AlwaysSelectableAndNeverDisabled
// is the NES-141 regression test: unlike sms (gated on OptedIn), the
// email option must always be present, never carry a disabled attribute,
// and reflect the member's current selection when set — every member
// reaching the settings page already has an email (login requires one),
// so there is no readiness gate to render here (see
// EmailNotificationSender's own doc for why resolution instead happens
// entirely at send time).
func TestSettingsPage_NotifySection_EmailOption_AlwaysSelectableAndNeverDisabled(t *testing.T) {
	view := components.SettingsView{
		Notify: components.NotifySettingsView{
			EmailAvailable: true,
			CSRFToken:      "csrf-test",
			Preferences: []components.NotifyPreferenceRow{
				{EventType: "claim_expiring", Label: "Claim expiring soon", Channel: "email"},
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if strings.Contains(out, `value="email" disabled`) {
		t.Errorf("the email option must never be disabled: %q", out)
	}
	if !strings.Contains(out, `value="email" selected`) {
		t.Errorf("the claim_expiring row must show email selected: %q", out)
	}
}

func TestSettingsPage_NotifySection_ErrorMessage_RendersInline(t *testing.T) {
	view := components.SettingsView{
		Notify:    components.NotifySettingsView{CSRFToken: "csrf-test", Error: "Enter a valid phone number, e.g. +15551234567."},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, "Enter a valid phone number") {
		t.Errorf("notify section missing the inline error message: %q", out)
	}
}

func TestSettingsPage_QuietHoursSection_HiddenWhenNotShown(t *testing.T) {
	view := components.SettingsView{
		Notify:                components.NotifySettingsView{CSRFToken: "csrf-test"},
		ShowQuietHoursSection: false,
		CSRFToken:             "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if strings.Contains(out, `action="/settings/notify/quiet-hours"`) {
		t.Errorf("quiet hours section must be entirely absent when ShowQuietHoursSection is false: %q", out)
	}
}

func TestSettingsPage_QuietHoursSection_ShownForOwner(t *testing.T) {
	view := components.SettingsView{
		Notify:                components.NotifySettingsView{CSRFToken: "csrf-test"},
		ShowQuietHoursSection: true,
		QuietHours: components.QuietHoursSettingsView{
			Enabled:    true,
			StartValue: "22:00",
			EndValue:   "07:00",
			CSRFToken:  "csrf-test",
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `action="/settings/notify/quiet-hours"`) {
		t.Errorf("quiet hours section missing when ShowQuietHoursSection is true: %q", out)
	}
	if !strings.Contains(out, `value="22:00"`) || !strings.Contains(out, `value="07:00"`) {
		t.Errorf("quiet hours section missing the current start/end values: %q", out)
	}
}

func TestSettingsPage_QuietHoursSection_ErrorMessage_RendersInline(t *testing.T) {
	view := components.SettingsView{
		Notify:                components.NotifySettingsView{CSRFToken: "csrf-test"},
		ShowQuietHoursSection: true,
		QuietHours:            components.QuietHoursSettingsView{CSRFToken: "csrf-test", Error: "Enter both a start and end time, or turn quiet hours off."},
		CSRFToken:             "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, "Enter both a start and end time") {
		t.Errorf("quiet hours section missing the inline error message: %q", out)
	}
}

func TestSettingsPage_NotifySection_NoSMSSender_HidesSMSControls(t *testing.T) {
	view := components.SettingsView{
		Notify: components.NotifySettingsView{
			CSRFToken: "csrf-test",
			Preferences: []components.NotifyPreferenceRow{
				{EventType: "claim_expiring", Label: "Claim expiring soon", Channel: "inapp"},
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	for _, hidden := range []string{`action="/settings/notify/phone"`, `action="/settings/notify/opt-in"`, `value="sms"`, `value="email"`, "SMS messages"} {
		if strings.Contains(out, hidden) {
			t.Errorf("notify section must not contain %q without an SMS or email sender: %q", hidden, out)
		}
	}
	if !strings.Contains(out, `value="inapp"`) {
		t.Errorf("the in-app option must always be offered: %q", out)
	}
}

// A member whose number and consent were stored while SMS was wired must
// still be able to remove the number and withdraw consent once it is not,
// but must not be able to add a number or give consent.
func TestSettingsPage_NotifySection_NoSMSSender_StoredContact_OffersWithdrawalOnly(t *testing.T) {
	view := components.SettingsView{
		Notify:    components.NotifySettingsView{Phone: "+15551234567", OptedIn: true, CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	for _, want := range []string{
		`action="/settings/notify/phone"`,
		`<input type="hidden" name="phone" value="">`,
		"Remove phone number",
		`action="/settings/notify/opt-in"`,
		"Withdraw text message consent",
		"+15551234567",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("notify section missing %q for a member with a stored phone and consent: %q", want, out)
		}
	}
	for _, hidden := range []string{`id="notify-phone"`, `type="tel"`, `type="checkbox" name="opted_in"`, `id="notify-opted-in"`, "SMS messages"} {
		if strings.Contains(out, hidden) {
			t.Errorf("notify section must not let the member add a number or consent, found %q: %q", hidden, out)
		}
	}
}

func TestSettingsPage_NotifySection_NoSMSSender_PhoneWithoutConsent_OffersRemovalOnly(t *testing.T) {
	view := components.SettingsView{
		Notify:    components.NotifySettingsView{Phone: "+15551234567", CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, "Remove phone number") {
		t.Errorf("notify section missing the phone removal control: %q", out)
	}
	if strings.Contains(out, `action="/settings/notify/opt-in"`) {
		t.Errorf("notify section must not offer consent withdrawal when no consent is on file: %q", out)
	}
}

// A preference stored before its sender was removed stays visible but
// disabled, so saving the form neither offers the channel nor rewrites it.
func TestSettingsPage_NotifySection_StoredSMSPreferenceWithoutSender_StaysDisabled(t *testing.T) {
	view := components.SettingsView{
		Notify: components.NotifySettingsView{
			CSRFToken: "csrf-test",
			Preferences: []components.NotifyPreferenceRow{
				{EventType: "claim_expiring", Label: "Claim expiring soon", Channel: "sms"},
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `<option value="sms" disabled selected>`) {
		t.Errorf("a stored sms preference must render disabled and selected: %q", out)
	}
}
