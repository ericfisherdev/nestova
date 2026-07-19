# Passkey registration (NES-136)

Nestova's WebAuthn support has two parts: this ticket (NES-136) built opt-in
platform passkey (Face ID, Android fingerprint, Windows Hello) registration
and self-service device management (the `/settings` "Your devices" section —
register, rename, revoke). Login itself via a passkey is a follow-up ticket
(NES-137); nothing here changes how a member logs in today.

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
