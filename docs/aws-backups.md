# Postgres backups to S3 (NES-142)

Nightly `pg_dump` of the appliance database, streamed to S3. Without this,
the Pi's SD card holds the only copy of the family's data — the
highest-value ops item in the AWS batch, at roughly $0.05–0.30/mo. This
page is the operator runbook: bucket provisioning, IAM, the backup and
restore scripts, the systemd timer that runs the backup, and the quarterly
restore drill. An untested backup is a hope, not a backup — the restore
path here is scripted and periodically exercised, not aspirational.

Everything below is deliberately **outside the app process** — a plain
shell script under systemd, not Go code — so backups keep running when the
app itself is the thing that broke. There are no application code changes
in this ticket.

Like `docs/kiosk.md`, the units and scripts below are embedded here as
code blocks (there is no `deploy/` directory in this repo); copy them onto
the Pi at the paths given in each block's first line.

Shell variables used throughout (set them before running any command):
`$BACKUP_BUCKET` (the dedicated backups bucket — see below) and
`$BACKUP_REGION` (the Region the bucket lives in). Database connection
settings reach the scripts as standard libpq environment variables
(`PGHOST`/`PGPORT`/`PGUSER`/`PGPASSWORD`/`PGDATABASE`) rather than a
`DATABASE_URL` argument — a connection URL on a command line would put
the database password in the process's argv, visible to `ps`; libpq env
vars are readable only by the same user and root. `$BACKUP_BUCKET` is
intentionally NOT the photo bucket (`S3_BUCKET`, `docs/storage.md`):
backups get lifecycle rules (Glacier transition, ~400-day expiry) that
must never collide with photo retention.

## Provisioning the bucket

One-time, from an admin credential (never the appliance's). The
provisioning commands need the `aws` CLI and `jq` (the lifecycle
script's read-back assertion) on the machine they run from:

```bash
#!/usr/bin/env bash
# Save as provision-backup-bucket.sh and run with bash — as a script,
# like the lifecycle one below, so a failed step (a typo'd bucket, a
# failed create) aborts the whole sequence instead of letting later
# commands partially mutate whatever bucket the name happened to hit.
set -euo pipefail
: "${BACKUP_BUCKET:?set BACKUP_BUCKET}"
: "${BACKUP_REGION:?set BACKUP_REGION}"

# Never point any of this at the photo bucket: everything below mutates
# whatever $BACKUP_BUCKET names, and lifecycle rules meant for backups
# would destroy photo retention (docs/storage.md). S3_BUCKET must be
# SET for the guard to mean anything (an unset value would silently
# skip the comparison) — export the real photo bucket name, or the
# literal "none" on a deployment whose photos use the local backend.
: "${S3_BUCKET:?set S3_BUCKET to the photo bucket name (or the literal \"none\")}"
if [ "$BACKUP_BUCKET" = "$S3_BUCKET" ]; then
  echo "BACKUP_BUCKET must not be the photo bucket ($S3_BUCKET)" >&2
  exit 1
fi

# us-east-1 is S3's API quirk: it must be created WITHOUT a
# LocationConstraint (S3 rejects LocationConstraint=us-east-1).
if [ "$BACKUP_REGION" = "us-east-1" ]; then
  aws s3api create-bucket --bucket "$BACKUP_BUCKET" --region "$BACKUP_REGION"
else
  aws s3api create-bucket --bucket "$BACKUP_BUCKET" --region "$BACKUP_REGION" \
    --create-bucket-configuration LocationConstraint="$BACKUP_REGION"
fi

# Versioning ON: an overwrite of an existing object preserves the prior
# version as a noncurrent version. The appliance credential has no
# s3:DeleteObject (see IAM below) and lifecycle keeps noncurrent
# versions for the full 400-day retention (below), so even a same-key
# PutObject from a compromised appliance cannot erase an existing
# backup within the retention window (restoring a prior version is an
# admin-credential action).
aws s3api put-bucket-versioning --bucket "$BACKUP_BUCKET" \
  --region "$BACKUP_REGION" --versioning-configuration Status=Enabled

# First-time versioning can take up to ~15 minutes to fully propagate.
# Before trusting the first backup, confirm status AND that a real
# object actually received a version ID (an unversioned-era object
# reports VersionId "null"):
aws s3api get-bucket-versioning --bucket "$BACKUP_BUCKET" \
  --region "$BACKUP_REGION"          # must report Status: Enabled
# ...then after the first backup upload (use the exact key from that
# upload's `backup ok:` log line — never reconstructed from today's
# date, which differs after a boot catch-up or a later-day check):
#   aws s3api head-object --bucket "$BACKUP_BUCKET" --region "$BACKUP_REGION" \
#     --key "<key-from-backup-ok-log>" --query VersionId   # must NOT be "null"

# Block all public access, unconditionally.
aws s3api put-public-access-block --bucket "$BACKUP_BUCKET" \
  --region "$BACKUP_REGION" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true

# ...and read it back — provisioning isn't done until all four report true:
aws s3api get-public-access-block --bucket "$BACKUP_BUCKET" \
  --region "$BACKUP_REGION" --query 'PublicAccessBlockConfiguration'
```

SSE-S3 encryption at rest needs no configuration: S3 has applied it by
default to every new object in every bucket since January 2023. Verify
it at the object level after the first backup lands — bucket-level
`get-bucket-encryption` only reports an *explicitly configured* policy
and can come back empty on a bucket relying on the default, so ask a
real object instead:

```bash
# <key-from-backup-ok-log> = the exact key printed by that upload's own
# `backup ok:` log line — never reconstructed from today's date, which
# differs after a boot catch-up run or a later-day check.
aws s3api head-object --bucket "$BACKUP_BUCKET" --region "$BACKUP_REGION" \
  --key "<key-from-backup-ok-log>" --query ServerSideEncryption
# expected: "AES256"
```

(Every bucket-level command on this page passes `--region` explicitly so
it targets the bucket's own Region, not whatever the operator's CLI
default happens to be.)

### Lifecycle: Glacier at 30 days, expire at 400

Dumps land as `STANDARD_IA` (the script below sets it per-object at
upload), transition to Glacier Instant Retrieval at 30 days, and leave
the current view at 400 days — long enough to reach back more than a
full year, on lifecycle rules the appliance credential cannot touch.
Noncurrent (overwritten) versions get the SAME 400-day retention as
current ones, not a shorter one — they are the recovery path if a
compromised appliance overwrites a key, so expiring them early would
hand the attacker a deletion primitive the IAM policy deliberately
withholds; they transition to Glacier IR at 30 days to keep their
carrying cost negligible.

One versioning consequence, stated honestly: on a versioned bucket,
`Expiration` at day 400 does not destroy the dump — it stamps a delete
marker, and the dump itself becomes a noncurrent version that
`NoncurrentVersionExpiration` finally removes up to 400 days later.
(All of these day counts are *eligibility* thresholds, not deadlines —
S3 runs lifecycle actions asynchronously and can lag them by days.) A
dump's data therefore persists for **up to roughly ~800 days**, the
tail of it in Glacier IR at fractions of a cent. That tail is the price
of the security property above (lifecycle is the only AUTOMATED,
appliance-reachable destruction path — an admin credential can still
permanently delete versions, which is exactly why provisioning runs
from operator credentials the appliance never holds, and restores from
a read-only credential (IAM below) — and
noncurrent retention must stay long); the "400
days" figure is when a dump stops being the restorable current version,
not when its bytes vanish. Once a dump's payload versions are all gone,
its delete marker remains only as a zero-payload tombstone (billed as
key-name metadata in S3 Standard — negligible, but not literally zero).
S3 may clean such "expired object delete markers" up on its own as part
of lifecycle processing; if any linger and the versioned-listing
clutter ever matters, an explicit `ExpiredObjectDeleteMarker` action —
which cannot share a rule with a `Days`-based `Expiration`, so it would
be a second, marker-only rule — is the deliberate cleanup, and at one
tombstone per day it is deliberately left out of this runbook's scope.

`AbortIncompleteMultipartUpload` cleans up parts left behind by an upload
that died mid-stream — `Expiration` and version expiry never touch those,
and they bill until aborted.

`put-bucket-lifecycle-configuration` REPLACES the bucket's entire
lifecycle configuration — it does not merge. On this dedicated backups
bucket the preflight below normally reports no configuration (that is
the expected state on first provisioning); if it ever shows rules other
than `nestova-backup-retention`, stop and fold them into the JSON here
rather than silently wiping them:

```bash
#!/usr/bin/env bash
# Save as provision-backup-lifecycle.sh and run with bash — as a script
# (not pasted commands) so its exit codes are real: any outcome other
# than "rule applied and verified" exits nonzero, and a not-applied
# outcome can never be mistaken for completed provisioning.
#
# This script is the SINGLE OWNER of this bucket's lifecycle
# configuration — never edit lifecycle rules for this bucket in the
# console or elsewhere. put-bucket-lifecycle-configuration replaces the
# whole configuration, so the preflight-then-put below is only
# race-free while nothing else writes it concurrently; single ownership
# is what makes that hold.
set -euo pipefail
: "${BACKUP_BUCKET:?set BACKUP_BUCKET}"
: "${BACKUP_REGION:?set BACKUP_REGION}"
# Same photo-bucket guard as bucket provisioning (S3_BUCKET required —
# the real photo bucket name, or the literal "none" for a local-photo
# deployment — so the clash comparison can never be silently skipped):
# these lifecycle rules applied to the photo bucket would destroy photo
# retention.
: "${S3_BUCKET:?set S3_BUCKET to the photo bucket name (or the literal \"none\")}"
if [ "$BACKUP_BUCKET" = "$S3_BUCKET" ]; then
  echo "BACKUP_BUCKET must not be the photo bucket ($S3_BUCKET)" >&2
  exit 1
fi

# Fail-closed preflight: ONLY the specific "no configuration exists"
# error clears the way. An existing configuration means STOP AND MERGE;
# any other failure (auth, wrong region, typo'd bucket) means the
# check itself didn't run — never fall through to a replace on that.
if OUT="$(aws s3api get-bucket-lifecycle-configuration \
  --bucket "$BACKUP_BUCKET" --region "$BACKUP_REGION" 2>&1)"; then
  echo "existing lifecycle rules — merge them into this script's JSON before applying:" >&2
  echo "$OUT" >&2
  exit 1
elif [ "${OUT#*NoSuchLifecycleConfiguration}" != "$OUT" ]; then
  aws s3api put-bucket-lifecycle-configuration --bucket "$BACKUP_BUCKET" \
    --region "$BACKUP_REGION" --lifecycle-configuration '{
  "Rules": [
    {
      "ID": "nestova-backup-retention",
      "Status": "Enabled",
      "Filter": {"Prefix": "backups/"},
      "Transitions": [{"Days": 30, "StorageClass": "GLACIER_IR"}],
      "Expiration": {"Days": 400},
      "NoncurrentVersionTransitions": [{"NoncurrentDays": 30, "StorageClass": "GLACIER_IR"}],
      "NoncurrentVersionExpiration": {"NoncurrentDays": 400},
      "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 7}
    }
  ]
}' || { echo "lifecycle configuration failed" >&2; exit 1; }
  # "Applied" is only believed after reading it back: assert the rule
  # actually stored with the retention numbers this runbook promises.
  aws s3api get-bucket-lifecycle-configuration --bucket "$BACKUP_BUCKET" \
    --region "$BACKUP_REGION" --output json \
    | jq -e '.Rules[] | select(.ID == "nestova-backup-retention")
        | (.Status == "Enabled")
          and (.Filter.Prefix == "backups/")
          and (.Transitions[0].Days == 30)
          and (.Transitions[0].StorageClass == "GLACIER_IR")
          and (.Expiration.Days == 400)
          and (.NoncurrentVersionTransitions[0].NoncurrentDays == 30)
          and (.NoncurrentVersionTransitions[0].StorageClass == "GLACIER_IR")
          and (.NoncurrentVersionExpiration.NoncurrentDays == 400)
          and (.AbortIncompleteMultipartUpload.DaysAfterInitiation == 7)' \
        > /dev/null \
    || { echo "lifecycle rule read-back did not match expected settings" >&2; exit 1; }
  echo "lifecycle rule applied and verified"
else
  echo "preflight failed, not applying: $OUT" >&2
  exit 1
fi
```

Verify: `aws s3api get-bucket-lifecycle-configuration --bucket
"$BACKUP_BUCKET" --region "$BACKUP_REGION"` shows the rule, and the S3
console's Management tab lists it as enabled.

## Required IAM permissions

Extend the shared appliance policy (the `Sid` pattern from
`docs/aws-guardrails.md`, alongside `SMSSend`/`SESSend`) with two
statements. The appliance can **write new backups and nothing else**:
explicitly NO `s3:DeleteObject`, no lifecycle/versioning actions, and no
read-back of existing dumps — a compromised appliance cannot destroy or
exfiltrate history. Expiry is handled server-side by the lifecycle rule
above; restores run from the read-only restore credential below, not the Pi's.

```json
{
  "Sid": "BackupWrite",
  "Effect": "Allow",
  "Action": ["s3:PutObject", "s3:AbortMultipartUpload"],
  "Resource": "arn:aws:s3:::<BACKUP_BUCKET>/backups/*"
},
{
  "Sid": "BackupHeartbeat",
  "Effect": "Allow",
  "Action": ["cloudwatch:PutMetricData"],
  "Resource": "*",
  "Condition": {
    "StringEquals": {"cloudwatch:namespace": "Nestova/Backups"}
  }
}
```

`cloudwatch:PutMetricData` does not support resource-level scoping — the
namespace condition is the standard way to confine it. Precisely: this
is namespace-scoped *publishing* — the credential can publish any metric
name or dimensions it likes within `Nestova/Backups` (a namespace
dedicated to this one purpose so nothing else shares it), and nothing in
any other namespace.

`s3:AbortMultipartUpload` is included alongside `PutObject` because the
streamed upload is a multipart upload under the hood: it lets the CLI
abort an interrupted upload immediately, freeing the orphaned parts
right away instead of leaving them billable until the seven-day
`AbortIncompleteMultipartUpload` lifecycle sweep. Scope, stated
precisely: the grant covers ANY in-progress multipart upload under
`backups/*`, not only ones this process started — in practice the
appliance is the sole writer there, so the worst abuse is aborting its
own in-flight backup, which the missing heartbeat (below) then reports.
It grants no power over completed objects — aborting applies only to
uploads that never finished, so the no-delete guarantee above is
untouched.

**Residual risk, stated explicitly:** `s3:PutObject` on `backups/*`
necessarily lets the holder write objects of any size and any name under
that prefix — S3 has no per-prefix storage quota, so a compromised
appliance could inflate storage spend by uploading junk. What bounds it:
the Pi's upstream bandwidth makes multi-terabyte abuse slow, versioning
plus no-delete means the abuse is additive (never destroys real
backups), and the account-wide cost budget
(`docs/aws-guardrails.md`, Budget 1) alerts on the spend — alert-only,
with up to ~24h lag, which is the accepted trade for a family appliance.
If NES-143 wants a tighter, backup-specific signal, an alarm on the
bucket's `BucketSizeBytes` CloudWatch storage metric is the AWS-side
control that closes this gap without granting the appliance anything
new.

### The restore credential

Restores don't need (and shouldn't casually reuse) the full admin
credential either: least privilege for everything the restore script
and recovery examples do is **read-only** on this one bucket — list
(current and versions) plus get (current and versions), no write, no
delete, no lifecycle. Attach this to a dedicated restore user/role and
save the admin credential for provisioning:

```json
{
  "Sid": "BackupRestoreRead",
  "Effect": "Allow",
  "Action": ["s3:ListBucket", "s3:ListBucketVersions"],
  "Resource": "arn:aws:s3:::<BACKUP_BUCKET>",
  "Condition": {"StringLike": {"s3:prefix": "backups/*"}}
},
{
  "Sid": "BackupRestoreGet",
  "Effect": "Allow",
  "Action": ["s3:GetObject", "s3:GetObjectVersion"],
  "Resource": "arn:aws:s3:::<BACKUP_BUCKET>/backups/*"
}
```

## The backup script

```bash
#!/usr/bin/env bash
# /usr/local/bin/nestova-backup.sh — nightly pg_dump to S3 (NES-142).
set -euo pipefail

# Connection comes from libpq env vars (PGHOST/PGPORT/PGUSER/PGPASSWORD/
# PGDATABASE, from the EnvironmentFile) — never a URL in argv, which
# would expose the password to `ps`. Host and user are required
# explicitly: left unset, libpq silently falls back to the local socket
# and the current OS user, i.e. possibly the wrong database entirely.
: "${PGDATABASE:?set PGDATABASE}"
: "${PGHOST:?set PGHOST}"
: "${PGPORT:?set PGPORT}"
: "${PGUSER:?set PGUSER}"
# Password-authenticated cluster: a missing credential must fail HERE,
# loudly, not as a nightly auth failure (or a hung prompt on a manual
# run). PGPASSFILE (a .pgpass file) is the supported alternative.
[ -n "${PGPASSWORD:-}" ] || [ -n "${PGPASSFILE:-}" ] \
  || { echo "set PGPASSWORD or PGPASSFILE" >&2; exit 1; }
: "${BACKUP_BUCKET:?set BACKUP_BUCKET}"
: "${BACKUP_REGION:?set BACKUP_REGION}"
# Same routing hygiene as the restore script: a service file or
# PGHOSTADDR inherited from the environment could steer pg_isready and
# pg_dump at a different cluster than the PGHOST just validated. And
# one endpoint only — libpq treats comma-separated host/port values as
# a fallback LIST, so pg_isready could validate one cluster while
# pg_dump lands on another.
unset PGSERVICE PGSERVICEFILE PGHOSTADDR
case "$PGHOST$PGPORT" in *,*)
  echo "refusing multi-host/port PGHOST/PGPORT ('$PGHOST'/'$PGPORT')" >&2; exit 1 ;;
esac

# The standard deployment runs Postgres on the Pi itself (local
# PGHOST). If PGHOST is ever remote, the dump crosses a network — TLS
# is forced to verify-full, UNCONDITIONALLY: an inherited weaker
# PGSSLMODE (prefer/require/disable) must not be able to downgrade a
# remote connection to plaintext or an unverified peer. verify-full
# needs the server's CA available (~/.postgresql/root.crt or
# PGSSLROOTCERT) — set that up rather than weakening the mode.
case "$PGHOST" in
  localhost|127.0.0.1|::1) ;;
  *) export PGSSLMODE=verify-full ;;
esac

# No pager, ever — under systemd there is no tty to page to, and a
# manual run must not hang waiting for `q`.
export AWS_PAGER=""

# A Persistent=true catch-up run fires shortly after boot, and
# network-online.target says nothing about Postgres being ready to
# accept connections yet. A missed timer run is only replayed ONCE, so
# failing that one run to a race costs a whole night's backup — wait
# for the database first, against a wall-clock deadline (a count of
# sleeps would understate the real elapsed time, since each pg_isready
# probe can itself block for its own timeout).
DEADLINE=$((SECONDS + 300))
until pg_isready -q -t 3; do
  if [ "$SECONDS" -ge "$DEADLINE" ]; then
    echo "database not ready after 5 minutes, aborting" >&2
    exit 1
  fi
  sleep 10
done

umask 077
STATE_DIR="$(mktemp -d)"
trap 'rm -rf -- "$STATE_DIR"' EXIT

# One COMPLETE attempt — fresh key, dump, streamed upload, hash, marker
# — as a function whose body is a subshell with its own `set -e`, so an
# internal failure aborts just that attempt and its own trap cleans up.
#
# Per-ATTEMPT key, never reused: a retry can therefore never overwrite
# a good dump (and its completion marker can never vouch for an object
# it didn't accompany). The UTC timestamp keeps names chronologically
# sortable for humans; uniqueness rests on the random nonce, NOT the
# clock or the PID (a corrected clock can move backward and PIDs
# recycle — neither may be a uniqueness guarantee).
#
# Custom-format dump (pg_restore's input format: compressed, supports
# --clean and parallel restore), streamed straight to S3 — the dump
# never needs free space on the SD card. STANDARD_IA per-object:
# backups are written nightly and read almost never. The dump streams
# through tee so a SHA-256 of the exact uploaded bytes is computed in
# the same single pass; the hasher reads from a 0600 FIFO in a private
# temp dir (the plaintext dump passes through it — a default-umask
# rendezvous point would let another local user steal or corrupt the
# stream) as an explicitly managed background job whose completion and
# exit status `wait` makes the attempt's own. The attempt's trap also
# reaps that hasher: on a mid-attempt failure, deleting the FIFO's
# path alone would leave sha256sum blocked on it forever.
#
# The completion marker is written ONLY after the dump pipeline fully
# succeeded, and carries the dump's checksum: the restore script's
# default selection requires the marker (a partial upload that still
# completed as an S3 object is never auto-selected) and verifies the
# downloaded bytes against this checksum before pg_restore ever runs.
run_attempt() (
  set -euo pipefail
  ATTEMPT_ID="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  KEY="backups/$(date -u +%FT%H%M%SZ)-${ATTEMPT_ID}.dump"

  TMP_DIR="$(mktemp -d)"
  SUM_FILE="$TMP_DIR/sum"
  HASH_FIFO="$TMP_DIR/hash.fifo"
  HASHER_PID=""
  cleanup() {
    if [ -n "${HASHER_PID:-}" ]; then
      kill "$HASHER_PID" 2>/dev/null || true
      wait "$HASHER_PID" 2>/dev/null || true
    fi
    rm -rf -- "$TMP_DIR"
  }
  trap cleanup EXIT
  mkfifo -m 600 "$HASH_FIFO"
  # The hasher is a SINGLE process (no pipe into cut — $! after a
  # background pipeline names its last member, so a piped hasher's
  # sha256sum failure would be invisible to `wait`): its exit status
  # is exactly what wait reports.
  sha256sum < "$HASH_FIFO" > "$SUM_FILE.raw" &
  HASHER_PID=$!

  pg_dump --format=custom --compress=6 \
    | tee "$HASH_FIFO" \
    | aws s3 cp - "s3://$BACKUP_BUCKET/$KEY" \
        --region "$BACKUP_REGION" --storage-class STANDARD_IA --expected-size 1073741824

  wait "$HASHER_PID"
  HASHER_PID=""
  cut -d' ' -f1 "$SUM_FILE.raw" > "$SUM_FILE"
  [ -s "$SUM_FILE" ] || { echo "checksum was never produced" >&2; exit 1; }

  printf '%s  %s\n' "$(cat "$SUM_FILE")" "$KEY" \
    | aws s3 cp - "s3://$BACKUP_BUCKET/$KEY.ok" --region "$BACKUP_REGION"

  printf '%s' "$KEY" > "$STATE_DIR/key"
)
# The attempt must be callable by `timeout` below (which cannot run a
# shell function directly): export it, and everything it reads.
export -f run_attempt
export STATE_DIR BACKUP_BUCKET BACKUP_REGION

# Bounded whole-attempt retry: Persistent=true only replays a MISSED
# timer event — it does nothing for a run that fired and failed on a
# transient error (DB hiccup, DNS, S3 throttle). Three attempts, five
# minutes apart, all inside the one systemd run (TimeoutStartSec below
# budgets for this). Retries are safe by construction: per-attempt
# keys mean nothing is ever overwritten.
#
# Each attempt runs under GNU timeout, which places it in its OWN
# process group and, on expiry, signals that whole group — pg_dump,
# tee, and the aws upload all die with it (killing only the wrapper
# shell would orphan them into the next retry), with a KILL escalation
# for anything that ignores TERM. 15 minutes per attempt × 3 attempts
# + 2 × 5-minute retry waits fits the unit's 60-minute budget.
#
# The attempt is invoked with -e suspended around it (NOT inside an
# `if`-condition, where bash would silently ignore the subshell's own
# `set -e` and let a failed pg_dump keep going) so its exit status can
# be examined without disabling its internal aborts. rc=124 is
# timeout's "attempt exceeded its deadline".
MAX_ATTEMPTS=3
for ATTEMPT in 1 2 3; do
  set +e
  timeout --kill-after=30s 15m bash -c run_attempt
  RC=$?
  set -e
  [ "$RC" -eq 0 ] && break
  if [ "$ATTEMPT" -eq "$MAX_ATTEMPTS" ]; then
    echo "backup failed after $MAX_ATTEMPTS attempts" >&2
    exit 1
  fi
  echo "attempt $ATTEMPT failed (rc=$RC) — retrying in 5 minutes" >&2
  sleep 300
done
KEY="$(cat "$STATE_DIR/key")"

# Log the completed backup BEFORE the heartbeat: the dump and marker
# are already durable at this point, and the `backup ok:` line is the
# operator's only record of the exact key — a heartbeat failure must
# not hide it or masquerade as a failed backup.
echo "backup ok: s3://$BACKUP_BUCKET/$KEY"

# Heartbeat for NES-143's dead-man alarm: a fresh backup just succeeded,
# so its age is 0. It only runs after everything above succeeded; on
# any earlier failure the metric is simply not emitted and the alarm's
# missing-data handling fires. Same out-of-band CloudWatch push channel
# as the alarm itself — deliberately NOT the app's Prometheus registry
# (internal/platform/metrics), which dies with the app. A publish
# failure still exits nonzero (systemd records it, and the silent
# heartbeat trips the alarm) — but as its own distinct message, never
# disguised as a backup failure.
if ! aws cloudwatch put-metric-data --region "$BACKUP_REGION" \
  --namespace Nestova/Backups \
  --metric-name BackupAgeHours \
  --unit None --value 0; then
  echo "backup succeeded but heartbeat publication failed" >&2
  exit 1
fi
```

Requires `postgresql` client tools and the `aws` CLI on the Pi (the
restore script below additionally needs `jq` and `psql`). The client
tools must be at least the server's major version — this deployment runs
**Postgres 16**, so `pg_dump --version` / `pg_restore --version` should
report 16.x (or newer, which can read older dumps; an OLDER pg_restore
cannot reliably read a newer pg_dump's archive). Check both at deploy
time and again during each restore drill. The pipeline runs under `set
-o pipefail`, so a `pg_dump` failure fails the whole script even though
it feeds a pipe. `--expected-size` sizes the
multipart upload for a stream whose length S3 cannot know in advance —
it is a part-sizing hint, not a cap, but a stream that outgrows the hint
badly enough runs into S3's 10,000-part limit. 1 GiB of headroom is
orders of magnitude above a family database today and costs nothing if
unused; if the dump ever approaches that (check the object sizes in the
S3 console), bump the hint to a few times the current dump size.

Keys are per-attempt (`<utc-timestamp>-<nonce>.dump`, exactly as printed
by the `backup ok:` log line), never reused: a normal
night produces one, a manual re-run adds another rather than
overwriting, and lifecycle expires them all on the same clock. That
non-reuse is what makes the `.ok` completion marker trustworthy — a
marker can only ever describe the one attempt it was written after, so
a later failed attempt cannot inherit an earlier attempt's marker.

The marker's trust boundary, stated explicitly: both the dump and its
marker are written by the SAME appliance credential, so the marker is a
**transport/storage integrity** signal — it catches partial uploads,
replaced-by-mistake objects, and corruption in flight — and is NOT an
attestation against a *compromised* appliance, which could write a
matching dump/marker pair of its own choosing (the same limitation
already documented for the heartbeat under Observability; a
server-side, appliance-unwritable manifest would require admin-side
machinery out of scope for a family appliance). What the compromised
case keeps is versioning — every prior version survives, recoverable
via the version-id path — and the quarterly drill remains the only real
proof the restored data is good: for a drill run after any suspected
compromise, take the expected digest from an admin-attested marker
VERSION (as in the recovery example below), not from whatever the
current marker happens to say — and inspect the archive's table of
contents first (`pg_restore --list <dump>`) before restoring it
anywhere, since a checksum only proves the bytes are what the marker
said, never that their content is trustworthy.

## systemd oneshot + timer

Nightly at 03:15, with `Persistent=true` so a night the Pi was powered
off is made up at next boot instead of silently skipped:

```ini
# /etc/systemd/system/nestova-backup.service
[Unit]
Description=Nestova nightly Postgres backup to S3
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=nestova
EnvironmentFile=/etc/nestova/backup.env
ExecStart=/usr/local/bin/nestova-backup.sh
# Type=oneshot disables systemd's default start timeout, so a hung
# pg_dump or stalled upload would otherwise stay "activating" forever.
# The budget covers the script's full worst case with headroom — a
# 5-minute DB readiness wait, three 15-minute attempts each with a 30s
# kill-after grace, and two 5-minute retry waits (≈62.5min) — while
# each single attempt at family scale really takes seconds; on expiry
# systemd kills the pipeline, no metric is emitted, and NES-143's
# missing-data alarm reports the failure.
TimeoutStartSec=70min
```

```ini
# /etc/systemd/system/nestova-backup.timer
[Unit]
Description=Run nestova-backup nightly

[Timer]
OnCalendar=*-*-* 03:15:00
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
```

`/etc/nestova/backup.env` holds the libpq connection vars
(`PGHOST`/`PGPORT`/`PGUSER`/`PGPASSWORD`/`PGDATABASE`), `BACKUP_BUCKET`,
`BACKUP_REGION`, and the AWS credentials (or rely on the default
credential chain) — `chmod 600`, owned by the service user, since
`PGPASSWORD` is the database password.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now nestova-backup.timer
systemctl list-timers nestova-backup.timer   # confirm next-run time
sudo systemctl start nestova-backup.service  # fire one immediately as a smoke test
```

## The restore script

Run from the read-only restore credential (IAM above; the appliance's own cannot read dumps —
see IAM above), against a scratch database so a drill never touches the
live one:

```bash
#!/usr/bin/env bash
# /usr/local/bin/nestova-restore.sh — restore an S3 dump (NES-142).
# Usage: nestova-restore.sh [key] [version-id]
#   no args        — newest backups/*.dump (current version)
#   key            — that object's current version
#   key version-id — a specific (e.g. overwritten) version of that key
set -euo pipefail

# Connection via libpq env vars (PGPORT/PGUSER/PGPASSWORD), same
# no-secrets-in-argv rule as the backup script; RESTORE_DATABASE is just
# a database name — NEVER the live database.
: "${BACKUP_BUCKET:?set BACKUP_BUCKET}"
: "${BACKUP_REGION:?set BACKUP_REGION}"
: "${RESTORE_DATABASE:?set RESTORE_DATABASE}"

# Never let the CLI hand output to a pager: interactively that would
# hang the key lookup below waiting for a human to press q.
export AWS_PAGER=""

# Fail closed, on BOTH coordinates of the target. pg_restore below runs
# with --clean, which DROPs objects in the target database before
# recreating them, so:
#   1. The database name is an exact allowlist of one — the drill's
#      scratch name. A suffix check would not be a guard (prod_restore,
#      or a whole connection URI ending in _restore, would pass).
#   2. The HOST must be named deliberately via RESTORE_PGHOST — this
#      script refuses to inherit an ambient PGHOST (which could be the
#      production cluster, where a database that happens to be called
#      nestova_restore would be destroyed) and refuses libpq's silent
#      local-socket fallback.
if [ "$RESTORE_DATABASE" != "nestova_restore" ]; then
  echo "refusing target '$RESTORE_DATABASE': only the scratch DB 'nestova_restore' is allowed" >&2
  exit 1
fi
: "${RESTORE_PGHOST:?set RESTORE_PGHOST to the drill host (deliberately — never inherited)}"
# One endpoint only: libpq treats comma-separated host/port values as a
# fallback LIST and tries each in order — a multi-host value could
# silently drift the drill onto an alternate cluster.
case "$RESTORE_PGHOST" in *,*)
  echo "refusing multi-host RESTORE_PGHOST '$RESTORE_PGHOST'" >&2; exit 1 ;;
esac
# The host coordinate is pinned to the local machine by default: drills
# run against a scratch cluster on the box running this script. Naming
# any OTHER host (which could be the production cluster) requires a
# second, explicit key — both variables together are a deliberate act,
# not an inheritable accident.
case "$RESTORE_PGHOST" in
  localhost|127.0.0.1|::1) ;;
  *)
    if [ "${RESTORE_PGHOST_ALLOW_REMOTE:-}" != "yes" ]; then
      echo "refusing non-local RESTORE_PGHOST '$RESTORE_PGHOST' without RESTORE_PGHOST_ALLOW_REMOTE=yes" >&2
      exit 1
    fi
    # Stated limit of this override: it names A remote host, it does
    # not attest WHICH cluster answers there — an empty nestova_restore
    # on the wrong remote cluster would pass every later gate. The
    # operator setting these two variables IS the identity check.
    # Prefer keeping drills local (the default path); use remote only
    # against a cluster you provisioned for drills.
    # A deliberately remote restore crosses a network: TLS is forced to
    # verify-full exactly as in the backup script — an inherited weaker
    # PGSSLMODE must not be able to downgrade it. Requires the server's
    # CA (~/.postgresql/root.crt or PGSSLROOTCERT).
    export PGSSLMODE=verify-full
    ;;
esac
export PGHOST="$RESTORE_PGHOST"
# PGHOST alone does not fully pin libpq's routing: a service file
# (PGSERVICE/PGSERVICEFILE) or PGHOSTADDR inherited from the ambient
# shell could still steer the connection at a different cluster than
# the host just validated. Clear them so RESTORE_PGHOST is the only
# routing input the destructive commands below ever see.
unset PGSERVICE PGSERVICEFILE PGHOSTADDR
# The port is pinned the same way as the host, and REQUIRED — a
# defaulted 5432 could silently mean a different cluster than the
# nonstandard-port scratch cluster the operator intended.
: "${RESTORE_PGPORT:?set RESTORE_PGPORT explicitly (5432 for a default-port scratch cluster)}"
# A single numeric port, for the same one-endpoint reason as the host.
case "$RESTORE_PGPORT" in ''|*[!0-9]*)
  echo "refusing non-numeric RESTORE_PGPORT '$RESTORE_PGPORT'" >&2; exit 1 ;;
esac
export PGPORT="$RESTORE_PGPORT"
# Same credential fail-fast as the backup script: on a
# password-authenticated cluster, fail here rather than mid-drill.
: "${PGUSER:?set PGUSER}"
[ -n "${PGPASSWORD:-}" ] || [ -n "${PGPASSFILE:-}" ] \
  || { echo "set PGPASSWORD or PGPASSFILE" >&2; exit 1; }

# jq over the aggregated JSON: the CLI applies --query per pagination
# page, so a bucket past 1,000 keys would yield one "newest" key PER
# PAGE and break the single-key contract here. jq sees the fully
# aggregated listing instead. (.Contents // []) covers an empty bucket,
# where Contents is omitted entirely.
#
# Default selection requires the dump's .ok completion marker (written
# by the backup script only after a fully successful upload) — an
# upload that completed as an object but whose pg_dump died mid-stream
# has no marker and is never auto-selected. An EXPLICIT key argument
# bypasses only this marker-based SELECTION (the operator is
# deliberately reaching for a specific object); it does NOT bypass
# integrity verification — every path below still checksum-verifies the
# downloaded bytes before pg_restore runs.
KEY="${1:-$(aws s3api list-objects-v2 --bucket "$BACKUP_BUCKET" \
  --prefix backups/ --region "$BACKUP_REGION" --output json \
  | jq -r '(.Contents // []) as $c | [$c[].Key] as $all
           | [$c[] | select(.Key | endswith(".dump"))
                   | select(.Key + ".ok" as $m | $all | index($m))]
           | sort_by(.LastModified, .Key) | last | .Key // empty')}"
# "Newest" is judged by S3's own LastModified (server-side upload
# time), with the key name only as tiebreaker — the key embeds the
# Pi's clock, which a skewed or rolled-back clock would misorder.
[ -n "$KEY" ] || { echo "no completed backups found" >&2; exit 1; }
# Only ever feed pg_restore an actual dump — guards both the default
# lookup and an explicitly passed key against stray non-dump objects.
case "$KEY" in
  backups/*.dump) ;;
  *) echo "refusing key outside backups/*.dump: $KEY" >&2; exit 1 ;;
esac

VERSION_ID="${2:-}"
echo "restoring s3://$BACKUP_BUCKET/$KEY ${VERSION_ID:+(version $VERSION_ID)}"

# Always download to a file first, restore second — NOTHING reaches
# pg_restore until the bytes have been checksum-verified against the
# .ok marker. Restores run on the drill host with a credential that can read
# (which can read), so the SD-card space constraint that forces the
# BACKUP to stream does not apply here. (Streaming into pg_restore
# would also break on the version-id path: get-object prints its JSON
# response to stdout alongside the body.)
TMP_DUMP="$(mktemp /tmp/nestova-restore.XXXXXX.dump)"
trap 'rm -f "$TMP_DUMP"' EXIT
if [ -n "$VERSION_ID" ]; then
  aws s3api get-object --bucket "$BACKUP_BUCKET" --key "$KEY" \
    --version-id "$VERSION_ID" --region "$BACKUP_REGION" "$TMP_DUMP" > /dev/null
else
  aws s3 cp "s3://$BACKUP_BUCKET/$KEY" "$TMP_DUMP" --region "$BACKUP_REGION"
fi

# Verify against a recorded checksum — ALWAYS, on every path. This is
# what binds a marker to the exact bytes it vouched for: an object
# replaced after its marker was written (compromised appliance, botched
# manual upload) fails here, BEFORE any destructive restore step.
#
# Default path: the expected sum comes from the key's current .ok
# marker. Version-id path: the current marker describes the CURRENT
# version, not the one being recovered, so the operator must supply the
# right digest via RESTORE_EXPECTED_SHA256 — taken from the marker
# VERSION written by the same attempt (list-object-versions on the .ok
# key; see the recovery example below). No digest, no restore: the
# recovery path is not allowed to be the least-verified one.
if [ -n "$VERSION_ID" ]; then
  EXPECTED_SUM="${RESTORE_EXPECTED_SHA256:?version-id restores require RESTORE_EXPECTED_SHA256 (from the matching .ok marker version)}"
else
  EXPECTED_SUM="$(aws s3 cp "s3://$BACKUP_BUCKET/$KEY.ok" - \
    --region "$BACKUP_REGION" 2>/dev/null | cut -d' ' -f1 || true)"
fi
ACTUAL_SUM="$(sha256sum "$TMP_DUMP" | cut -d' ' -f1)"
if [ -z "$EXPECTED_SUM" ] || [ "$ACTUAL_SUM" != "$EXPECTED_SUM" ]; then
  echo "checksum mismatch or missing marker for $KEY — refusing to restore" >&2
  exit 1
fi
echo "checksum verified: $ACTUAL_SUM"

# Last sentinel before anything destructive: the target must be EMPTY.
# The drill always creates nestova_restore fresh, so a target that
# already contains tables is, by definition, not the scratch database
# this script was pointed at — most plausibly a same-named database on
# the wrong cluster (e.g. the live Pi when the drill was meant to run
# elsewhere). No name or host check can prove which cluster
# localhost:PORT actually is; emptiness can.
# "Empty" means NO user objects of any kind — not merely no plain
# tables in public. Views, sequences, partitioned tables, or an extra
# schema all prove this database has a life of its own that --clean
# could destroy.
USER_OBJECT_COUNT="$(psql --dbname="$RESTORE_DATABASE" -Atc \
  "SELECT count(*) FROM (
     SELECT 1 FROM pg_namespace
     WHERE nspname NOT IN ('pg_catalog', 'information_schema', 'public')
       AND nspname NOT LIKE 'pg_toast%'
       AND nspname NOT LIKE 'pg_temp_%'
     UNION ALL
     SELECT 1 FROM pg_class c
     JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
       AND n.nspname NOT LIKE 'pg_toast%'
       AND n.nspname NOT LIKE 'pg_temp_%'
   ) objects")"
if [ "$USER_OBJECT_COUNT" != "0" ]; then
  echo "refusing restore: target '$RESTORE_DATABASE' already contains user objects — not a fresh scratch DB" >&2
  exit 1
fi

# --no-owner --no-acl: the scratch DB must not replay production
# ownership or GRANT/REVOKE statements (they reference roles that may
# not exist here, and must not confer production privileges if they do).
# --exit-on-error: a restore that hit an SQL error must fail the drill,
# not limp on to a partially-applied database that "looks restored".
# (--clean is kept for pg_restore's own idempotence, but the emptiness
# gate above means it never actually has anything to drop.)
pg_restore --dbname="$RESTORE_DATABASE" \
  --clean --if-exists --no-owner --no-acl --exit-on-error "$TMP_DUMP"

echo "restore ok — verifying migration status"
```

The version-id path is the recovery route the versioning setup exists
for: if a key was ever overwritten (a compromised appliance, or a
botched manual upload that clobbered a good dump), list its versions
and restore the one from before the overwrite. `KEY` is always the
exact key from the original `backup ok:` log line (the script's
generated `backups/<utc-timestamp>-<nonce>.dump` form) — never
reconstructed by hand:

```bash
KEY="<key-from-backup-ok-log>"

# The Key==... filter matters: --prefix alone would also match the
# key's own .ok marker object, and without Key in the output an
# operator could copy a MARKER's version ID into the restore command.
aws s3api list-object-versions --bucket "$BACKUP_BUCKET" \
  --prefix "$KEY" --region "$BACKUP_REGION" \
  --query 'Versions[?Key==`'"$KEY"'`].{VersionId:VersionId,LastModified:LastModified,IsLatest:IsLatest}' \
  --output table

# The expected digest comes from the MARKER VERSION the same attempt
# wrote (the marker's history parallels the dump's). List the marker's
# versions the same way and pick the one whose LastModified pairs with
# the dump version being recovered. That pairing is a heuristic, but a
# SAFE one: the restore script's checksum gate refuses any wrong pick,
# so if the first candidate is rejected, simply try the next marker
# version until the checksum matches — a match is definitive.
aws s3api list-object-versions --bucket "$BACKUP_BUCKET" \
  --prefix "$KEY.ok" --region "$BACKUP_REGION" \
  --query 'Versions[?Key==`'"$KEY"'.ok`].{VersionId:VersionId,LastModified:LastModified}' \
  --output table
# Private scratch space for the recovery artifacts — never fixed /tmp
# names, which another local process could pre-create (stealing or
# swapping the contents an admin is about to trust).
umask 077
RECOVERY_DIR="$(mktemp -d)"
trap 'rm -rf -- "$RECOVERY_DIR"' EXIT

aws s3api get-object --bucket "$BACKUP_BUCKET" --key "$KEY.ok" \
  --version-id "<marker-version-id>" --region "$BACKUP_REGION" \
  "$RECOVERY_DIR/marker.ok" > /dev/null
cut -d' ' -f1 "$RECOVERY_DIR/marker.ok"   # → the digest for RESTORE_EXPECTED_SHA256

# REQUIRED before restoring any archive you have reason to distrust
# (which a version-id recovery, by its nature, is): inspect the
# archive's table of contents first. A checksum match proves the bytes
# are the marker's bytes, not that their content is sane.
aws s3api get-object --bucket "$BACKUP_BUCKET" --key "$KEY" \
  --version-id "<dump-version-id-from-table>" --region "$BACKUP_REGION" \
  "$RECOVERY_DIR/suspect.dump" > /dev/null
pg_restore --list "$RECOVERY_DIR/suspect.dump" | "${PAGER:-more}"   # review: expected schemas/tables only?

RESTORE_PGHOST=localhost RESTORE_PGPORT=5432 RESTORE_DATABASE=nestova_restore \
  RESTORE_EXPECTED_SHA256="<digest-from-marker>" \
  /usr/local/bin/nestova-restore.sh "$KEY" <dump-version-id-from-table>
```

A dump older than 30 days has transitioned to Glacier Instant Retrieval;
`GetObject` on it still works (that is the point of Instant Retrieval —
no separate restore-request step), just at a higher per-GB retrieval
price. The drill below always exercises the newest dump, which is still
in `STANDARD_IA`.

Then confirm the restored database is at the schema the code expects —
`cmd/migrate`'s `status` subcommand against the scratch DB (from the repo
root), aimed at the SAME endpoint the restore script just wrote
(`$RESTORE_PGHOST:$RESTORE_PGPORT`, whatever they were — a hardcoded
`localhost:5432` here would happily "verify" a different cluster than a
remote or nonstandard-port restore actually touched). The `RESTORE_*`
values must be defined in THIS shell — an inline assignment made only on
the restore script's own command line never reaches this later command:

```bash
export RESTORE_PGHOST=localhost RESTORE_PGPORT=5432 RESTORE_DATABASE=nestova_restore
DATABASE_URL="postgres://$PGUSER@$RESTORE_PGHOST:$RESTORE_PGPORT/$RESTORE_DATABASE" \
  go run ./cmd/migrate status
# Remote restore? The script's own PGSSLMODE export never reaches this
# separate process — append ?sslmode=verify-full to the DSN here too.
# IPv6 host (e.g. ::1)? A URL requires it bracketed — [::1] — or the
# DSN parses the colons as a port separator.
```

(An environment-variable assignment like this never appears in the
process's argv, unlike a command-line argument — the same reason the
scripts above take libpq env vars. The DSN also carries no password:
`cmd/migrate`'s pgx driver fills any field the DSN omits from
`PGPASSWORD`/`PGPASSFILE`, keeping the secret out of shell history.)

Every migration listed as applied and none pending means the dump is a
complete, current-schema copy — the restore is verified, not assumed.

## Quarterly restore drill

The drill is the restore script plus the migration check against a
scratch database, end to end.

Trust gate, before running it: the ROUTINE drill (no suspected
compromise) may take the default path below — its checksum gate covers
the accident classes a routine drill exists to catch. After any
**suspected appliance compromise**, do NOT run the default path:
follow the recovery procedure above instead (admin-attested marker
version + `pg_restore --list` TOC inspection before anything touches a
database), because dump and marker share the appliance credential and a
checksum match alone proves nothing about intent. Either way the target
is a scratch database on a drill machine — never a cluster with
production data or credentials on it.

The sequence runs under `set -e` deliberately: a failed restore or
migration check must NOT fall through to `dropdb`, which would destroy
the very failed state a diagnosis needs. Clean up manually after
investigating a failure.

```bash
set -e
# Same routing hygiene as the restore script: PGHOST alone doesn't pin
# libpq if a service file, PGHOSTADDR, or a stray PGPORT lurks in the
# ambient shell — pin host AND port on every step.
unset PGSERVICE PGSERVICEFILE PGHOSTADDR
# One definition of the endpoint, used by EVERY step below — create,
# restore, verify, drop must all mean the same cluster.
RESTORE_PGHOST=localhost
RESTORE_PGPORT=5432
PGHOST="$RESTORE_PGHOST" PGPORT="$RESTORE_PGPORT" createdb nestova_restore
RESTORE_PGHOST="$RESTORE_PGHOST" RESTORE_PGPORT="$RESTORE_PGPORT" \
  RESTORE_DATABASE=nestova_restore /usr/local/bin/nestova-restore.sh
# Pin the status check to the SAME endpoint and database the restore
# just wrote — a bare DATABASE_URL from the ambient shell could
# silently report on some other cluster entirely. The DSN carries NO
# password: cmd/migrate's pgx driver reads PGPASSWORD/PGPASSFILE from
# the environment for any field the DSN omits, so the secret never
# lands in shell history or command auditing. (An IPv6 RESTORE_PGHOST
# such as ::1 must be bracketed — [::1] — inside a URL.)
STATUS="$(DATABASE_URL="postgres://$PGUSER@$RESTORE_PGHOST:$RESTORE_PGPORT/nestova_restore" \
  go run ./cmd/migrate status)"
echo "$STATUS"
# `migrate status` (goose) is informational — it exits 0 even with
# pending migrations — so gate the cleanup explicitly: any Pending row
# means the dump is NOT a complete current-schema copy, and the failed
# state must survive for diagnosis instead of being dropped. Anchor the
# match to the Applied-At column at line start — a bare `grep -i
# pending` could false-positive on a migration FILENAME containing
# "pending" in the Migration column.
if echo "$STATUS" | grep -Eq '^\s*Pending\s'; then
  echo "drill FAILED: restored database has pending migrations" >&2
  exit 1
fi
PGHOST="$RESTORE_PGHOST" PGPORT="$RESTORE_PGPORT" dropdb nestova_restore
```

Every database-touching step carries the same explicit `localhost` — the
restore script via `RESTORE_PGHOST`, `createdb`/`dropdb` via `PGHOST`,
the status check inside its DSN. Left ambient, libpq would happily
create (and later drop) the scratch database on whatever cluster the
surrounding shell happens to point at. (The restore script additionally
pins non-local hosts behind `RESTORE_PGHOST_ALLOW_REMOTE=yes` — see its
own comments.) `dropdb` runs only after the pending-migrations gate
passes; a failed drill leaves `nestova_restore` in place for diagnosis —
drop it manually once investigated.

Run it quarterly. Registering it as a recurring maintenance task in the
app's own recurrence engine is a deliberate follow-up ticket, not part of
NES-142 — until then, a calendar reminder is the mechanism.

## Observability

The script's final act on success is publishing `BackupAgeHours = 0` to
the `Nestova/Backups` CloudWatch namespace. NES-143's dead-man alarm
consumes it: healthy nights re-zero the metric, and any failure — script
error, dead Pi, dead timer — publishes nothing, which the alarm's
missing-data handling (`treat missing data as breaching`, evaluation
window comfortably past the nightly cadence) turns into an alert. The
failure mode where the whole appliance is down is exactly the one an
app-hosted metric could never report, which is why this is a CloudWatch
push and not a Prometheus scrape.

Stated threat model, explicitly: the heartbeat detects **broken or dead
backup automation** — script failure, dead timer, powered-off Pi. It is
NOT an integrity attestation: any process holding the appliance
credential could publish `BackupAgeHours = 0` without a real backup
(and could equally upload a garbage dump, which no client-pushed signal
can disprove). Defending the alarm against a *compromised* appliance
requires an AWS-side signal — e.g. alarming on the bucket's own
`PutObject` activity via an EventBridge rule or S3 request metrics
instead of a client-pushed metric. That hardening belongs to NES-143's
alarm design if it's wanted; the quarterly restore drill (above) remains
the only real proof the backups themselves are good.

Locally, `systemctl status nestova-backup.service` and `journalctl -u
nestova-backup.service` show the last run's output.

## Spend safety

STANDARD_IA for the newest 30 days of dumps, Glacier Instant Retrieval
from day 30, one small custom-format dump per night, with an expired
dump's bytes lingering as a Glacier-IR noncurrent version up to ~day 800
(the versioning consequence documented under Lifecycle above): roughly
$0.05–0.30/mo at family scale even counting that tail. It is covered by
the account-wide $10/mo cost budget (`docs/aws-guardrails.md`, Budget 1)
with no dedicated backup budget.

To be precise about what bounds what: lifecycle bounds **duration** (an
~800-day eligibility window — payload versions are removed once
eligible, subject to S3's asynchronous lifecycle lag of days; only
zero-payload delete-marker tombstones linger past it, per the Lifecycle
note above), not **volume** — as
the residual-risk note under IAM spells out, a compromised appliance can
upload arbitrary amounts within that window, and the budget only alerts
(with up to ~24h lag) rather than stopping anything. Under *normal
operation* (one nightly dump), the bounded window does keep steady-state
cost flat and tiny; under compromise, the `BucketSizeBytes` alarm
suggested for NES-143 is the control to reach for — treat it as required
if spend containment is a hard requirement rather than a
family-appliance trade-off.

## Acceptance criteria → where each is satisfied

- **Nightly dump, missed nights made up at boot** — the timer
  (`OnCalendar` 03:15, `Persistent=true`).
- **Appliance cannot delete any backup, and every overwrite preserves
  the prior version** — IAM (`s3:PutObject` only, no delete) plus bucket
  versioning; recovery of a preserved version is the admin-side
  version-id restore path.
- **Restore produces a working database, verified** — restore script +
  `cmd/migrate status` against the scratch DB.
- **Lifecycle transitions and expiry configured and visible** — the
  lifecycle rule (Glacier IR at 30d, expiry at 400d), verifiable via
  `get-bucket-lifecycle-configuration`.
- **Failure/staleness observable** — `BackupAgeHours` heartbeat consumed
  by NES-143's alarm.
