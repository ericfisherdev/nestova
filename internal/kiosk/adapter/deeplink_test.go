package adapter

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	deeplinkapp "github.com/ericfisherdev/nestova/internal/deeplink/app"
	deeplinkdomain "github.com/ericfisherdev/nestova/internal/deeplink/domain"
)

// White-box (package adapter, not adapter_test) coverage for the absolute
// deep-link URL builder: deepLinkURL/resolveBaseURL have no exported
// surface, and decoding a QR code's content back out of the PNG
// KioskWebHandlers ultimately renders would need a QR decoder dependency
// this project does not otherwise carry — testing the URL string directly,
// one layer below the (already-covered, in internal/platform/qrcode)
// PNG encoding step, verifies the same absolute-URL contract without one.

func newTestKioskHandlersForDeepLinks(t *testing.T, publicBaseURL string, now time.Time) *KioskWebHandlers {
	t.Helper()
	signer, err := deeplinkapp.NewSigner([]byte("kiosk-deeplink-url-test-key"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Only the fields deepLinkURL/resolveBaseURL/deepLinkQR actually touch are
	// set; every other KioskWebHandlers dependency is irrelevant to this test.
	return &KioskWebHandlers{
		deepLinkSigner: signer,
		publicBaseURL:  publicBaseURL,
		now:            func() time.Time { return now },
	}
}

func TestKioskWebHandlers_deepLinkURL_UsesConfiguredPublicBaseURL(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestKioskHandlersForDeepLinks(t, "https://kiosk.test", now)

	// The request's own Host must be ignored once PublicBaseURL is set
	// (NES-129: PUBLIC_BASE_URL exists precisely to override it).
	req := httptest.NewRequest("GET", "http://example-should-be-ignored.local/kiosk/chores", nil)

	link, err := h.deepLinkURL(req, deeplinkdomain.ActionClaimTask, "abc-123")
	if err != nil {
		t.Fatalf("deepLinkURL: %v", err)
	}
	const want = "https://kiosk.test/go/claim-task/abc-123?"
	if !strings.HasPrefix(link, want) {
		t.Errorf("deepLinkURL() = %q, want prefix %q", link, want)
	}
}

func TestKioskWebHandlers_deepLinkURL_FallsBackToRequestHostWhenUnconfigured(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestKioskHandlersForDeepLinks(t, "", now)

	req := httptest.NewRequest("GET", "http://kiosk.local/kiosk/chores", nil)

	link, err := h.deepLinkURL(req, deeplinkdomain.ActionAddChore, "")
	if err != nil {
		t.Fatalf("deepLinkURL: %v", err)
	}
	const want = "http://kiosk.local/go/add-chore?"
	if !strings.HasPrefix(link, want) {
		t.Errorf("deepLinkURL() = %q, want prefix %q", link, want)
	}
}

func TestKioskWebHandlers_deepLinkURL_ExpirySignedFromNow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newTestKioskHandlersForDeepLinks(t, "https://kiosk.test", now)
	req := httptest.NewRequest("GET", "http://kiosk.local/kiosk/chores", nil)

	link, err := h.deepLinkURL(req, deeplinkdomain.ActionRedeemReward, "reward-1")
	if err != nil {
		t.Fatalf("deepLinkURL: %v", err)
	}

	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", link, err)
	}
	gotExp, err := strconv.ParseInt(parsed.Query().Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("exp query param is not an integer: %v", err)
	}
	wantExp := now.Add(deeplinkapp.LinkTTL).Unix()
	if gotExp != wantExp {
		t.Errorf("exp = %d, want %d (now + deeplinkapp.LinkTTL)", gotExp, wantExp)
	}
	if parsed.Query().Get("sig") == "" {
		t.Error("deepLinkURL() produced no sig query parameter")
	}
}
