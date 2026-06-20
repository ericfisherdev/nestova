package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

func subscriptionsView() components.SubscriptionsView {
	return components.SubscriptionsView{
		MonthlyCost: "68.33 USD",
		Subscriptions: []components.SubscriptionRow{{
			ID:               "sub-1",
			Name:             "Streaming Plus",
			AmountLabel:      "9.99 USD",
			CycleLabel:       "monthly",
			NextRenewal:      "Jul 20, 2026",
			PayerName:        "Alex",
			PayerColor:       "clay",
			Category:         "entertainment",
			AmountValue:      "9.99",
			CurrencyValue:    "USD",
			CycleValue:       "monthly",
			NextRenewalValue: "2026-07-20",
			PayerValue:       "mem-1",
			ReminderLeadDays: 3,
		}},
		Members:   []components.SubscriptionMemberOption{{ID: "mem-1", Name: "Alex", Color: "clay"}},
		Cycles:    []components.SubscriptionCycleOption{{Value: "monthly", Label: "monthly"}, {Value: "weekly", Label: "weekly"}},
		CSRFToken: "tok-123",
	}
}

func TestSubscriptionsPageRendersRollupAndForms(t *testing.T) {
	out := renderString(t, components.SubscriptionsPage(subscriptionsView()))

	if !strings.Contains(out, "68.33 USD") {
		t.Errorf("missing monthly rollup: %q", out)
	}
	if !strings.Contains(out, "Streaming Plus") || !strings.Contains(out, "9.99 USD") {
		t.Errorf("missing subscription row: %q", out)
	}
	// Add form posts to /subscriptions via HTMX with the CSRF token.
	if !strings.Contains(out, `hx-post="/subscriptions"`) {
		t.Errorf("missing add hx-post: %q", out)
	}
	if !strings.Contains(out, `value="tok-123"`) {
		t.Errorf("missing csrf token field: %q", out)
	}
	// Edit form targets the subscription id; deactivate has its own action.
	if !strings.Contains(out, `hx-post="/subscriptions/sub-1"`) {
		t.Errorf("missing edit hx-post: %q", out)
	}
	if !strings.Contains(out, `hx-post="/subscriptions/sub-1/deactivate"`) {
		t.Errorf("missing deactivate hx-post: %q", out)
	}
	// Payer chip carries the member color.
	if !strings.Contains(out, "bg-member-clay-tint") {
		t.Errorf("missing payer color chip: %q", out)
	}
	// Category is editable (a form field) and shown, so it is not lost on edit.
	if !strings.Contains(out, `name="category"`) {
		t.Errorf("missing category form field: %q", out)
	}
	if !strings.Contains(out, "entertainment") {
		t.Errorf("missing category display/value: %q", out)
	}
	// Edit-form field ids are namespaced by the subscription id so multiple rows
	// on the page never collide (add form uses the "add-" prefix).
	if !strings.Contains(out, `id="edit-sub-1-name"`) {
		t.Errorf("edit form ids not namespaced per row: %q", out)
	}
	if !strings.Contains(out, `id="add-name"`) {
		t.Errorf("missing add-form namespaced id: %q", out)
	}
}

func TestSubscriptionsPageEmptyState(t *testing.T) {
	view := subscriptionsView()
	view.Subscriptions = nil
	out := renderString(t, components.SubscriptionsPage(view))
	if !strings.Contains(out, "No active subscriptions") {
		t.Errorf("missing empty state: %q", out)
	}
	// The add form is still present.
	if !strings.Contains(out, `hx-post="/subscriptions"`) {
		t.Errorf("empty state missing add form: %q", out)
	}
}
