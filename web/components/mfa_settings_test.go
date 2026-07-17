package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func TestSettingsPage_MFANotEnrolled_ShowsEnrollForm(t *testing.T) {
	view := components.SettingsView{
		MFA:       components.MFASettingsView{Status: components.MFAStatusNotEnrolled, CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `action="/settings/mfa/enroll"`) {
		t.Errorf("not-enrolled MFA section missing the enroll form: %q", out)
	}
	if strings.Contains(out, `action="/settings/mfa/confirm"`) {
		t.Errorf("not-enrolled MFA section must not show the confirm form: %q", out)
	}
	if strings.Contains(out, `action="/settings/mfa/disenroll"`) {
		t.Errorf("not-enrolled MFA section must not show the disenroll form: %q", out)
	}
}

// TestSettingsPage_MFAIntroCopy_DoesNotClaimSignInProtectionYet is the
// regression test for NES-134 CodeRabbit round 3 (finding 7): before
// NES-135 ships login enforcement, the page must never claim MFA already
// protects sign-in — every status must instead frame it as prepared/ready,
// not active protection.
func TestSettingsPage_MFAIntroCopy_DoesNotClaimSignInProtectionYet(t *testing.T) {
	for _, status := range []components.MFAEnrollmentStatus{
		components.MFAStatusNotEnrolled,
		components.MFAStatusPending,
		components.MFAStatusActive,
	} {
		view := components.SettingsView{
			MFA:       components.MFASettingsView{Status: status, CSRFToken: "csrf-test"},
			CSRFToken: "csrf-test",
		}
		out := renderString(t, components.SettingsPage(view))

		if strings.Contains(out, "Add a second step to sign-in") || strings.Contains(out, "Two-factor authentication is active.") {
			t.Errorf("status %q: MFA section copy must not claim sign-in is already protected: %q", status, out)
		}
		if !strings.Contains(out, "does not protect sign-in yet") {
			t.Errorf("status %q: MFA section intro must disclose it does not protect sign-in yet: %q", status, out)
		}
	}
}

func TestSettingsPage_MFAPending_ShowsQROnceAndConfirmForm(t *testing.T) {
	view := components.SettingsView{
		MFA: components.MFASettingsView{
			Status: components.MFAStatusPending,
			EnrollReveal: &components.MFAEnrollReveal{
				QRDataURI:         "data:image/png;base64,AAAA",
				ManualEntrySecret: "JBSWY3DPEHPK3PXP",
			},
			CSRFToken: "csrf-test",
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, "data:image/png;base64,AAAA") {
		t.Errorf("pending MFA section missing the QR reveal: %q", out)
	}
	if !strings.Contains(out, "JBSWY3DPEHPK3PXP") {
		t.Errorf("pending MFA section missing the manual-entry secret: %q", out)
	}
	if !strings.Contains(out, `action="/settings/mfa/confirm"`) {
		t.Errorf("pending MFA section missing the confirm form: %q", out)
	}
}

func TestSettingsPage_MFAPending_NoRevealAfterPageReload(t *testing.T) {
	// A GET after the enroll POST (no reveal supplied) must show the confirm
	// form WITHOUT re-displaying the secret — it cannot be reconstructed
	// server-side, and confirming does not require seeing it again.
	view := components.SettingsView{
		MFA:       components.MFASettingsView{Status: components.MFAStatusPending, CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if strings.Contains(out, "data:image/png;base64,") {
		t.Errorf("a later render without EnrollReveal must not show a QR code: %q", out)
	}
	if !strings.Contains(out, `action="/settings/mfa/confirm"`) {
		t.Errorf("pending MFA section (no reveal) still missing the confirm form: %q", out)
	}
}

func TestSettingsPage_MFAActive_ShowsRecoveryCodesOnceAndDisenrollForm(t *testing.T) {
	view := components.SettingsView{
		MFA: components.MFASettingsView{
			Status: components.MFAStatusActive,
			RecoveryCodesReveal: &components.MFARecoveryCodesReveal{
				Codes: []string{"AAAA-BBBB", "CCCC-DDDD", "EEEE-FFFF"},
			},
			CSRFToken: "csrf-test",
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	for _, code := range []string{"AAAA-BBBB", "CCCC-DDDD", "EEEE-FFFF"} {
		if !strings.Contains(out, code) {
			t.Errorf("active MFA section missing revealed recovery code %q: %q", code, out)
		}
	}
	if !strings.Contains(out, `action="/settings/mfa/disenroll"`) {
		t.Errorf("active MFA section missing the disenroll form: %q", out)
	}
	if !strings.Contains(out, `action="/settings/mfa/recovery-codes/regenerate"`) {
		t.Errorf("active MFA section missing the regenerate form: %q", out)
	}
	if strings.Contains(out, `action="/settings/mfa/enroll"`) {
		t.Errorf("active MFA section must not offer the enroll form: %q", out)
	}
	if strings.Contains(out, "Two-factor authentication is active.") {
		t.Errorf("active MFA section must not claim sign-in is already protected (NES-135 has not shipped): %q", out)
	}
	if !strings.Contains(out, "It will protect sign-in once we") {
		t.Errorf("active MFA section must frame itself as prepared, not already protecting sign-in: %q", out)
	}
}

func TestSettingsPage_MFAActive_NoRevealAfterPageReload(t *testing.T) {
	view := components.SettingsView{
		MFA:       components.MFASettingsView{Status: components.MFAStatusActive, CSRFToken: "csrf-test"},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if strings.Contains(out, "Save these recovery codes") {
		t.Errorf("a later render without RecoveryCodesReveal must not show the recovery codes panel: %q", out)
	}
}

func TestSettingsPage_MFAError_ShownInline(t *testing.T) {
	view := components.SettingsView{
		MFA:       components.MFASettingsView{Status: components.MFAStatusPending, CSRFToken: "csrf-test", Error: "That code could not be verified. Please try again."},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))
	if !strings.Contains(out, "That code could not be verified. Please try again.") {
		t.Errorf("MFA section missing the inline error message: %q", out)
	}
}

func TestSettingsPage_MFAOwner_ShowsAdminResetSection(t *testing.T) {
	view := components.SettingsView{
		MFA: components.MFASettingsView{
			Status:       components.MFAStatusNotEnrolled,
			CSRFToken:    "csrf-test",
			IsOwner:      true,
			OtherMembers: []components.MFAMemberOption{{ID: "member-1", DisplayName: "Kiddo"}},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))

	if !strings.Contains(out, `action="/settings/mfa/reset"`) {
		t.Errorf("owner view missing the admin reset form: %q", out)
	}
	if !strings.Contains(out, "Kiddo") {
		t.Errorf("owner view missing the family member option: %q", out)
	}
	if !strings.Contains(out, `name="owner_password"`) {
		t.Errorf("owner reset form missing the owner's own password field: %q", out)
	}
}

func TestSettingsPage_MFANonOwner_NoAdminResetSection(t *testing.T) {
	view := components.SettingsView{
		MFA:       components.MFASettingsView{Status: components.MFAStatusNotEnrolled, CSRFToken: "csrf-test", IsOwner: false},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))
	if strings.Contains(out, `action="/settings/mfa/reset"`) {
		t.Errorf("a non-owner must never see the admin reset form: %q", out)
	}
}

func TestSettingsPage_MFAOwner_NoOtherMembers_HidesAdminResetSection(t *testing.T) {
	// An owner with no other household members (a single-person household)
	// has nobody to reset, so the section should not render an empty picker.
	view := components.SettingsView{
		MFA:       components.MFASettingsView{Status: components.MFAStatusNotEnrolled, CSRFToken: "csrf-test", IsOwner: true},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))
	if strings.Contains(out, `action="/settings/mfa/reset"`) {
		t.Errorf("an owner with no other members must not see the admin reset form: %q", out)
	}
}

func TestSettingsPage_MFASectionRendersForEveryMember(t *testing.T) {
	// NES-134: the MFA section renders regardless of ShowKioskSection (a
	// child member never sees the kiosk section, but always sees their own
	// MFA section).
	view := components.SettingsView{
		ShowKioskSection: false,
		MFA:              components.MFASettingsView{Status: components.MFAStatusNotEnrolled, CSRFToken: "csrf-test"},
		CSRFToken:        "csrf-test",
	}
	out := renderString(t, components.SettingsPage(view))
	if !strings.Contains(out, "Two-factor authentication") {
		t.Errorf("MFA section must render even when the kiosk section is hidden: %q", out)
	}
}
