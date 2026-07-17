# AWS spend guardrails (NES-144)

Layered spend guardrails so the AWS footprint (S3 photos/backups, SMS, SES,
CloudWatch) runs unattended safely. Two mechanisms with different jobs:

- **AWS Budgets** — email alerts. Lag up to ~24h; they warn, they never stop
  anything.
- **End User Messaging SMS spend quota** — the hard stop. SMS is the only
  usage-priced service here that a dispatch bug can run away with; past the
  quota, sends fail.

Every command below targets account `768962091675` and is idempotent to
re-run on an account rebuild. Alert emails go to real inboxes, never through
the app's own notification system — the guardrail must work when the app is
the thing misbehaving.

> **TODO(second parent):** every `Subscribers` list below currently has one
> address. Add the second parent's email to both budgets when known:
> re-run the `create-notification` commands with the extra
> `--subscribers` entry, or use
> `aws budgets create-subscriber --account-id 768962091675 --budget-name <name> --notification <same-json> --subscriber SubscriptionType=EMAIL,Address=<second-parent>`.

## Required IAM permissions

The `claude-code-server-admin` IAM user does not carry these by default.
Attach this policy (console: IAM → Users → claude-code-server-admin →
Add permissions → Create inline policy → JSON) before running the commands:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BudgetGuardrails",
      "Effect": "Allow",
      "Action": ["budgets:ViewBudget", "budgets:ModifyBudget"],
      "Resource": "arn:aws:budgets::768962091675:budget/*"
    },
    {
      "Sid": "SMSSpendQuota",
      "Effect": "Allow",
      "Action": [
        "sms-voice:DescribeSpendLimits",
        "sms-voice:SetTextMessageSpendLimitOverride"
      ],
      "Resource": "*"
    }
  ]
}
```

Free Tier usage alerts and billing-access restriction (below) are
console-only root/owner actions and never need this policy.

## Budget 1: monthly cost budget ($10)

Steady state is $3–6/mo, so $5 actual spend means something changed.
Alerts at 50% actual, 80% forecast, 100% actual.

```bash
aws budgets create-budget --account-id 768962091675 --budget '{
  "BudgetName": "nestova-monthly-cost",
  "BudgetLimit": {"Amount": "10", "Unit": "USD"},
  "TimeUnit": "MONTHLY",
  "BudgetType": "COST"
}'

for spec in "ACTUAL 50" "FORECASTED 80" "ACTUAL 100"; do
  set -- $spec
  aws budgets create-notification --account-id 768962091675 \
    --budget-name nestova-monthly-cost \
    --notification "NotificationType=$1,ComparisonOperator=GREATER_THAN,Threshold=$2,ThresholdType=PERCENTAGE" \
    --subscribers "SubscriptionType=EMAIL,Address=esfisher@gmail.com"
done
```

## Budget 2: SMS service budget ($5)

Service-filtered on End User Messaging — the one service that can run away
via a dispatch bug.

```bash
aws budgets create-budget --account-id 768962091675 --budget '{
  "BudgetName": "nestova-sms-spend",
  "BudgetLimit": {"Amount": "5", "Unit": "USD"},
  "TimeUnit": "MONTHLY",
  "BudgetType": "COST",
  "CostFilters": {"Service": ["AWS End User Messaging"]}
}'

aws budgets create-notification --account-id 768962091675 \
  --budget-name nestova-sms-spend \
  --notification "NotificationType=ACTUAL,ComparisonOperator=GREATER_THAN,Threshold=80,ThresholdType=PERCENTAGE" \
  --subscribers "SubscriptionType=EMAIL,Address=esfisher@gmail.com"
```

## SMS spend quota: the circuit breaker

Budgets only alert, and lag up to a day. The End User Messaging monthly SMS
spend limit is enforced at send time: past it, sends fail. The account
default is $1/mo; raise it deliberately to $10 so legitimate family traffic
never hits it while a runaway loop still gets cut off the same day:

```bash
aws pinpoint-sms-voice-v2 set-text-message-spend-limit-override --monthly-limit 10
aws pinpoint-sms-voice-v2 describe-spend-limits
```

`describe-spend-limits` must show `EnforcedLimit: 10` (the override) for
`TEXT_MESSAGES_MONTHLY_SPEND_LIMIT`.

## Console-only steps (root/owner)

1. **Free Tier usage alerts**: Billing preferences → Alert preferences →
   enable "Receive Free Tier usage alerts".
2. **Restrict billing/IAM access**: keep billing and IAM console access
   limited to the root/parent user; household members and service users
   get neither.

## Verification

```bash
aws budgets describe-budgets --account-id 768962091675 \
  --query 'Budgets[].{Name:BudgetName,Limit:BudgetLimit.Amount}' --output table
aws pinpoint-sms-voice-v2 describe-spend-limits --output table
```

Then simulate a breach: temporarily lower `nestova-monthly-cost` to an
amount under current month-to-date spend
(`aws budgets update-budget ... "Amount": "0.01"`), wait for the alert
email (arrives within the hour for actual-threshold breaches), and restore
the $10 limit. Both parents' inboxes should receive it once the TODO above
is done.
