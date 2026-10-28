package components_test

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/ericfisherdev/nestova/web/components"
)

// These tests pin the NES-211 groundwork for a strict Content-Security-Policy
// (script-src 'self', no 'unsafe-inline', no 'unsafe-eval'): every page must be
// free of inline script and inline event handlers, and every Alpine expression
// must stay inside the subset the Alpine CSP build can evaluate. The policy
// itself is switched on in NES-212; nothing here asserts a header.

var (
	htmlComment        = regexp.MustCompile(`(?s)<!--.*?-->`)
	scriptOpenTag      = regexp.MustCompile(`<script\b[^>]*>`)
	inlineEventHandler = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	alpineAttribute    = regexp.MustCompile(`\s(?:x-(?:data|init|effect|on|bind|show|text|html|model|if|ref|transition|cloak|ignore|teleport|id|modelable)[a-z:.-]*|@[a-z][a-z.:-]*|:[a-z][a-z-]*)="([^"]*)"`)
	propertyAssignment = regexp.MustCompile(`[\w\]]\.\w+\s*=[^=]`)
)

// cspPages renders every standalone document and shell page that loads scripts,
// plus the fragments that carry inline Alpine or former inline handlers.
func cspPages(t *testing.T) map[string]string {
	t.Helper()
	proofRow := components.TaskRow{
		InstanceID: "csp-0000-0000-0001", Title: "Clean garage", Category: "chore",
		DueLabel: "Jun 20", Status: "pending", CSRFToken: "tok", PhotoPolicy: "before_after",
		ClaimExpiresAtISO: "2026-07-16T14:00:00Z",
	}
	return map[string]string{
		"Layout":          renderString(t, components.Layout(components.ShellProps{}, nil, templ.Raw(""))),
		"KioskLayout":     renderString(t, components.KioskLayout(components.KioskShellProps{Active: components.KioskTabChores}, templ.Raw(""))),
		"OfflinePage":     renderString(t, components.OfflinePage()),
		"SetupPage":       renderString(t, components.SetupPage(components.SetupForm{CSRFToken: "tok"})),
		"LoginPage":       renderString(t, components.LoginPage(components.LoginForm{CSRFToken: "tok"})),
		"LoginMFAPage":    renderString(t, components.LoginMFAPage(components.LoginMFAForm{CSRFToken: "tok"})),
		"AlbumViewerPage": renderString(t, components.AlbumViewerPage(components.AlbumViewerView{AlbumName: "A", RotationSeconds: 6, Slides: []components.SlideView{{RawURL: "/x"}}})),
		"NewTaskPage":     renderString(t, components.NewTaskPage(components.NewTaskForm{CSRFToken: "tok"})),
		"PhotosPage":      renderString(t, components.PhotosPage(photosView())),
		"MealsPage":       renderString(t, components.MealsPage(sampleMealsView())),
		"RewardFormPage":  renderString(t, components.RewardAdminFormPage(components.RewardAdminForm{CSRFToken: "tok"})),
		"TaskRowItem":     renderString(t, components.TaskRowItem(proofRow)),
	}
}

func TestPages_HaveNoInlineScriptOrEventHandlers(t *testing.T) {
	for name, page := range cspPages(t) {
		t.Run(name, func(t *testing.T) {
			markup := htmlComment.ReplaceAllString(page, "")
			for _, tag := range scriptOpenTag.FindAllString(markup, -1) {
				if !strings.Contains(tag, ` src="`) {
					t.Errorf("inline <script> block (needs script-src 'unsafe-inline'): %s", tag)
				}
			}
			if m := inlineEventHandler.FindString(markup); m != "" {
				t.Errorf("inline event-handler attribute %q (needs script-src 'unsafe-inline')", strings.TrimSpace(m))
			}
			if strings.Contains(markup, "<style") {
				t.Errorf("inline <style> element (needs style-src 'unsafe-inline')")
			}
			if strings.Contains(markup, ` style="`) {
				t.Errorf("inline style attribute (needs style-src 'unsafe-inline')")
			}
		})
	}
}

func TestPages_AlpineExpressionsStayWithinCSPSubset(t *testing.T) {
	for name, page := range cspPages(t) {
		t.Run(name, func(t *testing.T) {
			markup := htmlComment.ReplaceAllString(page, "")
			for _, match := range alpineAttribute.FindAllStringSubmatch(markup, -1) {
				expr := html.UnescapeString(match[1])
				switch {
				case strings.Contains(expr, "=>"):
					t.Errorf("arrow function in Alpine expression %q", expr)
				case strings.Contains(expr, ";"):
					t.Errorf("multiple statements in Alpine expression %q", expr)
				case strings.Contains(expr, "`"):
					t.Errorf("template literal in Alpine expression %q", expr)
				case strings.Contains(expr, "..."):
					t.Errorf("spread in Alpine expression %q", expr)
				case strings.HasPrefix(strings.TrimSpace(expr), "if "), strings.HasPrefix(strings.TrimSpace(expr), "if("):
					t.Errorf("statement block in Alpine expression %q", expr)
				case propertyAssignment.MatchString(expr):
					t.Errorf("property assignment in Alpine expression %q", expr)
				}
			}
		})
	}
}

func TestLayout_LoadsStaticReplacementsForInlineCode(t *testing.T) {
	out := cspPages(t)["Layout"]
	alpineIdx := strings.Index(out, `src="/static/js/alpine.min.js"`)
	if alpineIdx == -1 {
		t.Fatalf("layout missing alpine.min.js: %q", out)
	}
	// Alpine.data providers must register before Alpine starts (layout.templ's
	// script-order rule); sw-register.js and auto-submit.js are plain scripts.
	for _, script := range []string{"shell.js", "submit-once.js", "task-form.js", "claim-countdown.js", "upload-queue.js"} {
		idx := strings.Index(out, `src="/static/js/`+script+`" defer`)
		if idx == -1 {
			t.Errorf("layout missing deferred %s: %q", script, out)
		} else if idx > alpineIdx {
			t.Errorf("%s must load before alpine.min.js", script)
		}
	}
	for _, script := range []string{"sw-register.js", "auto-submit.js"} {
		if !strings.Contains(out, `src="/static/js/`+script+`" defer`) {
			t.Errorf("layout missing deferred %s: %q", script, out)
		}
	}
}

func TestHTMXConfig_DisablesInlineStylesAndEval(t *testing.T) {
	pages := cspPages(t)
	for _, name := range []string{"Layout", "KioskLayout"} {
		for _, want := range []string{`&#34;includeIndicatorStyles&#34;:false`, `&#34;allowEval&#34;:false`} {
			if !strings.Contains(pages[name], want) {
				t.Errorf("%s htmx-config missing %s", name, want)
			}
		}
	}
}

func TestOfflinePage_LoadsNoScript(t *testing.T) {
	out := cspPages(t)["OfflinePage"]
	if strings.Contains(out, "<script") {
		t.Errorf("offline page must stay script-free so it works from the service worker cache: %q", out)
	}
	if !strings.Contains(out, `<a href=""`) {
		t.Errorf("offline page \"Try again\" should be a link to the current URL: %q", out)
	}
}

func TestProofPhotoInput_AutoSubmitsViaDataAttribute(t *testing.T) {
	out := cspPages(t)["TaskRowItem"]
	if !strings.Contains(out, "data-autosubmit") {
		t.Errorf("proof-photo input missing data-autosubmit: %q", out)
	}
}

func TestSetupPage_WiresSetupFormComponent(t *testing.T) {
	out := cspPages(t)["SetupPage"]
	if !strings.Contains(out, `x-data="setupForm"`) || !strings.Contains(out, "data-state=") {
		t.Errorf("setup form missing setupForm component or data-state: %q", out)
	}
	formIdx := strings.Index(out, `src="/static/js/setup-form.js"`)
	alpineIdx := strings.Index(out, `src="/static/js/alpine.min.js"`)
	if formIdx == -1 || alpineIdx == -1 || formIdx > alpineIdx {
		t.Errorf("setup-form.js must load before alpine.min.js on the standalone setup page: %q", out)
	}
}
