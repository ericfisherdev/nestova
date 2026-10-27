# Roles and route gates

Nestova owns no roles of its own. The vocabulary is the shared identity one
(`owner` / `adult` / `child`), aliased from nestcore as `household.Role`.

| Predicate | Who passes |
|---|---|
| any member | a signed-in member of any role |
| administrator | `Role.CanAdminister()`: owner or adult |
| owner only | `Role == RoleOwner` |
| step-up | the session's last MFA verification is fresh (`RequireStepUp`); applies only to members enrolled in a second factor |

Where the gate lives:

- **route**: composable middleware in `cmd/server/home.go`. The only role
  middleware is `authadapter.RequireAdministrator()`; `RequireMember` and
  `RequireStepUp` cover authentication and step-up.
- **handler**: an inline check inside the handler (`isParent`, `requireAdmin`,
  `requireParent`, `requireOwner`, or a direct `Role` comparison). A route
  with only `RequireMember` and a handler-local gate is listed as `handler`.

## Member provisioning policy (NES-85)

`household.CanProvisionRole(actor, target)` is the rule: an administrator may
mint an adult or a child; nobody mints an owner through this form (the owner is
created once by the onboarding wizard); a child mints nothing. An adult adding
another adult is lateral, not escalation, and is the normal "add my partner"
flow. The handlers repeat the checks as defence in depth.

Any future link to `/members/new` must be rendered only when the viewer's role
satisfies `CanAdminister()`; today nothing in `web/components` links to it.

## State-changing routes

Public routes (no session) are listed first, then everything behind
`RequireMember`.

| Route | Gate | Enforced in |
|---|---|---|
| `POST /setup` | first-run setup token | handler (`internal/platform/setup`) |
| `POST /onboarding` | first-run guard (no household may exist) | handler |
| `POST /login`, `/login/mfa`, `/login/mfa/passkey/finish`, `/login/passkey/finish` | public (credential check) | handler |
| `POST /logout` | public | handler |
| `POST /kiosk/activate` | public (single-use activation code) | handler |
| `POST /kiosk/shopping/{id}/in-cart` | kiosk device or any member | route (`RequireKioskOrMember`) |
| `GET /members/new`, `POST /members` | **administrator** (and `CanProvisionRole`) | route (`RequireAdministrator`) and handler |
| `POST /tasks`, `/tasks/{id}/complete`, `/skip`, `/claim`, `/tasks/{id}/photos` | any member | route (`RequireMember`) |
| `POST /trades`, `/trades/{id}/accept`, `/decline`, `/cancel` | any member | route |
| `POST /rewards/{id}/redeem`, `/rewards/redemptions/{id}/cancel` | any member | route |
| `POST /admin/rewards`, `/admin/rewards/{id}`, `/{id}/archive` | administrator | handler (`isParent`) |
| `POST /admin/rewards/redemptions/{id}/fulfill`, `/deny` | administrator | handler (`isParent`) |
| `POST /groceries/*` (items, usage, pantry, shopping) | any member | route |
| `POST /meals/finder`, `/meals/recipes`, `/meals/recipes/{id}`, `/meals/recipes/{id}/delete`, `/meals/plan`, `/meals/plan/clear`, `/meals/plan/generate` | any member | route |
| `POST /calendar/google/connect` | any member (connects the caller's own Google account) | route |
| `POST /subscriptions`, `/subscriptions/{id}`, `/subscriptions/{id}/deactivate` | any member | route |
| `POST /photos`, `/photos/{id}/delete`, `/photos/{id}/add-to-album` | any member | route |
| `POST /albums`, `/albums/{id}`, `/albums/{id}/photos/{photoID}/remove`, `/move` | any member | route |
| `POST /go/add-chore`, `/go/{action}/{id}` | any member | route |
| `POST /settings/kiosk/generate` | administrator, step-up | route (`RequireStepUp`) and handler (`requireParent`) |
| `POST /settings/kiosk/{id}/revoke` | administrator | handler (`requireParent`) |
| `POST /settings/mfa/enroll`, `/confirm`, `/recovery-codes/regenerate`, `/disenroll` | any member (own enrollment) | route |
| `POST /settings/mfa/reset` | owner only | handler |
| `POST /settings/pin`, `/settings/pin/clear` | any member (own PIN) | route |
| `POST /settings/members/{id}/pin`, `/pin/reset` | administrator | handler (`requireAdmin`) |
| `POST /settings/notify/phone`, `/opt-in`, `/preferences` | any member (own settings) | route |
| `POST /settings/notify/quiet-hours` | owner only | handler (`requireOwner`) |
| `POST /settings/webauthn/register/begin`, `/register/finish` | any member (own passkeys), step-up | route (`RequireStepUp`) |
| `POST /settings/webauthn/{id}/rename`, `/revoke` | any member (own passkeys) | route |

## Follow-ups

The audit found state-changing household routes with no role check at all
(subscriptions, photo and album deletion, recipe deletion). They are out of
scope for NES-85 and tracked in NES-210. The handler-local administrator gates
above (`isParent`, `requireAdmin`, `requireParent`) are candidates for moving
onto `RequireAdministrator` if wanted.
