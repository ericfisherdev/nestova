# CloudWatch dead-man heartbeat and alarms (NES-143)

Detect a dead or unreachable appliance from OUTSIDE its own failure
domain: the Pi pushes a heartbeat metric to CloudWatch every five
minutes, and a CloudWatch alarm on missing data emails the parents via
SNS. Metrics are pushed outbound (`PutMetricData`), so the appliance's
LAN/Tailscale-only hosting is irrelevant — CloudWatch never needs to
reach the Pi. The heartbeat is gated on the app's own `/readyz` probe
(`internal/platform/httpserver/server.go`, backed by `db.Health` via
`cmd/server`'s readiness func), so one alarm covers power, OS, app,
database, and outbound-internet failure alike: if ANY of them is down,
the metric simply stops arriving. What it deliberately does NOT cover:
a Pi that is healthy and internet-reachable but has lost only its
LAN or Tailscale ingress keeps heartbeating — see the accepted gap
under the heartbeat script. Everything here fits CloudWatch's and
SNS's always-free allowances — $0/mo (verification and caveats below).

No application code changes: `/readyz` already exists and is exactly
the DB-connectivity check this ticket needs (`/healthz` stays
deliberately liveness-only). Like `docs/kiosk.md` and
`docs/aws-backups.md`, the script and units below are embedded as code
blocks; copy them onto the Pi at the paths in each block's first line.

Shell variables used throughout: `$MONITOR_REGION` (the Region the
metrics, alarms, and SNS topic live in — use one Region for all
three), `$ALERT_EMAIL_1` / `$ALERT_EMAIL_2` (the parents' addresses),
and `$AWS_ACCOUNT_ID` (`768962091675`, per `docs/aws-guardrails.md`).

## Metric namespaces

- `Nestova/Appliance` — `Heartbeat` (this page) and the optional
  `DiskFreePercent`.
- `Nestova/Backups` — `BackupAgeHours`, published by the backup script
  (`docs/aws-backups.md`); this page only alarms on it.

Namespaces are per-purpose on purpose: the IAM condition below scopes
the appliance credential to exactly these two, and nothing else shares
them. (As in `docs/aws-backups.md`: the condition fences the
NAMESPACE only — within it, the credential can publish any metric name
or dimensions it likes; dedicating the namespaces is what keeps that
acceptable. Residual spend risk, same shape as the backup bucket's: a
compromised appliance could mint arbitrary metric SERIES inside these
namespaces and burn past the 10-free-metrics allowance at $0.30/metric
— bounded and surfaced by the account-wide $10/mo cost budget,
`docs/aws-guardrails.md` Budget 1, the same accepted alert-only trade.)

## Required IAM permissions

The shared appliance policy already carries a `BackupHeartbeat` Sid
(`docs/aws-backups.md`) conditioned on the `Nestova/Backups` namespace.
Replace that statement with this widened one (same action, condition
becomes a two-namespace list — `cloudwatch:PutMetricData` still has no
resource-level scoping, so the namespace condition remains the whole
fence):

```json
{
  "Sid": "ApplianceMetrics",
  "Effect": "Allow",
  "Action": ["cloudwatch:PutMetricData"],
  "Resource": "*",
  "Condition": {
    "StringEquals": {
      "cloudwatch:namespace": ["Nestova/Appliance", "Nestova/Backups"]
    }
  }
}
```

The appliance gets NO SNS or CloudWatch-alarm permissions: alarms and
the topic are provisioned once from an operator credential, and a
compromised appliance must not be able to silence, delete, or
reconfigure the thing that reports its own death. (It can still fake a
healthy heartbeat — the same client-signal trust boundary
`docs/aws-backups.md` documents for `BackupAgeHours`; the dead-man
alarm detects a DEAD appliance, not a lying one.)

## SNS topic

One-time, from an operator credential:

```bash
#!/usr/bin/env bash
# Save as provision-monitoring-sns.sh and run with bash.
set -euo pipefail
: "${MONITOR_REGION:?set MONITOR_REGION}"
: "${ALERT_EMAIL_1:?set ALERT_EMAIL_1}"
export AWS_PAGER=""

TOPIC_ARN="$(aws sns create-topic --name nestova-appliance-alerts \
  --region "$MONITOR_REGION" --query TopicArn --output text)"
echo "topic: $TOPIC_ARN"
# Persist for the alarms script (a separate shell — an exported var
# would die with this one): it sources this file when TOPIC_ARN isn't
# already set.
printf 'TOPIC_ARN=%s\n' "$TOPIC_ARN" > ./nestova-monitoring-topic.env

aws sns subscribe --topic-arn "$TOPIC_ARN" --protocol email \
  --notification-endpoint "$ALERT_EMAIL_1" --region "$MONITOR_REGION"
if [ -n "${ALERT_EMAIL_2:-}" ]; then
  aws sns subscribe --topic-arn "$TOPIC_ARN" --protocol email \
    --notification-endpoint "$ALERT_EMAIL_2" --region "$MONITOR_REGION"
else
  echo "TODO(second parent): subscribe the second parent's email when known" >&2
fi
```

Each address receives a confirmation email that must be clicked before
SNS will deliver to it — an unconfirmed subscription silently drops
notifications, so verify both show `Confirmed` before trusting the
alarm path:

```bash
aws sns list-subscriptions-by-topic --topic-arn "$TOPIC_ARN" \
  --region "$MONITOR_REGION" \
  --query 'Subscriptions[].{Endpoint:Endpoint,Arn:SubscriptionArn}' --output table
# "PendingConfirmation" instead of an ARN = not confirmed yet.
```

> **Guardrail status: incomplete (single-recipient)** while
> `ALERT_EMAIL_2` is unknown — the same second-parent TODO carried in
> `docs/aws-guardrails.md`. Ship with one subscriber; add the second
> with the same `sns subscribe` command when known. A single point of
> failure in the alerting path defeats a dead-man alarm's purpose, so
> the TODO is a real one.

## The heartbeat script

```bash
#!/usr/bin/env bash
# /usr/local/bin/nestova-heartbeat.sh — every-5-minutes dead-man
# heartbeat (NES-143).
set -euo pipefail

: "${MONITOR_REGION:?set MONITOR_REGION}"
: "${APP_PORT:?set APP_PORT (the PORT cmd/server listens on, e.g. 8080)}"
export AWS_PAGER=""

# Gate on /readyz — 200 only when the app is up AND its database
# answers (handleReadyz + db.Health). The status is compared to 200
# EXACTLY (curl's -f alone would still accept a 204 or a redirect),
# with a hard timeout so a wedged app can't hang the timer unit. If
# this gate fails, set -e aborts the script and NO metric is published
# — which is exactly the signal: the alarm below treats missing data
# as breaching.
HTTP_STATUS="$(curl -sS --max-time 10 -o /dev/null \
  -w '%{http_code}' "http://localhost:${APP_PORT}/readyz")"
[ "$HTTP_STATUS" = "200" ]

aws cloudwatch put-metric-data --region "$MONITOR_REGION" \
  --namespace Nestova/Appliance \
  --metric-name Heartbeat \
  --unit Count --value 1

# Optional but cheap while we're here: free % on the data volume, for
# the disk alarm below. DATA_MOUNT is REQUIRED (a silent fallback to /
# would "monitor" the root filesystem and miss the actual data volume
# filling up — set it to the mount Postgres and media actually live
# on). Failure of this whole stanza must NOT fail the heartbeat
# already sent — under set -e that means guarding the df pipeline
# itself, not just the aws call.
: "${DATA_MOUNT:?set DATA_MOUNT to the appliance data volume mount point}"
# df's pcent column is USED percent; the metric publishes free.
USED_PCT="$(df --output=pcent "$DATA_MOUNT" 2>/dev/null | tail -1 | tr -dc '0-9' || true)"
if [ -n "$USED_PCT" ]; then
  aws cloudwatch put-metric-data --region "$MONITOR_REGION" \
    --namespace Nestova/Appliance \
    --metric-name DiskFreePercent \
    --unit Percent --value "$((100 - USED_PCT))" || true
else
  echo "df on $DATA_MOUNT produced nothing — DiskFreePercent not published this run" >&2
fi
```

Accepted gap, stated explicitly: a Pi that is healthy on the LAN but
has lost only its Tailscale connectivity keeps heartbeating — that
failure mode is undetected by design (the household is home; the app
still works on the LAN). Revisit only if remote access ever becomes
the primary usage mode.

## systemd oneshot + timer

```ini
# /etc/systemd/system/nestova-heartbeat.service
[Unit]
Description=Nestova dead-man heartbeat to CloudWatch
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=nestova
EnvironmentFile=/etc/nestova/heartbeat.env
ExecStart=/usr/local/bin/nestova-heartbeat.sh
# oneshot disables the default start timeout; a heartbeat that can't
# finish in a minute is a failed heartbeat.
TimeoutStartSec=1min
```

```ini
# /etc/systemd/system/nestova-heartbeat.timer
[Unit]
Description=Run nestova-heartbeat every 5 minutes

[Timer]
OnCalendar=*:0/5
# No Persistent=true: a missed heartbeat is INFORMATION here (that is
# the whole dead-man design), not work to make up after boot — the
# next 5-minute tick reports the truth.

[Install]
WantedBy=timers.target
```

`/etc/nestova/heartbeat.env` holds `MONITOR_REGION`, `APP_PORT`,
`DATA_MOUNT` (the data-volume mount point — required), and the AWS
credentials (or rely on the default credential chain) — `chmod 600`,
owned by the service user.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now nestova-heartbeat.timer
systemctl list-timers nestova-heartbeat.timer
sudo systemctl start nestova-heartbeat.service   # one immediate smoke test
```

## Alarms

One-time, from an operator credential (`$TOPIC_ARN` from the SNS step):

```bash
#!/usr/bin/env bash
# Save as provision-monitoring-alarms.sh and run with bash.
set -euo pipefail
: "${MONITOR_REGION:?set MONITOR_REGION}"
# The topic ARN comes from the SNS script's env file (same directory)
# unless already exported — the two scripts run in separate shells, so
# nothing carries over implicitly. The file is PARSED, never sourced:
# sourcing would execute whatever a tampered file contains, and the
# ARN-shape check refuses anything that isn't an SNS ARN.
if [ -z "${TOPIC_ARN:-}" ] && [ -f ./nestova-monitoring-topic.env ]; then
  TOPIC_ARN="$(grep -m1 '^TOPIC_ARN=' ./nestova-monitoring-topic.env | cut -d= -f2-)"
fi
: "${TOPIC_ARN:?set TOPIC_ARN (or run provision-monitoring-sns.sh in this directory first)}"
case "$TOPIC_ARN" in
  arn:aws:sns:*) ;;
  *) echo "refusing TOPIC_ARN that is not an SNS ARN: '$TOPIC_ARN'" >&2; exit 1 ;;
esac
export AWS_PAGER=""

# 1. Dead-man heartbeat: healthy = one sample per 5-minute period.
#    SampleCount < 1 over three consecutive periods = ALARM, and —
#    load-bearing — missing data IS breaching: a dead Pi publishes
#    nothing at all, which without TreatMissingData=breaching would
#    read as "insufficient data" forever instead of an alarm. Three
#    periods so a single blip — one failed/skipped heartbeat — never
#    alarms on its own. Expected (not guaranteed) alarm latency is
#    ~15–20min after death: CloudWatch's evaluation may briefly reach
#    back to older real datapoints before missing-data treatment kicks
#    in, so treat ~20min as the typical observed bound, not a contract.
#    OK actions on: a self-healed blip should read as resolved in the
#    same inbox that saw it fail.
aws cloudwatch put-metric-alarm --region "$MONITOR_REGION" \
  --alarm-name nestova-appliance-heartbeat \
  --alarm-description "Appliance dead-man: no /readyz-gated heartbeat for 3x5min" \
  --namespace Nestova/Appliance --metric-name Heartbeat \
  --statistic SampleCount --period 300 \
  --evaluation-periods 3 --threshold 1 \
  --comparison-operator LessThanThreshold \
  --treat-missing-data breaching \
  --alarm-actions "$TOPIC_ARN" --ok-actions "$TOPIC_ARN"

# 2. Backup staleness: docs/aws-backups.md's script publishes
#    BackupAgeHours=0 ONLY after a fully successful nightly backup and
#    publishes NOTHING on failure — staleness is therefore ABSENCE of
#    data, not a high value, so this alarm is missing-data-shaped too
#    (the roadmap's original ">30h value threshold" idea predates that
#    success-only metric design and would never fire: the value is
#    always 0). Shape: ten consecutive 3-hour periods each containing
#    zero samples = no successful backup for 30 hours = ALARM. Healthy
#    nightly cadence leaves at most ~9 empty periods (~27h) between
#    samples, so it never alarms; one missed night crosses 30h and
#    does. (Periods of >= 1 hour may evaluate up to 7 days in total —
#    the one-day evaluation ceiling applies only to sub-hourly
#    periods.)
aws cloudwatch put-metric-alarm --region "$MONITOR_REGION" \
  --alarm-name nestova-backup-staleness \
  --alarm-description "Nightly Postgres backup missing: no BackupAgeHours sample for 30h" \
  --namespace Nestova/Backups --metric-name BackupAgeHours \
  --statistic SampleCount --period 10800 \
  --evaluation-periods 10 --threshold 1 \
  --comparison-operator LessThanThreshold \
  --treat-missing-data breaching \
  --alarm-actions "$TOPIC_ARN" --ok-actions "$TOPIC_ARN"

# 3. Data volume filling up: below 15% free for two consecutive
#    heartbeats. Missing data is NOT breaching here — if the metric is
#    absent the appliance is dead, and that is alarm 1's job, not a
#    disk problem.
aws cloudwatch put-metric-alarm --region "$MONITOR_REGION" \
  --alarm-name nestova-appliance-disk-free \
  --alarm-description "Appliance data volume below 15% free" \
  --namespace Nestova/Appliance --metric-name DiskFreePercent \
  --statistic Minimum --period 300 \
  --evaluation-periods 2 --threshold 15 \
  --comparison-operator LessThanThreshold \
  --treat-missing-data notBreaching \
  --alarm-actions "$TOPIC_ARN" --ok-actions "$TOPIC_ARN"

aws cloudwatch describe-alarms --region "$MONITOR_REGION" \
  --alarm-name-prefix nestova- \
  --query 'MetricAlarms[].{Name:AlarmName,State:StateValue}' --output table
```

Right after provisioning, BOTH missing-data alarms can go straight to
ALARM before the first sample ever lands —
`TreatMissingData=breaching` makes an empty evaluation window a breach
whether the metric is dead or merely brand-new. Expected, not broken:
`nestova-appliance-heartbeat` settles to OK within minutes of the
timer's first run (sending the corresponding OK email), and
`nestova-backup-staleness` after the first successful nightly backup.
Provision the heartbeat timer before the alarms if the spurious
initial ALARM email is unwanted.

## Free-tier check (documented at setup, per AC)

Always-free allowances vs. this page's usage:

| Resource | Always-free allowance | Used here |
|---|---|---|
| Custom metrics | 10 | 3 (`Heartbeat`, `DiskFreePercent`, `BackupAgeHours`) |
| Alarms (standard resolution) | 10 | 3 |
| `PutMetricData` API calls | 1M/mo | ~17.3k/mo (2 calls × 288 runs/day × 30 days + 1 nightly × 30) |
| SNS email notifications | 1,000/mo | a handful (state transitions only) |

Verify nothing else in the account is already consuming the metric and
alarm allowances: `aws cloudwatch list-metrics --region
"$MONITOR_REGION"` and `describe-alarms` should show only Nestova's —
in THIS Region; the allowances are account-wide, so if any other
Region ever hosts CloudWatch resources, check there too. Steady-state
cost: $0/mo **assuming the always-free allowances are otherwise
unused, as above**; confirm on the first month's Cost Explorer /
billing page rather than assuming, and note alarm/metric growth is
bounded by this page (no per-request or per-member scaling anywhere).

## Verifying the acceptance criteria

1. **App death → ALARM (typically within ~15–25min)**: `sudo systemctl
   stop nestova` (or stop Postgres) — `/readyz` fails, heartbeats
   stop, the 3×5min window breaches, and every CONFIRMED subscriber
   (both parents once the second-parent TODO is closed; until then the
   one configured address) gets the ALARM email. Don't treat any fixed
   minute-count as the pass bar: CloudWatch can evaluate a wider range
   than the configured periods and hold OK while older real datapoints
   age out — poll `describe-alarms` until the state flips and record
   the observed latency instead. Restart and confirm the OK email
   follows.
2. **Whole-Pi death**: pull power; same alarm, same window (nothing is
   published at all — `TreatMissingData=breaching` is what makes this
   identical to app death).
3. **One blip doesn't alarm**: `sudo systemctl stop
   nestova-heartbeat.timer`, wait ~6 minutes, restart it — one missed
   sample, no email (three consecutive misses are required).
4. **Backup staleness alarms independently**: disable
   `nestova-backup.timer` for a night (test env!) — the staleness
   alarm fires with the heartbeat alarm silent.
