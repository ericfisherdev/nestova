# Kiosk display (NES-128)

The kiosk is a wall-mounted touchscreen that shows Nestova's read-mostly
household data — chores, calendar, meals, shopping, and photos — without a
member login. It authenticates as a **device**, not a member, so a LAN guest
standing in front of the screen cannot browse family data: there is no login
form on the kiosk at all (see `internal/kiosk/adapter/session.go`'s
`RequireKioskOrMember`, which always returns a bare 401 instead of ever
surfacing `/login`).

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

## Running Chromium in kiosk mode

Point Chromium at the activated device's session and launch it fullscreen,
without error dialogs or "restore pages" prompts:

```sh
chromium \
  --kiosk \
  --noerrdialogs \
  --disable-session-crashed-bubble \
  --incognito \
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
- `--incognito` — the device's cookie (set once by `/kiosk/activate`) is the
  only persisted state this profile needs; incognito avoids accumulating
  browser cache/history on an always-on public display. The activation step
  must be re-run after a restart in this mode — pair it with a systemd unit
  (below) that only needs to run once per physical device setup, not per
  Chromium restart, by pointing the browser directly at `/kiosk` after the
  cookie has already been set via a non-incognito activation pass, or by
  dropping `--incognito` if session persistence across restarts is preferred
  for a given deployment.
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
