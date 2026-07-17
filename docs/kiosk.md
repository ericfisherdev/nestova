# Kiosk display (NES-128)

The kiosk is a wall-mounted touchscreen that shows Nestova's read-mostly
household data — chores, calendar, meals, shopping, and photos — without a
member login. It authenticates as a **device**, not a member, so a LAN guest
standing in front of the screen cannot browse family data: there is no login
form on the kiosk at all (see `internal/kiosk/adapter/session.go`'s
`RequireKioskOrMember`, which always returns a bare 401 instead of ever
surfacing `/login`).

**Threat model, stated explicitly:** device auth blocks an unauthenticated
client elsewhere on the LAN (or anyone who has not physically activated a
device) from reaching `/kiosk/*` at all. It does **not** — and is not meant
to — restrict what someone standing in front of an already-activated display
can see: physical access to the mounted screen itself is inside the trusted
boundary, exactly like a household's own paper calendar on the fridge. Anyone
who can touch the kiosk can view the five read-only tabs; only mutating an
item (the one allowed action, marking something in-cart) is gated further by
CSRF. If that boundary is ever too permissive for a given household's
placement of the display, mounting location — not application-layer
restriction — is the control to reach for.

## Provisioning a device

The settings page never displays the kiosk's long-lived bearer token — that
would leak a durable credential via browser history, access logs, or a
`Referer` header. Instead it issues a **short-lived (15-minute), single-use
activation code**, and the long-lived device token is generated only when the
kiosk device itself redeems that code.

1. A parent (owner/adult role) signs in on their own device and opens
   **Settings** (`/settings`).
2. Under **Kiosk display**, click **Generate activation code**. The code and
   an activation link are shown exactly once — only the code's SHA-256 hash is
   ever persisted, so this is the only chance to capture it. The code expires
   in 15 minutes and can be redeemed exactly once.
3. On the kiosk device itself, either open the activation link
   (`https://<host>/kiosk/activate?code=...`) or, if that's not convenient,
   navigate to `https://<host>/kiosk/activate` and type the code into the
   manual entry form. Either path redeems the code: the server mints a new
   long-lived device token, sets it as an `HttpOnly`, `SameSite=Lax` cookie on
   that browser, and lands on the chores tab.
4. Redeeming a code automatically revokes the household's previously active
   device, so replacing a lost or compromised device is a single action —
   there is no separate "revoke first" step required.

A parent can also revoke the active device directly at any time from the same
Settings section; the kiosk immediately loses access on its next request.

## Idle screensaver

After two minutes with no touch, the kiosk shows a full-screen rotating
slideshow of the household's earliest-created photo album (reusing the NES-76
album viewer's rendering and Alpine component). Touching the screen anywhere
dismisses the overlay; because the screensaver is an overlay on top of the
current tab rather than a page navigation, dismissing it always returns to
whichever tab was showing — there is no separate "last tab" state to restore.

## Live updates (NES-130)

Every kiosk tab's content is a self-polling htmx fragment: `GET /kiosk/{tab}`
returns the full page wrapped in the kiosk shell, and `GET
/kiosk/{tab}/content` re-renders just that tab's inner content, which is what
the tab polls (`hx-trigger="every 15s"`) to refresh itself in place. This
means a chore claimed or completed from a phone (the QR deep-link flow), or
any other change to household data, shows up on an untouched display within
15 seconds — no touch required. NES-129 originally added this only for the
chores tab (to keep its QR codes re-signed ahead of the deep link's 10-minute
expiry); NES-130 generalized it to every tab and tightened the shared
interval to 15 seconds, which is comfortably under half that expiry window,
so QR freshness is still covered by the same mechanism.

A real-time push mechanism (SSE) was considered and intentionally not built.
Tab navigation is a normal full-page link (`<a href="/kiosk/{tab}">`), not a
client-side tab switch, so only the ACTIVE tab's content div — and its own
poll — ever exists in the DOM; the other four tabs are not polling in the
background. The recurring load per kiosk device is therefore one request
every 15 seconds, and each of those requests is the same read path (and the
same Prometheus HTTP metrics instrumentation) as rendering that tab's page
once. A household is expected to run one, occasionally two, kiosk devices —
a single Raspberry Pi already serves that same page render on every
navigation, so sustaining it once every 15 seconds per active device adds no
new class of load.

Two long-session properties fall out of this design without any special
handling:

- **Network blips / an overnight idle display don't wedge the UI.** Each
  poll is an independent request; htmx does not swap the DOM on a non-2xx
  response (a device that lost its bearer token, e.g. after a revoke, gets a
  bare 401 from `RequireKioskOrMember`), so a failed poll simply leaves the
  last-good content in place and the next 15-second interval tries again —
  never a stuck half-rendered fragment or a browser error dialog.
- **Polling never resets the idle screensaver's timer.** `kiosk-idle.js`
  (NES-128) only listens for `touchstart`/`mousedown`/`keydown` — it has no
  htmx or DOM-mutation listener — so a content swap triggered by the 15s poll
  has no effect on when the screensaver appears.

## Running Chromium in kiosk mode

Point Chromium at the activated device's session and launch it fullscreen,
without error dialogs or "restore pages" prompts:

```sh
chromium \
  --kiosk \
  --noerrdialogs \
  --disable-session-crashed-bubble \
  --user-data-dir=/var/lib/nestova-kiosk/chromium-profile \
  --disable-pinch \
  --overscroll-history-navigation=0 \
  https://<host>/kiosk
```

- `--kiosk` — fullscreen, no browser chrome, no way to navigate away via the UI.
- `--noerrdialogs` — suppresses the "Chromium isn't your default browser"
  and similar interstitial dialogs that would otherwise sit on top of the
  display indefinitely on a screen with no keyboard.
- `--disable-session-crashed-bubble` — a prior crash or power-cycle must never
  greet the household with a "restore pages?" prompt instead of the kiosk.
- `--user-data-dir=...` — a dedicated, persistent profile directory for this
  kiosk account, not `--incognito`. The device's cookie (set once by
  `/kiosk/activate`) is the whole point of activation being a one-time step;
  `--incognito` discards it on every launch, which would force re-activation
  on every restart and, worse, mean the kiosk sits at the activation form —
  showing a live single-use code entry point — after every reboot instead of
  the read-only tabs. A dedicated `--user-data-dir` keeps the persisted state
  scoped to this one purpose-built profile rather than a general-purpose
  browser profile that might accumulate unrelated history.
- `--disable-pinch` / `--overscroll-history-navigation=0` — a touchscreen's
  pinch and edge-swipe gestures must not zoom or navigate away from the kiosk.

### systemd unit

Run Chromium as a long-lived service under the kiosk account, restarting it if
it ever exits:

```ini
# /etc/systemd/system/nestova-kiosk.service
[Unit]
Description=Nestova kiosk display
After=graphical.target network-online.target
Wants=network-online.target

[Service]
User=kiosk
Environment=DISPLAY=:0
ExecStart=/usr/bin/chromium \
  --kiosk \
  --noerrdialogs \
  --disable-session-crashed-bubble \
  --user-data-dir=/var/lib/nestova-kiosk/chromium-profile \
  --disable-pinch \
  --overscroll-history-navigation=0 \
  https://<host>/kiosk
Restart=always
RestartSec=3

[Install]
WantedBy=graphical.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now nestova-kiosk.service
```

### Disable screen blanking

The OS-level screensaver/DPMS must be disabled separately from Nestova's own
in-app idle screensaver — otherwise the display goes dark and Nestova's
photo slideshow never gets a chance to show. On a typical X11 kiosk account:

```sh
xset s off
xset -dpms
xset s noblank
```

Add these to the kiosk account's Xsession startup (or the systemd unit's
`ExecStartPre`, run against the same `DISPLAY`) so they apply on every boot.

## Security notes

- The kiosk cookie is `HttpOnly` (never readable from page JavaScript) and
  `SameSite=Lax`. Lax is not "never sent cross-site": a top-level navigation
  from another site (e.g. following a link) still carries it. What it
  withholds the cookie from is cross-site *subrequests* — image/iframe loads,
  `fetch`/XHR, and cross-site `POST`s — which is exactly the CSRF boundary
  that matters here: a malicious page cannot forge a `POST` (the kiosk's one
  mutation, marking a shopping item in-cart) using an ambient kiosk cookie.
  The per-session CSRF token on that form is the primary defense; `SameSite`
  is defense in depth.
- `/kiosk/*` routes never expose member-attributed actions (complete, claim,
  redeem, and every shopping-list transition except marking an item in-cart)
  — those are deferred to the QR-code, per-member deep link (NES-129).
- The kiosk relies on the appliance's existing Tailscale-only network
  exposure; no new inbound port or public endpoint is introduced.
