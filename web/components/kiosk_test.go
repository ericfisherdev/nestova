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

// extractTag returns the single HTML element of the given tag (e.g. "a",
// "form") — from its opening "<tag" to its closing "</tag>" — that contains
// needle, so a test can assert on classes/attributes/text scoped to exactly
// that element rather than the whole page (where a substring match could
// accidentally be satisfied by an unrelated element, such as a section
// heading that happens to repeat a button's label).
func extractTag(html, needle, tag string) string {
	idx := strings.Index(html, needle)
	if idx < 0 {
		return ""
	}
	start := strings.LastIndex(html[:idx], "<"+tag)
	if start < 0 {
		return ""
	}
	closeTag := "</" + tag + ">"
	end := strings.Index(html[idx:], closeTag)
	if end < 0 {
		return ""
	}
	return html[start : idx+end+len(closeTag)]
}

// extractTagByTestID is extractTag scoped to a data-testid attribute, the
// common case among this file's tests.
func extractTagByTestID(html, testID, tag string) string {
	return extractTag(html, `data-testid="`+testID+`"`, tag)
}

// assertKioskContentPollWiring asserts that html's #wrapperID div's OWN
// opening tag carries the complete NES-130 self-poll wiring: the wrapper id
// plus all four htmx attributes (hx-get, hx-trigger, hx-target, hx-swap).
// extractTag alone is not narrow enough here: every kiosk tab's wrapper
// contains nested divs, so extractTag's "first matching closing tag" is a
// DESCENDANT's "</div>", not the wrapper's own — scanning that whole range
// would let an attribute that drifted onto a descendant element satisfy a
// check meant for the wrapper itself. Slicing at the wrapper's own first '>'
// rules that out.
func assertKioskContentPollWiring(t *testing.T, html, wrapperID, contentRoute string) {
	t.Helper()
	wrapper := extractTag(html, `id="`+wrapperID+`"`, "div")
	if wrapper == "" {
		t.Fatalf("could not locate the %s wrapper element in: %q", wrapperID, html)
	}
	openTagEnd := strings.Index(wrapper, ">")
	if openTagEnd < 0 {
		t.Fatalf("could not locate the end of the %s wrapper's opening tag in: %q", wrapperID, wrapper)
	}
	openTag := wrapper[:openTagEnd]
	for _, want := range []string{
		`id="` + wrapperID + `"`,
		`hx-get="` + contentRoute + `"`,
		`hx-trigger="every 15s"`,
		`hx-target="this"`,
		`hx-swap="outerHTML"`,
	} {
		if !strings.Contains(openTag, want) {
			t.Errorf("%s wrapper opening tag missing content-poll wiring %q: %q", wrapperID, want, openTag)
		}
	}
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
	active := extractTagByTestID(out, "kiosk-tab-shopping", "a")
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
	inactive := extractTagByTestID(out, "kiosk-tab-chores", "a")
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
	// kiosk-idle.js AND album.js each register an Alpine.data provider via
	// an 'alpine:init' listener, so both must load and run before
	// alpine.min.js (layout.templ's documented script-order rule; album.js
	// previously loaded after Alpine and its component silently never
	// registered — NES-147).
	props := components.KioskShellProps{Active: components.KioskTabChores}
	out := renderString(t, components.KioskLayout(props, templ.Raw("")))

	// Match the src attributes, not bare filenames — the head comments
	// mention these script names too, and a comment match would compare
	// prose positions instead of actual load order.
	alpineIdx := strings.Index(out, `src="/static/js/alpine.min.js"`)
	if alpineIdx == -1 {
		t.Fatalf("missing alpine.min.js script tag in shell: %q", out)
	}
	for _, script := range []string{`src="/static/js/kiosk-idle.js"`, `src="/static/js/album.js"`} {
		idx := strings.Index(out, script)
		if idx == -1 {
			t.Fatalf("missing %s in shell: %q", script, out)
		}
		if idx > alpineIdx {
			t.Errorf("%s must load before alpine.min.js, got order: %q", script, out)
		}
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

// TestKioskChoresPage_ContentPollWiring asserts the chores tab's content
// wrapper carries the shared NES-130 self-poll attributes, driven by the
// view model's ContentPollTrigger field rather than a hardcoded interval.
func TestKioskChoresPage_ContentPollWiring(t *testing.T) {
	out := renderString(t, components.KioskChoresPage(components.KioskChoresView{ContentPollTrigger: "every 15s"}))
	assertKioskContentPollWiring(t, out, "kiosk-chores-content", "/kiosk/chores/content")
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

// TestKioskCalendarPage_ContentPollWiring mirrors
// TestKioskChoresPage_ContentPollWiring for the calendar tab (NES-130).
func TestKioskCalendarPage_ContentPollWiring(t *testing.T) {
	view := components.KioskCalendarView{ContentPollTrigger: "every 15s"}
	out := renderString(t, components.KioskCalendarPage(view))
	assertKioskContentPollWiring(t, out, "kiosk-calendar-content", "/kiosk/calendar/content")
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

// TestKioskMealsPage_ContentPollWiring mirrors
// TestKioskChoresPage_ContentPollWiring for the meals tab (NES-130).
func TestKioskMealsPage_ContentPollWiring(t *testing.T) {
	view := components.KioskMealsView{ContentPollTrigger: "every 15s"}
	out := renderString(t, components.KioskMealsPage(view))
	assertKioskContentPollWiring(t, out, "kiosk-meals-content", "/kiosk/meals/content")
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
	// Scoped to the action form itself, not the whole page: the page also
	// carries an "In cart" SECTION HEADING (for the in-cart list below), so a
	// bare strings.Contains(out, "In cart") would pass even if the button
	// itself were missing or renamed.
	inCartForm := extractTag(out, `action="/kiosk/shopping/item-1/in-cart"`, "form")
	if inCartForm == "" {
		t.Fatalf("could not locate the in-cart form element in: %q", out)
	}
	if !strings.Contains(inCartForm, "In cart") {
		t.Errorf("needed row's action form missing the In cart button label: %q", inCartForm)
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

// TestKioskShoppingPage_ContentPollWiring mirrors
// TestKioskChoresPage_ContentPollWiring for the shopping tab (NES-130). The
// wrapper's own poll wiring is independent of the needed row's plain
// (non-htmx) in-cart form — see KioskShoppingPage's doc comment.
func TestKioskShoppingPage_ContentPollWiring(t *testing.T) {
	view := components.KioskShoppingView{ContentPollTrigger: "every 15s"}
	out := renderString(t, components.KioskShoppingPage(view))
	assertKioskContentPollWiring(t, out, "kiosk-shopping-content", "/kiosk/shopping/content")
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

// TestKioskPhotosPage_ContentPollWiring mirrors
// TestKioskChoresPage_ContentPollWiring for the photos tab (NES-130).
func TestKioskPhotosPage_ContentPollWiring(t *testing.T) {
	view := components.KioskPhotosView{ContentPollTrigger: "every 15s"}
	out := renderString(t, components.KioskPhotosPage(view))
	assertKioskContentPollWiring(t, out, "kiosk-photos-content", "/kiosk/photos/content")
}
