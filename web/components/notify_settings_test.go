package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func TestSettingsPage_NotifySection_NoPhone_HidesOptInForm(t *testing.T) {
	view := components.SettingsView{
		Notify:    components.NotifySettingsView{Phone: "", CSRFToken: "csrf-test"},
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
		Notify:    components.NotifySettingsView{Phone: "+15551234567", CSRFToken: "csrf-test"},
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
			Phone:     "+15551234567",
			OptedIn:   true,
			CSRFToken: "csrf-test",
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
			Phone:     "+15551234567",
			OptedIn:   false,
			CSRFToken: "csrf-test",
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
