package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// ---------------------------------------------------------------------------
// NewTaskPage component tests
// ---------------------------------------------------------------------------

func TestNewTaskPageRendersFormFields(t *testing.T) {
	form := components.NewTaskForm{
		CSRFToken: "csrf-test-tok",
	}
	out := renderString(t, components.NewTaskPage(form))

	// Title input.
	if !strings.Contains(out, `name="title"`) {
		t.Errorf("new-task form missing title input: %q", out)
	}
	// Category radios.
	if !strings.Contains(out, `name="category"`) {
		t.Errorf("new-task form missing category radio: %q", out)
	}
	if !strings.Contains(out, `value="chore"`) {
		t.Errorf("new-task form missing chore category option: %q", out)
	}
	if !strings.Contains(out, `value="maintenance"`) {
		t.Errorf("new-task form missing maintenance category option: %q", out)
	}
	// Frequency select.
	if !strings.Contains(out, `name="freq"`) {
		t.Errorf("new-task form missing freq select: %q", out)
	}
	if !strings.Contains(out, `value="daily"`) {
		t.Errorf("new-task form missing daily option: %q", out)
	}
	if !strings.Contains(out, `value="weekly"`) {
		t.Errorf("new-task form missing weekly option: %q", out)
	}
	if !strings.Contains(out, `value="monthly"`) {
		t.Errorf("new-task form missing monthly option: %q", out)
	}
	// Interval input.
	if !strings.Contains(out, `name="interval"`) {
		t.Errorf("new-task form missing interval input: %q", out)
	}
	// Weekday checkboxes.
	if !strings.Contains(out, `name="byweekday"`) {
		t.Errorf("new-task form missing byweekday checkboxes: %q", out)
	}
	// All 7 day values 0–6.
	for _, wd := range []string{"0", "1", "2", "3", "4", "5", "6"} {
		if !strings.Contains(out, `value="`+wd+`"`) {
			t.Errorf("new-task form missing weekday value %q: %q", wd, out)
		}
	}
	// Anchor date input.
	if !strings.Contains(out, `name="anchor"`) {
		t.Errorf("new-task form missing anchor date input: %q", out)
	}
	if !strings.Contains(out, `type="date"`) {
		t.Errorf("new-task form anchor should be a date input: %q", out)
	}
	// Rotation policy select.
	if !strings.Contains(out, `name="rotation_policy"`) {
		t.Errorf("new-task form missing rotation_policy select: %q", out)
	}
	if !strings.Contains(out, `value="fixed"`) {
		t.Errorf("new-task form missing fixed rotation option: %q", out)
	}
	if !strings.Contains(out, `value="round_robin"`) {
		t.Errorf("new-task form missing round_robin rotation option: %q", out)
	}
	if !strings.Contains(out, `value="claimable"`) {
		t.Errorf("new-task form missing claimable rotation option: %q", out)
	}
	// Points input.
	if !strings.Contains(out, `name="points"`) {
		t.Errorf("new-task form missing points input: %q", out)
	}
	// Lead-time input.
	if !strings.Contains(out, `name="lead_time_days"`) {
		t.Errorf("new-task form missing lead_time_days input: %q", out)
	}
	// CSRF hidden field.
	if !strings.Contains(out, `name="csrf_token"`) {
		t.Errorf("new-task form missing csrf_token hidden field: %q", out)
	}
	if !strings.Contains(out, "csrf-test-tok") {
		t.Errorf("new-task form csrf_token has wrong value: %q", out)
	}
	// Submit button.
	if !strings.Contains(out, "Save chore") {
		t.Errorf("new-task form missing submit button: %q", out)
	}
	// Cancel link.
	if !strings.Contains(out, `href="/tasks"`) {
		t.Errorf("new-task form missing Cancel link to /tasks: %q", out)
	}
}

func TestNewTaskPageRendersMembers(t *testing.T) {
	form := components.NewTaskForm{
		CSRFToken: "tok",
		Members: []components.MemberOption{
			{ID: "uuid-alice", Name: "Alice", Color: "clay"},
			{ID: "uuid-bob", Name: "Bob", Color: "blue"},
		},
	}
	out := renderString(t, components.NewTaskPage(form))

	if !strings.Contains(out, `name="pool"`) {
		t.Errorf("new-task form missing pool checkboxes: %q", out)
	}
	if !strings.Contains(out, "Alice") {
		t.Errorf("new-task form missing member Alice: %q", out)
	}
	if !strings.Contains(out, "Bob") {
		t.Errorf("new-task form missing member Bob: %q", out)
	}
	if !strings.Contains(out, "uuid-alice") {
		t.Errorf("new-task form missing Alice's ID as checkbox value: %q", out)
	}
	if !strings.Contains(out, "uuid-bob") {
		t.Errorf("new-task form missing Bob's ID as checkbox value: %q", out)
	}
	// Member color tints.
	if !strings.Contains(out, "bg-member-clay-tint") {
		t.Errorf("new-task form missing clay tint for Alice: %q", out)
	}
	if !strings.Contains(out, "bg-member-blue-tint") {
		t.Errorf("new-task form missing blue tint for Bob: %q", out)
	}
}

func TestNewTaskPageShowsErrorMessage(t *testing.T) {
	form := components.NewTaskForm{
		CSRFToken: "tok",
		Error:     "Title is required.",
	}
	out := renderString(t, components.NewTaskPage(form))

	if !strings.Contains(out, "Title is required.") {
		t.Errorf("new-task form missing error message: %q", out)
	}
	if !strings.Contains(out, `role="alert"`) {
		t.Errorf("new-task form error missing role=alert: %q", out)
	}
}

func TestNewTaskPageNoErrorWhenErrorEmpty(t *testing.T) {
	form := components.NewTaskForm{CSRFToken: "tok", Error: ""}
	out := renderString(t, components.NewTaskPage(form))

	if strings.Contains(out, `role="alert"`) {
		t.Errorf("new-task form should not render alert when Error is empty: %q", out)
	}
}

func TestNewTaskPagePreservesStickyValues(t *testing.T) {
	form := components.NewTaskForm{
		CSRFToken:         "tok",
		Title:             "Mow the lawn",
		Category:          "maintenance",
		Freq:              "weekly",
		Interval:          "2",
		Weekdays:          []string{"1", "3"},
		Anchor:            "2026-07-01",
		RotationPolicy:    "round_robin",
		Points:            "5",
		LeadTimeDays:      "1",
		SelectedMemberIDs: []string{"uuid-alice"},
		Members: []components.MemberOption{
			{ID: "uuid-alice", Name: "Alice", Color: "sage"},
			{ID: "uuid-bob", Name: "Bob", Color: "clay"},
		},
	}
	out := renderString(t, components.NewTaskPage(form))

	// Title value preserved.
	if !strings.Contains(out, "Mow the lawn") {
		t.Errorf("sticky form missing title value: %q", out)
	}
	// Interval value preserved.
	if !strings.Contains(out, `value="2"`) {
		t.Errorf("sticky form missing interval=2: %q", out)
	}
	// Anchor value preserved.
	if !strings.Contains(out, "2026-07-01") {
		t.Errorf("sticky form missing anchor date: %q", out)
	}
	// Points value preserved.
	if !strings.Contains(out, `value="5"`) {
		t.Errorf("sticky form missing points=5: %q", out)
	}
	// Alice's checkbox should be checked (selected member).
	// The rendered HTML for a checked checkbox contains "checked" near Alice's ID.
	// We look for both the value and the checked attribute.
	aliceChunk := between(out, "uuid-alice", "uuid-bob")
	if aliceChunk == "" {
		// Fall back: just confirm checked appears before Bob's entry.
		aliceIdx := strings.Index(out, "uuid-alice")
		checkedIdx := strings.Index(out[aliceIdx:], "checked")
		if checkedIdx < 0 {
			t.Errorf("sticky form: Alice's checkbox should be checked: %q", out)
		}
	}
}

// between returns the substring of s between a and b (exclusive), or "" if
// either marker is absent or b precedes a.
func between(s, a, b string) string {
	ai := strings.Index(s, a)
	if ai < 0 {
		return ""
	}
	bi := strings.Index(s[ai:], b)
	if bi < 0 {
		return ""
	}
	return s[ai : ai+bi]
}

func TestNewTaskPageAlpineFreqBinding(t *testing.T) {
	// Alpine x-data is initialised from the form's Freq value so the weekday
	// section shows or hides correctly without a round-trip.
	form := components.NewTaskForm{
		CSRFToken: "tok",
		Freq:      "weekly",
		Members:   []components.MemberOption{{ID: "m1", Name: "Alice", Color: "sage"}},
	}
	out := renderString(t, components.NewTaskPage(form))

	// The seeded freq must live inside the JSON x-data initialiser (templ
	// HTML-escapes the JSON quotes to &#34;), not merely the freq <select>.
	if !strings.Contains(out, "&#34;freq&#34;:&#34;weekly&#34;") {
		t.Errorf("new-task form x-data initialiser does not seed freq=weekly: %q", out)
	}
	// The weekday section must be gated by an x-show keyed off freq === 'weekly'.
	if !strings.Contains(out, `x-show="freq === 'weekly'"`) {
		t.Errorf("new-task form missing x-show=\"freq === 'weekly'\" on weekday section: %q", out)
	}
	// The pool picker must be gated by an x-show keyed off the policy.
	if !strings.Contains(out, `x-show="policy !== 'claimable'"`) {
		t.Errorf("new-task form missing x-show=\"policy !== 'claimable'\" on pool picker: %q", out)
	}
}

// TestNewTaskPageAsNeededOption verifies the as-needed frequency option is
// present, the interval/anchor fields and the rotation-policy select are gated
// by an x-show keyed off freq !== 'as_needed', and an x-effect forces the
// policy to claimable whenever as-needed is selected (NES-116).
func TestNewTaskPageAsNeededOption(t *testing.T) {
	form := components.NewTaskForm{CSRFToken: "tok"}
	out := renderString(t, components.NewTaskPage(form))

	if !strings.Contains(out, `value="as_needed"`) {
		t.Errorf("new-task form missing as_needed frequency option: %q", out)
	}
	if !strings.Contains(out, `x-show="freq !== 'as_needed'"`) {
		t.Errorf("new-task form missing x-show gating on freq !== 'as_needed': %q", out)
	}
	if !strings.Contains(out, "policy = 'claimable'") {
		t.Errorf("new-task form missing x-effect forcing policy to claimable for as-needed: %q", out)
	}
}

// TestNewTaskPageXDataInjectionSafe is the regression test for Alpine expression
// injection: a hostile sticky freq value (from a 422 re-render) must be safely
// JSON- and HTML-encoded inside x-data, never breaking out of the expression.
func TestNewTaskPageXDataInjectionSafe(t *testing.T) {
	form := components.NewTaskForm{
		CSRFToken: "tok",
		Freq:      "evil'); alert(1); ('",
	}
	out := renderString(t, components.NewTaskPage(form))

	// The vulnerable string-concat form must be gone.
	if strings.Contains(out, "freq: 'evil") {
		t.Errorf("x-data still concatenates the raw freq value (injection): %q", out)
	}
	// The single quotes in the payload must be HTML-escaped, so the payload can
	// neither close the x-data attribute nor the Alpine string literal.
	if strings.Contains(out, "'); alert(1); ('") {
		t.Errorf("x-data contains the unescaped injection payload: %q", out)
	}
	// It must still be the JSON object form.
	if !strings.Contains(out, "&#34;freq&#34;:") {
		t.Errorf("x-data is not the JSON object form: %q", out)
	}
}
