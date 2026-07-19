# SMS notifications: enabling AWS End User Messaging (NES-138)

Nestova can deliver notifications by SMS text message behind a swappable
`SMSSender` port. Every install starts with `NoopSMSSender`
(`NOTIFY_SMS_ENABLED=false`, the default) — it logs the send and returns
without any AWS dependency, so a fresh install never needs AWS credentials
just to boot. An install that wants real SMS delivery can opt into AWS End
User Messaging (the successor service to Amazon Pinpoint SMS) instead. This
page is the operator runbook for that switch.

This ticket ships only the port, the AWS adapter, and the spend safety
around it. It does **not** wire routing or preferences: no code enqueues a
`sms`-channel notification yet, since that requires member phone numbers
and delivery routing (NES-139). Flipping `NOTIFY_SMS_ENABLED=true` today
builds a working sender that nothing calls.

## Provisioning the origination number

Create a **toll-free number** in AWS End User Messaging and complete its
verification (toll-free numbers require a one-time registration review
before AWS allows sending) before touching any Nestova configuration — see
the team wiki's AWS setup page for account/console steps. You need, at
minimum: the verified number's phone number ID or ARN (this is
`SMS_ORIGINATION_IDENTITY`) and the Region it was provisioned in.

## Required IAM permissions

Scope the sending credential to SMS send only — it must not carry the
`sms-voice:SetTextMessageSpendLimitOverride` permission from
[`docs/aws-guardrails.md`](aws-guardrails.md); that permission belongs to
the operator's own admin credential, never to the application's runtime
credential.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SMSSend",
      "Effect": "Allow",
      "Action": ["sms-voice:SendTextMessage"],
      "Resource": "*"
    }
  ]
}
```

## Configuration reference

These are the same `SMSConfig` settings `cmd/server` reads (see
`internal/platform/config/config.go`'s `SMSConfig`):

| Variable | Required | Notes |
|---|---|---|
| `NOTIFY_SMS_ENABLED` | yes | Set to `true` to select the AWS End User Messaging sender. Every `SMS_*` setting below is parsed/validated only when this is `true` — a disabled deployment never fails startup on a stray `SMS_*` value it will never use. |
| `SMS_ORIGINATION_IDENTITY` | yes (when enabled) | The verified toll-free number's phone number ID or ARN `SendTextMessage` sends from. |
| `SMS_REGION` | yes (when enabled) | The Region the origination identity was provisioned in. |
| `SMS_ACCESS_KEY_ID` / `SMS_SECRET_ACCESS_KEY` | no (both-or-neither) | Static credentials. Leave both blank to use the AWS SDK's default credential chain instead. |
| `SMS_RETRY_MAX_ATTEMPTS` | no | Caps the AWS SDK's own built-in retryer (default 3). Kept tight deliberately: SMS is billed per attempt handed to the carrier, so an unbounded retry loop against a persistently failing destination is a real spend risk, not just a latency one. |

## Message body handling

A body is truncated to whatever fits in a SINGLE SMS segment rather than
being split across multiple — each additional segment is billed
separately, and the port's contract is "never split" (see
`truncateSMSBody` in `internal/notify/adapter/sms_encoding.go`). The cap
is encoding-aware, not a flat character count, since AWS itself encodes
and segments a message differently depending on its content:

- If every character in the body has a GSM-7 representation (the carrier's
  7-bit default alphabet — ASCII plus a handful of accented/European
  characters), the body is capped at **160 septets** and truncated with a
  trailing `...`. A handful of characters (`^ { } \ [ ~ ] | €`) come from
  GSM-7's extension table and cost **two** septets each, not one, so a
  body full of them can be truncated well under 160 characters.
- If the body contains any character GSM-7 cannot represent (an emoji,
  most non-Latin scripts), the WHOLE body — not just the offending
  character — is capped at **70 UTF-16 code units** instead and truncated
  with a trailing `…`, since SMS encoding is chosen once per message,
  never mixed within a segment.

Truncation is rune-aware, never byte-aware, in either case, so a
multi-byte UTF-8 character (GSM-7 path) or a UTF-16 surrogate pair
(UCS-2 path) is never cut in half.

## Opted-out recipients

AWS reports a destination that has opted out of SMS (replied STOP, or was
opted out by a carrier) as a `ConflictException`. The adapter maps this to
`domain.ErrRecipientOptedOut` and does not retry it — the carrier will not
deliver to that number no matter the attempt count, so retrying only spends
budget for nothing. Every other **retryable** send failure (throttling, a
5xx service error) is retried up to `SMS_RETRY_MAX_ATTEMPTS` via the SDK's
own retryer before being returned as a generic wrapped error. A
non-retryable failure — a validation error (e.g. a malformed
`SMS_ORIGINATION_IDENTITY`) or an authorization error (e.g. a credential
without `sms-voice:SendTextMessage`) — fails immediately, on the first
attempt, since the SDK's retryer only retries the throttling/5xx class of
error to begin with.

## Spend safety

This page covers only the sender's own configuration. The account-level
circuit breaker (AWS Budgets alerts and the End User Messaging monthly
spend limit override) is covered in
[`docs/aws-guardrails.md`](aws-guardrails.md) — set that up once, before
flipping `NOTIFY_SMS_ENABLED=true` in any environment that can reach real
phone numbers, rather than redoing it here.

## Metrics

Every send attempt increments the `nestova_sms_sends_total` Prometheus
counter, labeled `result` (`sent`, `failed`, `opted_out`) — see
`internal/platform/metrics/sms.go`. This is the fastest way to confirm the
sender is behaving as expected after a configuration change, without
waiting on a real phone.

## The runbook

1. **Provision** the toll-free number and complete its verification, and
   create a send-only IAM credential (above).
2. **Set up spend safety** per
   [`docs/aws-guardrails.md`](aws-guardrails.md), if not already done for
   this account.
3. **Configure**: set `NOTIFY_SMS_ENABLED=true`, `SMS_ORIGINATION_IDENTITY`,
   `SMS_REGION`, and the rest of the table above in the environment.
4. **Restart `cmd/server`** and confirm the boot log reports `sms sender
   configured provider=aws_end_user_messaging` (not `provider=noop`).
5. **Watch `nestova_sms_sends_total`** on the first real send once NES-139
   starts routing notifications to the SMS channel.
