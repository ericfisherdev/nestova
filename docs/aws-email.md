# Email notifications: enabling Amazon SES (NES-141)

Nestova can deliver notifications by email behind a swappable `EmailSender`
port. Every install starts with `NoopEmailSender` (`NOTIFY_EMAIL_ENABLED=false`,
the default) — it logs the send and returns without any AWS dependency, so a
fresh install never needs AWS credentials just to boot. An install that wants
real email delivery can opt into Amazon SES instead. This page is the operator
runbook for that switch.

This deployment stays **deliberately in the SES sandbox**: all four family
addresses get verified individually, rather than moving to SES production
access (which requires domain/DKIM verification and an AWS review). Sandbox
mode caps the abuse surface to only the addresses you have explicitly
verified — see [Sandbox limits](#sandbox-limits-and-the-graduation-path)
below — at negligible cost (roughly $0.03/mo at family volume). Email is
intended as the default rich channel; SMS ([`docs/aws-sms.md`](aws-sms.md))
stays reserved for urgent, time-sensitive alerts.

## Verifying the four family addresses

In the SES console (or via `aws sesv2 create-email-identity --email-identity
<address> --region "$SES_REGION"`), verify each family member's own email
address individually —
this is required in the SES sandbox regardless of who the sender is: **every**
recipient must be verified too, not just the sending address. Verification
sends a confirmation link to the address; it must be clicked before SES will
accept mail to (or from) that address.

You need, at minimum: one verified sending address (`SES_FROM_ADDRESS`) and
every recipient address the household actually uses verified the same way. A
send to an address that has not completed this step fails immediately with
`MessageRejected` — see [Rejected recipients](#rejected-recipients-and-bounce-handling)
below.

## Required IAM permissions

Scope the sending credential to `ses:SendEmail` only — it must not carry any
identity-management or account-configuration permissions; those are
console/admin actions, never the application's own runtime credential. Scope
the `Resource` to the verified sending identity's ARN and pin the
`ses:FromAddress` condition to `SES_FROM_ADDRESS`, so a leaked application
credential cannot send from any other verified identity in the account.
Replace `<region>`, `<account-id>`, and `<SES_FROM_ADDRESS>` with this
deployment's real values:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SESSend",
      "Effect": "Allow",
      "Action": ["ses:SendEmail"],
      "Resource": "arn:aws:ses:<region>:<account-id>:identity/<SES_FROM_ADDRESS>",
      "Condition": {
        "StringEquals": {
          "ses:FromAddress": "<SES_FROM_ADDRESS>"
        }
      }
    }
  ]
}
```

## Configuration reference

These are the same `EmailConfig` settings `cmd/server` reads (see
`internal/platform/config/config.go`'s `EmailConfig`):

| Variable | Required | Notes |
|---|---|---|
| `NOTIFY_EMAIL_ENABLED` | yes | Set to `true` to select the SES sender. Every `SES_*` setting below is parsed/validated only when this is `true` — a disabled deployment never fails startup on a stray `SES_*` value it will never use. |
| `SES_FROM_ADDRESS` | yes (when enabled) | The verified sending address `SendEmail` sends from. |
| `SES_REGION` | yes (when enabled) | The Region SES is configured in. **Pin this explicitly** — SES identity verification (the four family addresses above) is per-Region, so a mismatched Region here would try to send from/to addresses that were never verified in that Region at all. |
| `SES_ACCESS_KEY_ID` / `SES_SECRET_ACCESS_KEY` | no (both-or-neither) | Static credentials. Leave both blank to use the AWS SDK's default credential chain instead. |

## Rejected recipients and bounce handling

Sending to a recipient address the SES sandbox has not verified is rejected
**synchronously** — SES returns `MessageRejected` on the `SendEmail` call
itself, not an asynchronous bounce notification. This is the day-one case
this ticket handles.

SES uses the same `MessageRejected` error, with the same `Email address is
not verified` text, for at least three different underlying causes: an
unverified **recipient** (the sandbox case above), an unverified **sender**
identity (a misconfigured `SES_FROM_ADDRESS`), and unrelated invalid-content
rejections. The SDK gives no structured field to tell these apart, so
`SESEmailSender` parses the rejection's own message text: only when it names
the destination address being sent to (requires both the "not verified"
phrase and that exact address to appear) does it map the error to
`domain.ErrRecipientRejected`. Every other `MessageRejected` — including one
caused by our own sender identity, or by invalid message content — is
surfaced as a plain, non-downgrading send failure instead. This matters
because a sender-identity misconfiguration is *our* bug, not evidence that a
member's address is bad; treating it as a recipient rejection would
needlessly downgrade a valid member's preferences and blame the wrong thing.
See `isDestinationRejection` in `internal/notify/adapter/email_ses.go` for
the exact heuristic, and its tests for the sender-identity and
invalid-content cases that must NOT be downgraded.

Only a genuine `domain.ErrRecipientRejected` — the destination-address case
— is terminal and non-retryable at the `EmailNotificationSender` layer. When
a send is rejected this way, `EmailNotificationSender` does two things,
best-effort, in addition to the terminal failure itself:

1. **Downgrades that member's email preferences to in-app** — every
   `member_notification_pref` row currently set to `email` for that member
   flips to `inapp`, so they stop silently missing future notifications
   routed to an address that does not accept mail from this deployment.
2. **Warns the household's owner-role members in-app**, naming the affected
   member, so a human knows to check (and re-verify, if needed) that
   member's address.

Any other terminal email failure — a sender-identity `MessageRejected`, an
invalid-content rejection, or any other SES/network error — is NOT treated
as a recipient problem: no preference downgrade, no owner warning. It still
fails delivery for that one notification, and (like every terminal channel
failure) still goes through the fallback below.

Separately, `Dispatcher.fallbackToInApp` (channel-agnostic, shared with SMS)
re-enqueues the ORIGINAL notification's own content to in-app whenever the
email send fails for any reason — including a disabled email channel (see
[Configuration reference](#configuration-reference): with
`NOTIFY_EMAIL_ENABLED=false` the dispatcher never registers an email sender
at all, so an email-preference notification fails to resolve a sender and
falls back the same way a send failure would) — regardless of the specific
failure reason. This fallback is complementary to, not a substitute for, the
preference-downgrade above: one recovers the message that was about to be
lost, the other (only for genuine recipient rejections) prevents the next
one from being lost the same way.

There is no asynchronous bounce/complaint webhook wired up (SNS notification
topics, etc.) — that is out of this ticket's sandbox scope. If this
deployment ever graduates out of the sandbox (below), revisit this page: a
production SES identity can receive real hard bounces and complaints
asynchronously, which this synchronous-rejection-only handling does not cover.

## Sandbox limits and the graduation path

While in the SES sandbox:

- You can only send **to** a verified identity (the four family addresses
  above) — never to an arbitrary address.
- The default sandbox sending quota is **200 messages per 24-hour period**,
  at a modest rate limit (1 message/second by default) — far above what a
  household notification volume needs, but worth knowing if a bug were to
  loop.
- There is no domain/DKIM verification requirement, no dedicated IP
  warm-up, and no AWS support-case review needed — this is exactly why the
  sandbox is the right choice for a four-person household deployment: it
  trades sending to arbitrary addresses (never needed here) for a much
  simpler, faster, cheaper setup.

**Graduating to SES production access** (only if this deployment's needs
change — e.g. sending to addresses outside the household) requires: verifying
a domain (not just individual addresses) with DKIM, requesting production
access via an AWS support case describing the use case and bounce/complaint
handling process, and — per the bounce-handling note above — wiring a real
asynchronous bounce/complaint notification path (SNS) before relying on it in
production, since synchronous `MessageRejected` handling alone is a sandbox-mode
simplification, not a substitute for real bounce processing at production
volume.

## Spend safety

Email at SES sandbox, family volume is roughly $0.03/mo — SES pricing is
per-message and this deployment's monthly cost budget
([`docs/aws-guardrails.md`](aws-guardrails.md), Budget 1, account-wide, $10/mo)
already covers it; there is no dedicated per-service SES budget the way SMS
has its own (Budget 2), since SES's own sandbox recipient restriction is
already the primary abuse-surface guardrail here — a runaway loop cannot
spend meaningfully more than 200 messages/day can cost, and cannot reach
anyone outside the four verified addresses regardless.

## Metrics

Every send attempt through the SES sender increments the
`nestova_email_sends_total` Prometheus counter, labeled `result` (`sent`,
`failed`, `rejected`) — see `internal/platform/metrics/email.go`. This is
scoped to `NOTIFY_EMAIL_ENABLED=true` deployments specifically:
`NoopEmailSender` is never instrumented (see that type's own doc).

Dispatcher-level fallback to in-app is tracked separately, in its own
`nestova_email_fallbacks_total` counter — it is not a `result` value on
`nestova_email_sends_total`, because it is a different kind of event (a
dispatcher re-enqueue action, not a send attempt) and counting it as a send
result would double-count against `nestova_email_sends_total`'s own
`failed`/`rejected` totals. `nestova_email_fallbacks_total` increments both
for a terminal send failure (including a `rejected` one) and for a
notification that could not be delivered because the email channel was not
registered at all — i.e. `NOTIFY_EMAIL_ENABLED=false` with a member still
preferring email (see [Rejected recipients](#rejected-recipients-and-bounce-handling)
above). SMS reports the identical pair of counters
(`nestova_sms_sends_total` / `nestova_sms_fallbacks_total`) for the same
reason — see [`docs/aws-sms.md`](aws-sms.md).

## The runbook

1. **Verify** the sending address and every recipient address (the four
   family addresses) in the SES console, and create a send-only IAM
   credential (above).
2. **Configure**: set `NOTIFY_EMAIL_ENABLED=true`, `SES_FROM_ADDRESS`,
   `SES_REGION`, and the rest of the table above in the environment.
3. **Restart `cmd/server`** and confirm the boot log reports `email sender
   configured provider=ses` (not `provider=noop`).
4. **Send a test notification** to a verified recipient and confirm both the
   HTML and plain-text parts render correctly in a real mail client.
5. **Watch `nestova_email_sends_total`** on the first few real sends, and
   confirm a deliberately-unverified test address (if you want to exercise
   the path) produces a `rejected` result and an in-app owner warning, not a
   silently lost notification.
