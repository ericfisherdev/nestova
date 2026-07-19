package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func TestEmailNotificationHTML_RendersTitleAndBody(t *testing.T) {
	view := components.EmailNotificationView{Title: "Claim expiring soon", Body: "Complete it soon."}
	out := renderString(t, components.EmailNotificationHTML(view))

	if !strings.Contains(out, "<!doctype html>") {
		t.Errorf("missing doctype: %q", out)
	}
	if !strings.Contains(out, "Claim expiring soon") {
		t.Errorf("missing title: %q", out)
	}
	if !strings.Contains(out, "Complete it soon.") {
		t.Errorf("missing body: %q", out)
	}
	if !strings.Contains(out, "Nestova") {
		t.Errorf("missing brand name: %q", out)
	}
}

// TestEmailNotificationHTML_EmptyBody_OmitsBodyParagraph confirms an
// empty Body renders no body paragraph at all, rather than an empty one —
// mirrors SMSNotificationSender's own "no trailing colon for an empty
// Body" convention, applied here to markup instead of a string join.
func TestEmailNotificationHTML_EmptyBody_OmitsBodyParagraph(t *testing.T) {
	view := components.EmailNotificationView{Title: "Title only"}
	out := renderString(t, components.EmailNotificationHTML(view))

	if !strings.Contains(out, "Title only") {
		t.Errorf("missing title: %q", out)
	}
	if strings.Contains(out, `<p style="margin:0;font-size:15px`) {
		t.Errorf("empty Body should not render a body paragraph at all: %q", out)
	}
}

// TestEmailNotificationHTML_NoExternalAssets confirms the email template
// carries no external stylesheet or script reference — most email clients
// strip or refuse to load them, unlike the app's own Layout shell.
func TestEmailNotificationHTML_NoExternalAssets(t *testing.T) {
	out := renderString(t, components.EmailNotificationHTML(components.EmailNotificationView{Title: "T", Body: "B"}))

	for _, forbidden := range []string{`<link`, `<script`, "/static/css", "/static/js"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("email HTML must not reference external assets, found %q in: %q", forbidden, out)
		}
	}
}

func TestEmailNotificationPlainText_RendersTitleAndBody(t *testing.T) {
	view := components.EmailNotificationView{Title: "Claim expiring soon", Body: "Complete it soon."}
	out := components.EmailNotificationPlainText(view)

	if !strings.Contains(out, "Claim expiring soon") {
		t.Errorf("missing title: %q", out)
	}
	if !strings.Contains(out, "Complete it soon.") {
		t.Errorf("missing body: %q", out)
	}
	if !strings.Contains(out, "Nestova") {
		t.Errorf("missing brand name: %q", out)
	}
	if strings.Contains(out, "<") {
		t.Errorf("plain-text body must contain no markup: %q", out)
	}
}

func TestEmailNotificationPlainText_EmptyBody_OmitsBodySection(t *testing.T) {
	view := components.EmailNotificationView{Title: "Title only"}
	out := components.EmailNotificationPlainText(view)

	if !strings.Contains(out, "Title only") {
		t.Errorf("missing title: %q", out)
	}
	// Only the title, footer separator, and footer should appear — no
	// double blank line where an empty body would have gone.
	if strings.Count(out, "\n\n") != 1 {
		t.Errorf("plain text = %q, want exactly one blank-line separator (before the footer) when Body is empty", out)
	}
}
