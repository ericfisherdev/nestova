# Passkey registration, login, and step-up (NES-136 / NES-137)

Nestova's WebAuthn support has two parts: NES-136 built opt-in platform
passkey (Face ID, Android fingerprint, Windows Hello) registration and
self-service device management (the `/settings` "Your devices" section —
register, rename, revoke); NES-137 built usernameless passkey login and
passkey-based step-up, both documented below.

## Requirements

Passkey registration is only offered when `PUBLIC_BASE_URL` is configured
(see the README's `.env.example`). This is a hard requirement, not a nicety:
a WebAuthn Relying Party ID (RP ID) must be a single, fixed value pinned once
at server startup — unlike the kiosk's deep-link origin, which is safely
derived per-request from the incoming `Host` header, the RP ID is baked into
every credential an authenticator ever registers, so it cannot vary. A
deployment with no `PUBLIC_BASE_URL` set simply does not offer the "Your
devices" section at all; `cmd/server/main.go` skips constructing the
WebAuthn Relying Party entirely in that case.

The RP ID is derived automatically from `PUBLIC_BASE_URL`'s host (no
separate env var) — see `webauthnRPID` in `cmd/server/main.go`.

### Changing `PUBLIC_BASE_URL` orphans existing passkeys

**Do not change `PUBLIC_BASE_URL`'s host once passkeys have been
registered.** A browser will only offer a stored passkey to the exact
Relying Party ID it was registered under — that part is inherent to the
WebAuthn specification, and would be true no matter how a Relying Party
chooses its RP ID. What makes a `PUBLIC_BASE_URL` host change specifically
the trigger here is Nestova's own derivation strategy (`webauthnRPID`,
`cmd/server/main.go`): the RP ID is computed FROM that setting, with no
separate, independently pinned RP ID override. A deployment that instead
configured a fixed RP ID unrelated to `PUBLIC_BASE_URL` would not tie
passkey validity to this particular config value at all — that is simply
not the strategy Nestova uses. Given Nestova's actual derivation, changing
the host makes every previously registered passkey permanently unusable —
the only recovery is for each member to revoke and re-register.

## Registration flow

1. A member opens **Settings** and, in the **Your devices** section, clicks
   **Add a passkey**.
2. The client (`web/static/js/webauthn-register.js`) `POST`s
   `/settings/webauthn/register/begin`, which returns a JSON credential
   creation options payload and stores a matching, single-use challenge in
   the member's session.
3. The browser calls `navigator.credentials.create()`, prompting the
   platform authenticator (Face ID, fingerprint, Windows Hello). User
   verification is **required**, not merely preferred — a registered
   passkey is always gated by the device's own biometric/PIN prompt.
4. The client `POST`s the resulting attestation response, as JSON, to
   `/settings/webauthn/register/finish` along with a member-chosen
   nickname. The server verifies it against the pending challenge — cleared
   from the session immediately, win or lose, so it can never be reused —
   and persists the new credential.
5. The page reloads, showing the new device in the list.

### Step-up

Both `/settings/webauthn/register/begin` and `.../finish` require a fresh
login MFA verification (`internal/auth/adapter.RequireStepUp`, built for
NES-135), not just an authenticated session: registering a passkey mints a
durable credential, the same security-sensitivity as kiosk device token
provisioning. A member with no TOTP MFA enrolled at all has nothing to step
up from and reaches these routes normally; renaming or revoking an existing
passkey is not step-up-gated, mirroring the kiosk section's own asymmetry
(only minting a *new* credential is gated).

## Login flow (NES-137)

1. A member visits **Sign in** and clicks **Sign in with passkey** — first,
   above the password form (kid-friendliest: one biometric gesture, no
   typed identifier).
2. The client (`web/static/js/login-passkey.js`) `GET`s
   `/login/passkey/begin`, which returns JSON assertion options with an
   **empty** `allowCredentials` list — usernameless: the browser is free to
   offer any of its discoverable credentials for this Relying Party, since
   the server does not yet know who is signing in — and stores a matching,
   single-use challenge on the (anonymous, pre-auth) session.
3. The browser calls `navigator.credentials.get()`, prompting whichever
   platform authenticator the member picks. User verification is
   **required**, same as registration.
4. The client `POST`s the resulting assertion response, as JSON, to
   `/login/passkey/finish`. The server resolves WHICH member is
   authenticating from the assertion's own reported `userHandle` —
   `WebAuthnCredentialRepository.FindByUserHandle`, keyed on the
   `member_credential.user_handle` index — verifies the response, and, on
   success, promotes the session exactly like a completed password+TOTP
   login: a user-verified passkey assertion counts as both factors in one
   gesture, so there is no separate pending-MFA hand-off.
5. The response is `{"redirect": "<server-sanitized path>"}` — the client
   navigates there. The redirect target is always computed server-side
   (`sanitizeNext`), never trusted from the client's own copy of `next`.

## Step-up flow (NES-137)

The **step-up prompt** (`/login/mfa`, built for NES-135's TOTP/recovery-code
flow) additionally offers **"use your passkey"** whenever the pending
member has at least one registered passkey
(`LoginMFAHandlers.hasPasskey`). Unlike login's usernameless ceremony, this
one is TARGETED — `PasskeyBegin`/`PasskeyFinish`
(`internal/auth/adapter/login_mfa.go`) list the member's own credentials in
`allowCredentials`, since their identity is already known.

`RequireStepUp` (`internal/auth/adapter/session.go`) now treats a
registered passkey the SAME as a confirmed TOTP enrollment when deciding
whether a member has anything to step up FROM at all — a member with a
passkey but no TOTP would otherwise fall through the gate unconditionally,
exactly like a password-only member with no second factor, which is wrong.
For the same reason, `Handlers.Login`'s own hand-off decision
(`needsLoginStepUp`) ALSO checks for a registered passkey, not just
confirmed TOTP — logging in with a password alone must never mark a
passkey-having member's session as freshly verified, or RequireStepUp's own
gate would be trivially satisfied by a stamp Login placed there for the
wrong reason. Registering a NEW passkey (NES-136's own `RegisterFinish`)
likewise stamps the session fresh: a UV-gated proof of possession over a
brand-new credential is at least as strong as a TOTP code, so it satisfies
step-up for the rest of that session exactly like completing an explicit
prompt would — without this, registering a member's very FIRST passkey
would immediately trip their own next step-up-gated request.

## Sign-count anomaly detection (NES-137)

After every successful login or step-up assertion, the authenticator's
reported signature counter is compared against the value on file
(`member_credential.sign_count`) and always advanced to the new value
(`WebAuthnCredentialRepository.UpdateAfterAssertion`), regardless of the
outcome below. A new count is flagged as suspicious — and raises a
member-facing notification through the standard outbox — **only when it is
nonzero AND less than the stored value**. A synced passkey (iCloud
Keychain, Google Password Manager) permanently reports a counter of 0 by
design, since it has no single physical counter to track; this rule is
deliberately narrower than go-webauthn's own default `CloneWarning`
semantics (which would also flag a nonzero-to-zero transition) so that a
synced passkey's baseline can never false-positive. A flagged decrease does
**not** block the login or step-up — this is a family-appliance threat
model, not a high-assurance one: the member is notified, but access is not
denied on a signal this soft.

## Data model

`member_credential` (migration `00034_member_webauthn_credential.sql`)
stores one row per registered passkey: the WebAuthn credential id, the CBOR
public key (not encrypted at rest — a public key is not a secret), the
signature counter, advertised transports, the authenticator's AAGUID (when
reported), a member-chosen nickname, and the member's stable, HMAC-derived
WebAuthn user handle.

The user handle (`internal/auth/app.WebAuthnUserHandleDeriver`) is
deliberately **deterministic** — `HMAC(key, memberID)`, not a stored random
value — rather than the WebAuthn spec's usual recommendation of a random
handle: it lets the server recompute a member's handle from their id alone
(no extra lookup table to keep in sync), and it gives NES-137's login
ceremony a value to search `member_credential.user_handle` by. The handle is
**pseudonymous, not anonymous**: it is opaque to the authenticator and to
anyone without the deriver's key (a raw `member_credential` row alone does
not reveal which member it belongs to), but the server itself — which holds
that key — can always recompute the mapping back to a real member id. It is
not a substitute for encrypting or otherwise protecting the
`member_credential` table; it only keeps the member's real id off the wire
during a usernameless login.
