# Contributing

## Development setup

See the [README](README.md) for the toolchain and `make` targets. Enable the
Git hooks once per clone with `make hooks`.

## Branch protection & merging

`main` is protected. Pull requests cannot be merged until:

- **1 approving review** is recorded. CodeRabbit reviews every PR; its approval
  is required before merge.
- The **`build`** status check passes. This is the `build` job in
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml), which runs the same
  gates as the local hooks (templ freshness, lint, formatting, tests).
- The branch is **up to date with `main`** (strict mode) — rebase onto the
  latest `main` if it has moved on.

Admin enforcement is intentionally **off** (`enforce_admins=false`) so the solo
maintainer can merge their own approved PRs: GitHub blocks self-approval, so
without this a one-person project could never merge. It is not a license to skip
the checks — the required review and `build` check still apply to normal flow.

Merge with **rebase and merge** to keep a linear history, then delete the
branch.
