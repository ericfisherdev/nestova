package components_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/web/components"
)

// ---------------------------------------------------------------------------
// TradeProposalCard
// ---------------------------------------------------------------------------

// TestTradeProposalCard_ResponderActions verifies that a card where the
// viewer is the responder renders Accept and Decline forms posting to the
// right URLs, with the row's own container as the hx-target (NES-122).
func TestTradeProposalCard_ResponderActions(t *testing.T) {
	card := components.TradeCard{
		TradeID:       "trade-1",
		ProposerName:  "Alice",
		ResponderName: "Bob",
		Offered:       components.TradeChoreSummary{Title: "Vacuum", Points: 10},
		Requested:     components.TradeChoreSummary{Title: "Dishes", Points: 5},
		IsResponder:   true,
		CSRFToken:     "tok",
	}
	out := renderString(t, components.TradeProposalCard(card))

	if !strings.Contains(out, `id="trade-trade-1"`) {
		t.Errorf("card missing stable #trade-{id} container: %q", out)
	}
	if !strings.Contains(out, `hx-post="/trades/trade-1/accept"`) {
		t.Errorf("card missing accept hx-post: %q", out)
	}
	if !strings.Contains(out, `hx-post="/trades/trade-1/decline"`) {
		t.Errorf("card missing decline hx-post: %q", out)
	}
	if !strings.Contains(out, `hx-target="#trade-trade-1"`) {
		t.Errorf("card actions missing hx-target on their own container: %q", out)
	}
	if strings.Contains(out, "/cancel") {
		t.Errorf("responder card must not render a cancel action: %q", out)
	}
	for _, want := range []string{"Alice", "Bob", "Vacuum", "10", "Dishes", "5"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q: %q", want, out)
		}
	}
}

// TestTradeProposalCard_ProposerActions verifies that a card where the
// viewer is the proposer renders only a Cancel action.
func TestTradeProposalCard_ProposerActions(t *testing.T) {
	card := components.TradeCard{
		TradeID:      "trade-2",
		ProposerName: "Alice",
		IsProposer:   true,
		CSRFToken:    "tok",
	}
	out := renderString(t, components.TradeProposalCard(card))

	if !strings.Contains(out, `hx-post="/trades/trade-2/cancel"`) {
		t.Errorf("card missing cancel hx-post: %q", out)
	}
	if strings.Contains(out, "/accept") || strings.Contains(out, "/decline") {
		t.Errorf("proposer card must not render accept/decline actions: %q", out)
	}
}

// TestTradeProposalCard_NeitherRole verifies that a card for a viewer who is
// neither the proposer nor responder renders no action forms — defensive,
// should never occur given how DashboardSections/buildTradeCard populate the
// flags, but the template must not assume one of the two is always true.
func TestTradeProposalCard_NeitherRole(t *testing.T) {
	card := components.TradeCard{TradeID: "trade-3"}
	out := renderString(t, components.TradeProposalCard(card))

	for _, unwanted := range []string{"/accept", "/decline", "/cancel"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("card for uninvolved viewer must render no actions, found %q: %q", unwanted, out)
		}
	}
}

// ---------------------------------------------------------------------------
// TradeProposalsSection
// ---------------------------------------------------------------------------

// TestTradeProposalsSection_EmptyRendersNothing verifies that a member with
// no pending trades gets no dashboard section at all.
func TestTradeProposalsSection_EmptyRendersNothing(t *testing.T) {
	out := renderString(t, components.TradeProposalsSection(components.TradeSections{}))
	if strings.TrimSpace(out) != "" {
		t.Errorf("empty TradeSections should render nothing, got %q", out)
	}
}

// TestTradeProposalsSection_ShowsBothGroupsAndHistoryLink verifies that both
// "awaiting you" and "awaiting them" groups render under distinct headings,
// and that the parent-only history link appears when CanViewHistory is true.
func TestTradeProposalsSection_ShowsBothGroupsAndHistoryLink(t *testing.T) {
	sections := components.TradeSections{
		AwaitingYou:    []components.TradeCard{{TradeID: "t1", IsResponder: true}},
		AwaitingThem:   []components.TradeCard{{TradeID: "t2", IsProposer: true}},
		CanViewHistory: true,
	}
	out := renderString(t, components.TradeProposalsSection(sections))

	if !strings.Contains(out, "Trade requests for you") {
		t.Errorf("section missing 'awaiting you' heading: %q", out)
	}
	if !strings.Contains(out, "Your pending trade proposals") {
		t.Errorf("section missing 'awaiting them' heading: %q", out)
	}
	if !strings.Contains(out, `href="/trades/history"`) {
		t.Errorf("section missing trade history link: %q", out)
	}
	if !strings.Contains(out, `id="trade-t1"`) || !strings.Contains(out, `id="trade-t2"`) {
		t.Errorf("section missing one of the rendered cards: %q", out)
	}
}

// TestTradeProposalsSection_HidesHistoryLinkForNonParent verifies the
// dashboard never shows the history link to a child member.
func TestTradeProposalsSection_HidesHistoryLinkForNonParent(t *testing.T) {
	sections := components.TradeSections{
		AwaitingYou:    []components.TradeCard{{TradeID: "t1", IsResponder: true}},
		CanViewHistory: false,
	}
	out := renderString(t, components.TradeProposalsSection(sections))
	if strings.Contains(out, "/trades/history") {
		t.Errorf("section must not link to trade history for a non-parent: %q", out)
	}
}

// TestTradeProposalsSection_ShowsHistoryLinkWithNoPendingTrades verifies
// that a parent with zero pending trades still sees the history link — the
// section's render guard must not hide the link's only entry point just
// because there is nothing currently pending.
func TestTradeProposalsSection_ShowsHistoryLinkWithNoPendingTrades(t *testing.T) {
	out := renderString(t, components.TradeProposalsSection(components.TradeSections{CanViewHistory: true}))
	if !strings.Contains(out, `href="/trades/history"`) {
		t.Errorf("section missing trade history link for a parent with no pending trades: %q", out)
	}
}

// ---------------------------------------------------------------------------
// ProposeTradePage
// ---------------------------------------------------------------------------

// TestProposeTradePage_ShowsOfferedAndCandidates verifies the picker renders
// the offered chore's summary and a radio option per candidate.
func TestProposeTradePage_ShowsOfferedAndCandidates(t *testing.T) {
	form := components.ProposeTradeForm{
		OfferedInstanceID: "offered-1",
		OfferedTitle:      "Mow the lawn",
		OfferedPoints:     15,
		Candidates: []components.TradeCandidate{
			{InstanceID: "cand-1", Title: "Dishes", Points: 5, AssigneeName: "Bob"},
			{InstanceID: "cand-2", Title: "Laundry", Points: 8, AssigneeName: "Charlie"},
		},
		CSRFToken: "tok",
	}
	out := renderString(t, components.ProposeTradePage(form))

	if !strings.Contains(out, "Mow the lawn") || !strings.Contains(out, "15") {
		t.Errorf("picker missing offered chore summary: %q", out)
	}
	if !strings.Contains(out, `value="offered-1"`) {
		t.Errorf("picker missing hidden offered_instance_id field: %q", out)
	}
	for _, want := range []string{"cand-1", "Dishes", "Bob", "cand-2", "Laundry", "Charlie"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker missing candidate content %q: %q", want, out)
		}
	}
	if !strings.Contains(out, `action="/trades"`) {
		t.Errorf("picker form missing POST /trades action: %q", out)
	}
}

// TestProposeTradePage_NoCandidates verifies the empty-candidates state
// renders a message instead of an unusable empty radio list.
func TestProposeTradePage_NoCandidates(t *testing.T) {
	form := components.ProposeTradeForm{OfferedInstanceID: "offered-1", OfferedTitle: "Mow the lawn"}
	out := renderString(t, components.ProposeTradePage(form))

	if !strings.Contains(out, "No sibling chores are available") {
		t.Errorf("picker missing no-candidates message: %q", out)
	}
	if strings.Contains(out, `action="/trades"`) {
		t.Errorf("picker must not render a submit form with no candidates: %q", out)
	}
}

// TestProposeTradePage_ShowsError verifies a validation error banner renders
// when Error is set.
func TestProposeTradePage_ShowsError(t *testing.T) {
	form := components.ProposeTradeForm{Error: "One of the selected chores is no longer tradeable."}
	out := renderString(t, components.ProposeTradePage(form))

	if !strings.Contains(out, "One of the selected chores is no longer tradeable.") {
		t.Errorf("picker missing error banner: %q", out)
	}
}

// ---------------------------------------------------------------------------
// TradeHistoryPageComponent
// ---------------------------------------------------------------------------

// TestTradeHistoryPageComponent_Empty verifies the empty state.
func TestTradeHistoryPageComponent_Empty(t *testing.T) {
	out := renderString(t, components.TradeHistoryPageComponent(components.TradeHistoryPage{}))
	if !strings.Contains(out, "No trades have been proposed yet.") {
		t.Errorf("history page missing empty-state message: %q", out)
	}
}

// TestTradeHistoryPageComponent_ShowsRows verifies each trade renders with
// its parties, chores, status badge, and timestamps.
func TestTradeHistoryPageComponent_ShowsRows(t *testing.T) {
	page := components.TradeHistoryPage{
		Trades: []components.TradeCard{
			{
				ProposerName:  "Alice",
				ResponderName: "Bob",
				Offered:       components.TradeChoreSummary{Title: "Vacuum", Points: 10},
				Requested:     components.TradeChoreSummary{Title: "Dishes", Points: 5},
				Status:        "accepted",
				CreatedAt:     "Jun 1",
				ResolvedAt:    "Jun 2",
			},
		},
	}
	out := renderString(t, components.TradeHistoryPageComponent(page))

	for _, want := range []string{"Alice", "Bob", "Vacuum", "Dishes", "Accepted", "Jun 1", "Jun 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("history row missing %q: %q", want, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Accept response / card removal (HTMX wiring, NES-122)
// ---------------------------------------------------------------------------

// TestTradeCardRemoved_RendersHiddenPlaceholder verifies the Decline/Cancel
// response shape: an empty, hidden placeholder replacing the resolved card.
func TestTradeCardRemoved_RendersHiddenPlaceholder(t *testing.T) {
	out := renderString(t, components.TradeCardRemoved("trade-9"))
	if !strings.Contains(out, `id="trade-trade-9"`) {
		t.Errorf("card-removed placeholder missing the trade's own id: %q", out)
	}
	if !strings.Contains(out, "hidden") {
		t.Errorf("card-removed placeholder should be visually hidden: %q", out)
	}
}

// TestTradeAcceptResponse_IncludesCardRemovalAndOOBGroupsRefresh verifies
// that Accept's HTMX response carries both the resolved card's own removal
// and an out-of-band #task-groups refresh (NES-118/NES-122) — the attribute
// wiring AC2 requires.
func TestTradeAcceptResponse_IncludesCardRemovalAndOOBGroupsRefresh(t *testing.T) {
	groups := []components.TaskGroup{
		{Label: "Alice", Rows: []components.TaskRow{{InstanceID: "row-1", Title: "Dishes", Status: "pending"}}},
	}
	out := renderString(t, components.TradeAcceptResponse("trade-9", groups))

	if !strings.Contains(out, `id="trade-trade-9"`) {
		t.Errorf("accept response missing the resolved card's own removal: %q", out)
	}
	if !strings.Contains(out, `id="task-groups"`) {
		t.Errorf("accept response missing the #task-groups container: %q", out)
	}
	if !strings.Contains(out, `hx-swap-oob="true"`) {
		t.Errorf("accept response missing hx-swap-oob=\"true\" on #task-groups: %q", out)
	}
	if !strings.Contains(out, "Dishes") {
		t.Errorf("accept response missing refreshed group content: %q", out)
	}
}
