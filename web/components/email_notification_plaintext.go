package components

import "strings"

// EmailNotificationPlainText renders view as the plain-text alternative
// part for a notification email (NES-141). Every email
// notify/adapter.EmailNotificationSender sends carries both this and
// EmailNotificationHTML's rendered output (see domain.EmailSender's own
// doc for why neither part is optional): a client that cannot or chooses
// not to render HTML still gets fully readable content from this string.
//
// A plain Go string builder, not a second .templ component: templ's
// output is HTML-context-aware (attribute/text escaping rules that do
// not apply to a plain-text body), so using it here would be a misuse of
// the tool for no benefit — the text part has no markup to escape into.
func EmailNotificationPlainText(view EmailNotificationView) string {
	var b strings.Builder
	b.WriteString(view.Title)
	if view.Body != "" {
		b.WriteString("\n\n")
		b.WriteString(view.Body)
	}
	b.WriteString("\n\n---\nSent by Nestova. Manage which notifications go to email in your household's settings.")
	return b.String()
}
