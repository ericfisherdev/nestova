package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func TestSettingsPage_NoDeviceYet(t *testing.T) {
	view := components.SettingsView{ShowKioskSection: true, CSRFToken: "csrf-test"}
	out := renderString(t, components.SettingsPage(view))
	if !strings.Contains(out, "No kiosk device has been provisioned yet") {
		t.Errorf("settings page missing empty-state copy: %q", out)
	}
	if !strings.Contains(out, `action="/settings/kiosk/generate"`) {
		t.Errorf("settings page missing generate form: %q", out)
	}
	if !strings.Contains(out, `value="csrf-test"`) {
		t.Errorf("generate form missing CSRF token: %q", out)
	}
}

func TestSettingsPage_HidesKioskSectionForNonParent(t *testing.T) {
	view := components.SettingsView{ShowKioskSection: false, CSRFToken: "csrf-test"}
	out := renderString(t, components.SettingsPage(view))
	if strings.Contains(out, "Kiosk display") {
		t.Errorf("settings page must not show the kiosk section for a non-parent member: %q", out)
	}
	if strings.Contains(out, `action="/settings/kiosk/generate"`) {
		t.Errorf("settings page must not offer the kiosk generate form for a non-parent member: %q", out)
	}
}

func TestSettingsPage_ShowsActiveDeviceWithRevokeAction(t *testing.T) {
	view := components.SettingsView{
		ShowKioskSection: true,
		Kiosk: components.KioskSettingsView{
			Devices: []components.KioskDeviceView{
				{ID: "dev-1", Name: "Kitchen wall display", CreatedAtLabel: "Jul 16, 2026 3:04 PM", Active: true},
				{ID: "dev-0", Name: "Old tablet", RevokedAtLabel: "Jun 1, 2026 9:00 AM", Active: false},
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, "Kitchen wall display") || !strings.Contains(out, "Old tablet") {
		t.Errorf("settings page missing device names: %q", out)
	}
	if !strings.Contains(out, `action="/settings/kiosk/dev-1/revoke"`) {
		t.Errorf("active device missing revoke form: %q", out)
	}
	if strings.Contains(out, `action="/settings/kiosk/dev-0/revoke"`) {
		t.Errorf("revoked device must not offer a revoke action: %q", out)
	}
	if !strings.Contains(out, "Revoke") {
		t.Errorf("settings page missing Revoke button label: %q", out)
	}
}

func TestSettingsPage_RevealsNewActivationCodeOnce(t *testing.T) {
	view := components.SettingsView{
		ShowKioskSection: true,
		Kiosk: components.KioskSettingsView{
			NewToken: &components.KioskActivationReveal{
				Code:             "ABCD-EFGH-JK",
				ActivationURL:    "https://nestova.local/kiosk/activate?code=ABCD-EFGH-JK",
				ExpiresInMinutes: 15,
			},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))
	if !strings.Contains(out, "ABCD-EFGH-JK") {
		t.Errorf("settings page missing the revealed activation code: %q", out)
	}
	if !strings.Contains(out, "https://nestova.local/kiosk/activate?code=ABCD-EFGH-JK") {
		t.Errorf("settings page missing the activation URL: %q", out)
	}
	if !strings.Contains(out, "expires in 15 minutes") {
		t.Errorf("settings page missing the expiry warning copy: %q", out)
	}
	// The long-lived device token must never appear on this page — only the
	// short-lived code and the activation link that carries it.
	if strings.Contains(out, "token=") {
		t.Errorf("settings page must not embed a long-lived device token: %q", out)
	}
}
