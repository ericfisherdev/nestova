# Two-factor authentication (NES-134 / NES-135)

Nestova's TOTP-based two-factor authentication has two parts: NES-134 built
opt-in enrollment and self-service management (the `/settings` MFA section —
enroll, confirm, recovery codes, disenroll, and the household-owner admin
reset); NES-135 wires that enrollment into login itself.

## Login enforcement (NES-135)

A member with a **confirmed** enrollment cannot obtain a session with their
password alone. After password verification succeeds, `Handlers.Login`
(`internal/auth/adapter/http.go`) checks the member's enrollment status: if
confirmed, the session is parked in a pending state (`GET`/`POST
/login/mfa`) instead of being promoted, and the member must submit a current
authenticator code or an unused recovery code before the session becomes
authenticated. A member with no enrollment logs in exactly as before this
ticket.

### Replay protection

A TOTP code can only ever be accepted once. `member_mfa.last_totp_step`
(migration `00033_member_mfa_login.sql`) durably records the RFC 6238 step of
the most recently accepted login code; a resubmitted code — even one still
inside the ±1-period clock-skew window — is rejected once its step is no
longer strictly greater than the stored value. This lives in the database
rather than the session specifically because a member's phone and laptop
have independent sessions: a per-session guard would let the same code be
replayed once per device.

### Attempt limiting

Five wrong login codes are tolerated (a human squinting at a rotating
6-digit code on a small screen); the sixth triggers a five-minute backoff
window and enqueues an in-app notification to the member through the same
outbox every other Nestova notification uses
(`internal/auth/adapter/login_attempt_limiter.go`). This is in-memory,
process-lifetime state — the same accepted tradeoff `internal/deeplink/adapter`'s
`perKeyLimiter` documents for Nestova's single-household, local-first
appliance deployment shape.

### Remembered devices

Checking "remember this device" at the login MFA step sets a signed,
`HttpOnly` cookie (`nestova_remember`) valid for 30 days
(`internal/auth/app/remember.go`). A remembered device skips the login-time
prompt on a subsequent login, but the session it produces is **not** marked
freshly verified — see the next section.

### Step-up for sensitive actions

`RequireStepUp` (`internal/auth/adapter/session.go`) gates a
security-sensitive action (currently: kiosk device token provisioning, `POST
/settings/kiosk/generate`) on the session's login MFA verification being
fresh (within 15 minutes), independent of the remembered-device cookie. A
member with no confirmed enrollment always passes — there is nothing to step
up from. A stale or never-verified session is redirected back through
`/login/mfa` to re-prove the second factor (not the password — the member is
already authenticated) before the original destination is reachable again.

This ships as the general-purpose pattern for NES-137's own step-up needs
("managing another member's points", household settings changes); wiring
those is that ticket's own scope.

## Operations: clock accuracy

TOTP is a shared-secret-plus-time protocol: the server and every enrolled
member's authenticator app must agree on the current time to within the
±1-period (30-second) skew window this codebase tolerates
(`internal/platform/totp`). A server clock that has drifted further than
that will reject **every** member's correct code, indistinguishably from a
wrong one — there is no separate error path, so this failure mode looks
identical to "MFA is broken" from a support perspective.

Run `systemd-timesyncd` (or `chrony`) on the appliance and confirm it is
active and synced:

```sh
timedatectl status
# System clock synchronized: yes
# NTP service: active
```

On a Raspberry Pi with no RTC battery, the clock resets to boot-time defaults
on every power loss and only corrects once network time sync completes —
during that window, TOTP verification for every enrolled member will fail.
If the appliance is expected to serve logins before network sync completes
(e.g. immediately after a power outage), consider a battery-backed RTC
module so the clock starts close to correct even before the network is
reachable.

(This note's exact location may move once NES-153's broader deployment
documentation lands — the requirement itself does not depend on where it is
written down.)
