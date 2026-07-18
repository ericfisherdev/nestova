# Photo storage: enabling S3 (NES-131/132/133)

Nestova stores photo bytes (album photos and chore-proof before/after
photos) behind a swappable `PhotoStore` port. Every install starts on the
local filesystem (`LocalPhotoStore`, `MEDIA_STORAGE_BACKEND=local`, the
default); an install that wants off-appliance backup, more storage than the
local disk, or a Raspberry Pi with no room to grow can opt into an
S3-compatible object store instead (AWS S3, or a self-hosted MinIO/Garage
instance on the LAN). This page is the operator runbook for making that
switch on an **existing** install that already has local-backed photos —
`cmd/storage` (NES-133) is the tooling that moves them.

A brand-new install can skip straight to setting `MEDIA_STORAGE_BACKEND=s3`
before its first boot; there is nothing to migrate.

## Provisioning the bucket

Create the S3 bucket (or MinIO/Garage bucket) and credentials before
touching any Nestova configuration — see the team wiki's AWS setup page for
account/bucket/IAM provisioning steps. You need, at minimum: a bucket name,
a region (any non-empty value for MinIO/Garage), and either an access
key/secret pair or an environment that already supplies AWS credentials
another way (an IAM role, the shared credentials file, etc.).

## Configuration reference

These are the same `S3Config` settings `cmd/server` reads (see
`internal/platform/config/config.go`'s `S3Config`/`MediaConfig`):

| Variable | Required | Notes |
|---|---|---|
| `MEDIA_STORAGE_BACKEND` | yes | Set to `s3` to select the S3-compatible backend. **Must** be `s3` for `cmd/storage`'s migrate/verify/reap commands to do anything (see below). |
| `S3_BUCKET` | yes | The single bucket every photo (both classes) is stored under. |
| `S3_REGION` | yes | Real AWS S3 needs a genuine region; MinIO/Garage accept any non-empty value (e.g. `us-east-1`). |
| `S3_ENDPOINT` | no | Blank targets real AWS S3. Set this to a MinIO/Garage/R2 base URL for a self-hosted or non-AWS target. |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | no (both-or-neither) | Static credentials. Leave both blank to use the AWS SDK's default credential chain instead. |
| `S3_USE_PATH_STYLE` | no | Set `true` for MinIO/Garage and most self-hosted S3-compatible servers. |
| `S3_PRESIGN_TTL` | no | How long a presigned photo URL stays valid (default 15 minutes). |

**Why `MEDIA_STORAGE_BACKEND=s3` must already be set before running
`cmd/storage`:** `config.Load` only parses/validates the `S3_*` settings
above when `MEDIA_STORAGE_BACKEND=s3` — a local-backend deployment must
never fail startup on a stray or partial `S3_*` value it will never use
(see `config.go`'s "resolve the media storage backend BEFORE any
S3-specific parsing" comment). If you run `cmd/storage` with
`MEDIA_STORAGE_BACKEND` still `local`, every command fails immediately with
a clear "MEDIA_STORAGE_BACKEND=s3 ... must be set" error rather than doing
something silently wrong (e.g. constructing a MinIO client with
`S3_USE_PATH_STYLE` force-defaulted to `false`, which would break every
upload).

This has a useful consequence: it is **safe to flip `MEDIA_STORAGE_BACKEND`
to `s3` and restart `cmd/server` before running the migrator at all.** New
uploads immediately start going to S3, while every existing local-backed
row keeps reading correctly — `cmd/server`'s composition root always
constructs the local `PhotoStore` too, regardless of the configured
backend, specifically so a backend switch never strands historical rows
(see `domain.PhotoStoreResolver`'s doc). You do not have to choose between
"flip the server" and "run the migrator" first; either order works, because
reads are resolved per-row, not per-deployment.

## The runbook

1. **Provision** the bucket and credentials (above).
2. **Configure**: set `S3_BUCKET`, `S3_REGION`, and the rest of the table
   above, plus `MEDIA_STORAGE_BACKEND=s3`, in the environment.
3. **Migrate**: move every existing local-backed photo to S3.

   ```sh
   go run ./cmd/storage migrate
   ```

   This walks both photo classes (album photos in `photo`, chore-proof
   before/after photos in `task_instance_photo`) in batches, uploading each
   local row's bytes to S3 under its canonical, content-addressed,
   class-namespaced key and flipping `storage_backend` to `s3` once the
   upload is verified. It is **safe to interrupt at any point** (Ctrl-C,
   `SIGTERM`, a crash, a network blip) and re-run: a row already flipped to
   `s3` no longer appears in the next run's local-backend listing, so
   re-running simply picks up wherever it left off — no duplicate uploads,
   no corrupted state. Progress is logged per class as `done/total`.

   Two content-addressed photos that happen to share identical bytes within
   the same household (the local store already dedups them onto one file)
   migrate to the SAME S3 object: the second row's migration finds the
   object already present and just flips its own row, without re-uploading.

   Restrict to one class with `--class=album` or `--class=chore` if you
   want to migrate in stages.

   A **hash mismatch** (a local file's bytes no longer match the row's
   recorded `content_sha256` — e.g. on-disk corruption) aborts THAT row's
   flip only; the row keeps serving from local, the mismatch is counted and
   reported, and the migrator continues with every other row. Re-run
   `storage verify` (below) to see exactly which rows need attention, and
   `storage migrate`'s own exit code is nonzero whenever any class hit a
   mismatch or a hard error, so this is easy to catch in a script.

4. **Verify**: cross-check the database against the bucket's actual
   contents.

   ```sh
   go run ./cmd/storage verify
   ```

   For each class, this reports:

   - **Rows without an object** — a `storage_backend='s3'` row whose object
     is missing from the bucket. This is the data-loss alarm: it means the
     database thinks a photo is on S3, but the bytes are not there.
   - **Objects without a row** — a bucket object no row references
     (informational: a `storage reap` candidate, not data loss by itself —
     see below).
   - **Cross-prefix rows** — a row whose OWN `storage_ref` embeds the WRONG
     class's key prefix for the table it lives in (e.g. an album-table row
     pointing at a `chore-photos/` key). This should never happen in normal
     operation; it signals a bug, most likely in whatever wrote that row.
   - **Missing local files** — the identical data-loss alarm for
     `storage_backend='local'` rows: a row references a file MEDIA_ROOT
     does not have.

   Exit codes: `0` clean, `1` any data-loss finding (rows without an
   object, or a missing local file), `2` a usage/configuration error (e.g.
   `MEDIA_STORAGE_BACKEND` is not `s3`, or the database/bucket is
   unreachable).

5. **Flip the server** (if you have not already — see the note in
   "Configuration reference" above about why this can happen at any point):
   restart `cmd/server` with `MEDIA_STORAGE_BACKEND=s3` set. New uploads now
   go to S3.
6. **Verify again**, once the server has been running on S3 for a while, to
   confirm nothing regressed.
7. **Reclaim local disk space, much later, once you're confident**:

   ```sh
   go run ./cmd/storage migrate --delete-local
   ```

   `--delete-local` deletes a photo's local file once its move to S3 is
   verified — but ONLY when no other local-backend row still references the
   exact same file (content-addressed dedup means two chore-proof rows can
   legitimately share one local object; deleting it out from under an
   un-migrated sibling row would destroy bytes that row still needs). If you
   ran the initial `storage migrate` WITHOUT `--delete-local` (the
   recommended default — keep local as a safety net until you trust the S3
   copy), running `storage migrate --delete-local` again later finds every
   row already on S3, re-verifies each one's local file against its
   recorded hash, and only then deletes it. This makes "migrate now, delete
   local much later" a completely ordinary two-step operation, not a
   special mode.

   A rare edge case: a photo whose `storage_ref` predates NES-131's
   class-segmented key layout (a legacy bare `<household>/<aa>/<hash>.<ext>`
   ref — see `LocalPhotoStore`'s doc) is normalized to the current canonical
   key when it migrates. `--delete-local`'s later sweep pass looks a row's
   local file up by its CURRENT (post-normalization) ref, so it cannot find
   that legacy row's file at its OLD path — the file is simply never
   deleted (over-retention, not data loss). This only affects photos
   uploaded before NES-131 shipped.

8. **Reclaim orphaned S3 objects** (separate from local disk cleanup above):
   `storage reap` runs NES-132's `ReaperService` — it deletes bucket objects
   no row references at all (typically left behind by `PhotoService.Delete`,
   which only ever deletes the ROW immediately; the object is reclaimed here
   after a grace window, never inline, so a concurrent in-flight upload is
   never destroyed).

   ```sh
   go run ./cmd/storage reap --dry-run   # preview only, deletes nothing
   go run ./cmd/storage reap             # the real, destructive pass
   go run ./cmd/storage reap --grace=720h  # override the default 30-day grace window
   ```

   `--dry-run` lists exactly what the next real run would delete — every
   orphaned object per class, and how many chore-proof rows the optional
   retention pass (`MEDIA_CHORE_PROOF_RETENTION_DAYS`) would remove — without
   deleting or removing anything.

   **Operator contract:** `storage reap` (dry-run or not) must not be run
   concurrently with a database restore. A restore is expected to happen
   with the application fully quiesced. See `ReaperService`'s doc
   (`internal/media/app/reaper_service.go`) for the full TOCTOU-narrowing
   argument this contract rests on.

## Command reference

```
go run ./cmd/storage migrate [--class=album|chore] [--delete-local]
go run ./cmd/storage verify
go run ./cmd/storage reap [--dry-run] [--grace=720h]
```

Every subcommand loads configuration the same way `cmd/server` does
(`config.Load`) and requires `DATABASE_URL` plus `MEDIA_STORAGE_BACKEND=s3`
(and the `S3_*` settings) to be set in its environment.

Exit codes: `migrate` and `verify` both use `0` (clean), `1` (findings —
a hash mismatch/hard error for migrate, a data-loss finding for verify),
`2` (usage/configuration error). `reap` uses `0` for any successful run
(dry-run or real) and `2` for a usage/configuration error; there is no
separate "findings" exit state for `reap` beyond what it prints.
