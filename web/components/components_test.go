package components_test

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/ericfisherdev/nestova/web/components"
)

func renderString(t *testing.T, c templ.Component) string {
	t.Helper()
	var sb strings.Builder
	if err := c.Render(context.Background(), &sb); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return sb.String()
}

func TestButtonVariants(t *testing.T) {
	primary := renderString(t, components.Button("Create", components.ButtonPrimary, ""))
	if !strings.Contains(primary, "bg-sage") || !strings.Contains(primary, "Create") {
		t.Errorf("primary button missing sage class or label: %q", primary)
	}
	if !strings.Contains(primary, `type="button"`) {
		t.Errorf("primary button should default type to button: %q", primary)
	}

	secondary := renderString(t, components.Button("Cancel", components.ButtonSecondary, "submit"))
	if !strings.Contains(secondary, "border") || !strings.Contains(secondary, "bg-surface") {
		t.Errorf("secondary button missing bordered/surface classes: %q", secondary)
	}
	if !strings.Contains(secondary, `type="submit"`) {
		t.Errorf("secondary button should honor an explicit type: %q", secondary)
	}
}

func TestCardRendersTitleAndChildren(t *testing.T) {
	// Inject the card body via templ's children mechanism.
	ctx := templ.WithChildren(context.Background(), templ.Raw("<p>body</p>"))
	var sb strings.Builder
	if err := components.Card("Welcome").Render(ctx, &sb); err != nil {
		t.Fatalf("Card.Render: %v", err)
	}
	got := sb.String()
	if !strings.Contains(got, "Welcome") || !strings.Contains(got, "<p>body</p>") {
		t.Errorf("card missing title or children: %q", got)
	}
	if !strings.Contains(got, "rounded-card") {
		t.Errorf("card missing rounded-card token class: %q", got)
	}
}

func TestNavPillActive(t *testing.T) {
	active := renderString(t, components.NavPill("Calendar", "/calendar", true))
	if !strings.Contains(active, "bg-sage-tint") || !strings.Contains(active, `aria-current="page"`) {
		t.Errorf("active nav pill missing tint or aria-current: %q", active)
	}
	inactive := renderString(t, components.NavPill("Chores", "/chores", false))
	if strings.Contains(inactive, `aria-current`) {
		t.Errorf("inactive nav pill should not be aria-current: %q", inactive)
	}
}

func TestLayoutRendersShell(t *testing.T) {
	props := components.ShellProps{
		Members:   []components.MemberView{{Name: "Maya", Initials: "M", Color: "sage"}},
		CSRFToken: "csrf-test-token",
	}
	nav := []components.NavItem{
		{Label: "Calendar", Href: "/calendar", Active: true},
		{Label: "Chores", Href: "/chores"},
	}
	out := renderString(t, components.Layout(props, nav, templ.Raw(`<p id="body">hi</p>`)))

	for _, want := range []string{
		"<!doctype html>",
		`href="/static/css/app.css"`,           // styled
		`src="/static/js/htmx.min.js"`,         // htmx wired
		`src="/static/js/alpine.min.js"`,       // alpine wired
		`src="/static/js/gsap.min.js"`,         // gsap loaded for the motion pass
		`src="/static/js/dashboard-motion.js"`, // dashboard entrance/transition polish
		`id="sidebar"`,                         // fixed sidebar
		`aria-label="Primary"`,                 // nav landmark
		"Skip to content",                      // a11y skip link
		`aria-controls="sidebar"`,              // drawer toggle wiring
		`aria-current="page"`,                  // active nav pill
		"Maya",                                 // family list
		`id="body"`,                            // content slot rendered
		`action="/logout"`,                     // logout form
		`value="csrf-test-token"`,              // csrf token threaded into the logout form
	} {
		if !strings.Contains(out, want) {
			t.Errorf("layout missing %q", want)
		}
	}
}

func TestDashboardRendersCards(t *testing.T) {
	out := renderString(t, components.Dashboard(components.TradeSections{}))
	// Note: templ HTML-escapes "&", so the meals card asserts the escaped title.
	for _, want := range []string{"Dashboard", "Calendar", "Chores", "Meals &amp; Recipes", "Groceries", "Photos", "Subscriptions"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing card/heading %q", want)
		}
	}
}

func TestMemberAvatar(t *testing.T) {
	avatar := renderString(t, components.MemberAvatar(components.MemberView{Name: "Maya", Initials: "M", Color: "clay"}))
	if !strings.Contains(avatar, "bg-member-clay-tint") || !strings.Contains(avatar, "text-member-clay-fg") {
		t.Errorf("avatar missing clay member-color classes: %q", avatar)
	}
	if !strings.Contains(avatar, "M") || !strings.Contains(avatar, `aria-label="Maya"`) {
		t.Errorf("avatar missing initials or accessible name: %q", avatar)
	}

	// An unknown color falls back to a valid (safelisted) palette key.
	fallback := renderString(t, components.MemberAvatar(components.MemberView{Name: "X", Initials: "X", Color: "chartreuse"}))
	if !strings.Contains(fallback, "bg-member-sage-tint") {
		t.Errorf("unknown color should fall back to sage: %q", fallback)
	}
}

func TestLayoutLoadsMotionScriptsDeferred(t *testing.T) {
	props := components.ShellProps{CSRFToken: "t"}
	out := renderString(t, components.Layout(props, nil, templ.Raw("<p>x</p>")))
	// The GSAP + motion scripts load deferred so they never block first paint.
	if !strings.Contains(out, `src="/static/js/gsap.min.js" defer`) || !strings.Contains(out, `src="/static/js/dashboard-motion.js" defer`) {
		t.Errorf("motion scripts should be deferred: %q", out)
	}
}

// TestLayout_RendersPWAHead guards the manifest/theme-color/apple-touch-icon
// tags added in NES-151. Without the manifest link Chrome never offers to
// install the app, and the failure is invisible outside a real device.
func TestLayout_RendersPWAHead(t *testing.T) {
	out := renderString(t, components.Layout(components.ShellProps{}, nil, templ.Raw("")))

	for _, want := range []string{
		`rel="manifest"`,
		`href="/static/manifest.webmanifest"`,
		`name="theme-color"`,
		`content="#6f8c6a"`, // --color-sage, the Hearth status-bar tint
		`rel="apple-touch-icon"`,
		`href="/static/icons/icon-180.png"`, // iOS ignores the manifest icons
	} {
		if !strings.Contains(out, want) {
			t.Errorf("layout head missing %s: %q", want, out)
		}
	}
}
