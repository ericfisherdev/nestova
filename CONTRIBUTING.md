# Contributing

## Development setup

See the [README](README.md) for the toolchain and `make` targets. Enable the
Git hooks once per clone with `make hooks`.

## Commit messages

Commits follow [Conventional Commits](https://www.conventionalcommits.org):

```text
<type>(<optional scope>): <description>
```

- **Allowed types:** `feat`, `fix`, `docs`, `chore`, `refactor`, `test`,
  `build`, `ci`, `perf`, `style`, `revert`.
- **Scope** is optional; the project uses the Jira issue key, e.g.
  `feat(NES-25): ...`.
- The description is lower-case and has no trailing period.

Enforcement is automated (policy in [`.conform.yaml`](.conform.yaml), via
[conform](https://github.com/siderolabs/conform)):

- Locally, the Lefthook `commit-msg` hook rejects a non-conforming message
  before the commit is created (`make hooks` to enable).
- In CI, the `commit-lint` job validates every commit a PR adds, and the
  `pr-title` check validates the PR title (used as the subject on a squash
  merge).

Examples — good: `fix(NES-8): clamp value to non-negative`;
bad: `Added stuff.` (no type, capitalised, trailing period).

## Branch protection & merging

`main` is protected. Pull requests cannot be merged until:

- **1 approving review** is recorded. CodeRabbit reviews every PR; its approval
  is required before merge.
- The **`build`** and **`commit-lint`** status checks pass. These are jobs in
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml): `build` runs the same
  gates as the local hooks (templ freshness, lint, formatting, tests), and
  `commit-lint` enforces Conventional Commits.
- The branch is **up to date with `main`** (strict mode) — rebase onto the
  latest `main` if it has moved on.

Admin enforcement is intentionally **off** (`enforce_admins=false`) so the solo
maintainer can merge their own approved PRs: GitHub blocks self-approval, so
without this a one-person project could never merge. It is not a license to skip
the checks — the required review and `build` check still apply to normal flow.

Merge with **rebase and merge** to keep a linear history, then delete the
branch.
