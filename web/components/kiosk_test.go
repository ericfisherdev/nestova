package components_test

import (
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/ericfisherdev/nestova/web/components"
)

// ---------------------------------------------------------------------------
// KioskLayout — shell, tab bar, screensaver (NES-128 AC3, AC4)
// ---------------------------------------------------------------------------

// extractTag returns the single HTML element (from its opening "<a" to its
// closing "</a>") that carries data-testid=testID, so a test can assert on
// classes/attributes scoped to exactly that element rather than the whole
// page (where a substring match could accidentally be satisfied by a
// different element entirely).
func extractTag(html, testID string) string {
	needle := `data-testid="` + testID + `"`
	idx := strings.Index(html, needle)
	if idx < 0 {
		return ""
	}
	start := strings.LastIndex(html[:idx], "<a")
	if start < 0 {
		return ""
	}
	end := strings.Index(html[idx:], "</a>")
	if end < 0 {
		return ""
	}
	return html[start : idx+end+len("</a>")]
}

func TestKioskLayout_RendersTouchSizedTabBarForEveryTab(t *testing.T) {
	props := components.KioskShellProps{Active: components.KioskTabShopping}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))

	for _, tab := range []string{"kiosk-tab-chores", "kiosk-tab-calendar", "kiosk-tab-meals", "kiosk-tab-shopping", "kiosk-tab-photos"} {
		if !strings.Contains(out, `data-testid="`+tab+`"`) {
			t.Errorf("tab bar missing %s", tab)
		}
	}
	// AC3: every tab target is at least 48x48px (min-h-[64px], full-width flex-1).
	if !strings.Contains(out, "min-h-[64px]") {
		t.Errorf("tab bar items are not sized for touch: %q", out)
	}

	// The active (shopping) tab is distinguished by a persistent tint and
	// aria-current, scoped to its own element — not merely present somewhere
	// on the page.
	active := extractTag(out, "kiosk-tab-shopping")
	if active == "" {
		t.Fatalf("could not locate the shopping tab element in: %q", out)
	}
	if !strings.Contains(active, "bg-sage-tint") || !strings.Contains(active, "text-sage-darker") {
		t.Errorf("active shopping tab missing its tint styling: %q", active)
	}
	if !strings.Contains(active, `aria-current="page"`) {
		t.Errorf("active shopping tab missing aria-current: %q", active)
	}

	// An inactive tab must carry neither the tint styling nor aria-current.
	inactive := extractTag(out, "kiosk-tab-chores")
	if inactive == "" {
		t.Fatalf("could not locate the chores tab element in: %q", out)
	}
	if strings.Contains(inactive, "bg-sage-tint") || strings.Contains(inactive, "text-sage-darker") {
		t.Errorf("inactive chores tab must not carry the active tint styling: %q", inactive)
	}
	if strings.Contains(inactive, "aria-current") {
		t.Errorf("inactive chores tab must not carry aria-current: %q", inactive)
	}
}

func TestKioskLayout_ActiveTabMarksAriaCurrent(t *testing.T) {
	props := components.KioskShellProps{Active: components.KioskTabChores}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))

	// aria-current="page" must appear exactly once (only the active tab).
	if strings.Count(out, `aria-current="page"`) != 1 {
		t.Errorf("expected exactly one aria-current=page, got %d: %q", strings.Count(out, `aria-current="page"`), out)
	}
}

func TestKioskLayout_NoHoverDependentAffordance(t *testing.T) {
	// AC3: no hover-dependent UI. The kiosk shell (tab bar + screensaver
	// chrome, excluding page content which is tested per-tab) must never rely
	// on a :hover-only Tailwind class.
	props := components.KioskShellProps{Active: components.KioskTabChores}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))
	if strings.Contains(out, "hover:") {
		t.Errorf("kiosk shell must not use hover-dependent classes: %q", out)
	}
}

func TestKioskLayout_IdleScreensaverWiring(t *testing.T) {
	// AC4: the idle timer is Alpine-driven on the shell root, and the
	// screensaver overlay is hidden until 'screensaverActive' toggles —
	// dismissing it never navigates (there is no page swap, just x-show).
	props := components.KioskShellProps{Active: components.KioskTabChores}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))

	for _, want := range []string{
		`x-data="kioskIdle"`,
		`data-idle-timeout-ms="120000"`,
		`data-testid="kiosk-screensaver"`,
		`x-show="screensaverActive"`,
		`x-on:click="dismiss()"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("kiosk shell missing idle/screensaver wiring %q in: %q", want, out)
		}
	}
}

func TestKioskLayout_ScriptOrder(t *testing.T) {
	// kiosk-idle.js registers its own Alpine.data provider and must load and
	// run before alpine.min.js (layout.templ's documented script-order rule).
	props := components.KioskShellProps{Active: components.KioskTabChores}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))

	idleIdx := strings.Index(out, "kiosk-idle.js")
	alpineIdx := strings.Index(out, "alpine.min.js")
	if idleIdx == -1 || alpineIdx == -1 {
		t.Fatalf("missing expected scripts in shell: %q", out)
	}
	if idleIdx > alpineIdx {
		t.Errorf("kiosk-idle.js must load before alpine.min.js, got order: %q", out)
	}
}

func TestKioskScreensaver_RendersSlidesWhenPresent(t *testing.T) {
	props := components.KioskShellProps{
		Active: components.KioskTabPhotos,
		Screensaver: components.KioskScreensaverView{
			RotationSeconds: 10,
			Slides:          []components.SlideView{{RawURL: "/kiosk/photos/p1/raw", Caption: "Beach day"}},
		},
	}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))
	if !strings.Contains(out, `data-rotation-seconds="10"`) {
		t.Errorf("screensaver missing rotation seconds: %q", out)
	}
	if !strings.Contains(out, "/kiosk/photos/p1/raw") {
		t.Errorf("screensaver missing slide image: %q", out)
	}
}

func TestKioskScreensaver_EmptyStateWhenNoAlbum(t *testing.T) {
	props := components.KioskShellProps{Active: components.KioskTabPhotos}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))
	if !strings.Contains(out, "No photos yet") && !strings.Contains(out, "Add photos") {
		t.Errorf("empty screensaver missing placeholder copy: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Chores tab — read-only (no complete/skip/claim actions)
// ---------------------------------------------------------------------------

func TestKioskChoresPage_RendersReadOnlyRows(t *testing.T) {
	view := components.KioskChoresView{
		Chores: []components.KioskChoreView{
			{Title: "Wash dishes", Category: "chore", DueLabel: "Today", AssigneeName: "Maya", AssigneeInitials: "M", AssigneeColor: "sage"},
			{Title: "Mow lawn", Category: "maintenance", DueLabel: "Tomorrow"},
		},
	}
	out := renderString(t, components.KioskChoresPage(view))

	for _, want := range []string{"Wash dishes", "Mow lawn", "Today", "Tomorrow", "Maya", "Anyone"} {
		if !strings.Contains(out, want) {
			t.Errorf("chores page missing %q: %q", want, out)
		}
	}
	// Member-attributed mutations must never appear on the kiosk (AC5's rule
	// applies household-wide, not just to shopping).
	if strings.Contains(out, "<form") {
		t.Errorf("chores tab must be fully read-only (no forms): %q", out)
	}
	for _, forbidden := range []string{"Complete", "Skip", "Claim"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("chores tab must not expose the %q action: %q", forbidden, out)
		}
	}
}

func TestKioskChoresPage_EmptyState(t *testing.T) {
	out := renderString(t, components.KioskChoresPage(components.KioskChoresView{}))
	if !strings.Contains(out, "caught up") {
		t.Errorf("empty chores page missing empty-state copy: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Calendar tab
// ---------------------------------------------------------------------------

func TestKioskCalendarPage_RendersItems(t *testing.T) {
	view := components.KioskCalendarView{
		RangeLabel: "Jul 16 – Jul 23, 2026",
		Items: []components.CalendarItemView{
			{Kind: "task", KindLabel: "Chore", Title: "Take out trash", When: "Today"},
		},
	}
	out := renderString(t, components.KioskCalendarPage(view))
	for _, want := range []string{"Jul 16", "Take out trash", "Chore"} {
		if !strings.Contains(out, want) {
			t.Errorf("calendar page missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "<form") {
		t.Errorf("calendar tab must be read-only: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Meals tab
// ---------------------------------------------------------------------------

func TestKioskMealsPage_RendersDaysAndSlots(t *testing.T) {
	view := components.KioskMealsView{
		WeekLabel: "Jul 16 – Jul 22, 2026",
		Days: []components.KioskMealDayView{
			{DateLabel: "Monday, Jul 16", Slots: []components.KioskMealSlotView{
				{MealLabel: "Dinner", RecipeTitle: "Tacos"},
			}},
		},
	}
	out := renderString(t, components.KioskMealsPage(view))
	for _, want := range []string{"Monday, Jul 16", "Dinner", "Tacos"} {
		if !strings.Contains(out, want) {
			t.Errorf("meals page missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "<form") {
		t.Errorf("meals tab must be read-only: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Shopping tab — the one allowed mutation (AC5)
// ---------------------------------------------------------------------------

func TestKioskShoppingPage_NeededItemGetsInCartActionOnly(t *testing.T) {
	view := components.KioskShoppingView{
		Needed: []components.ShoppingItemView{
			{ID: "item-1", Name: "Milk", QuantityLabel: "1 count", SourceLabel: "Manual", Status: "needed"},
		},
		CSRFToken: "csrf-test",
	}
	out := renderString(t, components.KioskShoppingPage(view))

	if !strings.Contains(out, "Milk") {
		t.Errorf("shopping page missing item name: %q", out)
	}
	if !strings.Contains(out, `action="/kiosk/shopping/item-1/in-cart"`) {
		t.Errorf("needed item missing kiosk-scoped in-cart form action: %q", out)
	}
	if !strings.Contains(out, `value="csrf-test"`) {
		t.Errorf("in-cart form missing CSRF token: %q", out)
	}
	if !strings.Contains(out, "In cart") {
		t.Errorf("needed row missing the In cart button label: %q", out)
	}
	// Every other status transition the member-facing /groceries page exposes
	// must be absent here.
	for _, forbidden := range []string{"Needed</button>", "Purchased</button>", "/groceries/shopping"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("kiosk shopping tab must not expose %q: %q", forbidden, out)
		}
	}
}

func TestKioskShoppingPage_InCartItemIsReadOnly(t *testing.T) {
	view := components.KioskShoppingView{
		InCart: []components.ShoppingItemView{
			{ID: "item-2", Name: "Eggs", QuantityLabel: "12 count", SourceLabel: "Manual", Status: "in_cart"},
		},
	}
	out := renderString(t, components.KioskShoppingPage(view))
	if !strings.Contains(out, "Eggs") {
		t.Errorf("shopping page missing in-cart item: %q", out)
	}
	if strings.Contains(out, "<form") {
		t.Errorf("an already-in-cart row must expose no further action: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Photos tab
// ---------------------------------------------------------------------------

func TestKioskPhotosPage_RendersGrid(t *testing.T) {
	view := components.KioskPhotosView{
		Photos: []components.PhotoView{{ID: "p1", RawURL: "/kiosk/photos/p1/raw", Caption: "Beach"}},
	}
	out := renderString(t, components.KioskPhotosPage(view))
	if !strings.Contains(out, "/kiosk/photos/p1/raw") {
		t.Errorf("photos grid missing image src: %q", out)
	}
	if strings.Contains(out, "<form") || strings.Contains(out, "delete") {
		t.Errorf("photos tab must be read-only (no upload/delete affordance): %q", out)
	}
}

func TestKioskPhotosPage_EmptyState(t *testing.T) {
	out := renderString(t, components.KioskPhotosPage(components.KioskPhotosView{}))
	if !strings.Contains(out, "No photos yet") {
		t.Errorf("empty photos page missing empty-state copy: %q", out)
	}
}
