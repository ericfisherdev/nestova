# Testing conventions

Nestova uses Go's standard `testing` package — no third-party assertion or
mocking libraries — to keep the toolchain minimal and tests obvious.

## Conventions

- **Table-driven tests.** Express cases as a slice of structs and iterate with
  `t.Run(tt.name, ...)` subtests. Name each case after the behavior it pins.
- **Black-box where practical.** Put tests in the `<pkg>_test` package and
  exercise the exported API. Drop to white-box (`package <pkg>`) only when a
  test genuinely needs unexported internals.
- **Lock the contract.** Prefer explicit `want` values and exact comparisons
  over permissive checks (e.g. assert the full output, not just `Contains`).
- **Isolate the environment.** Use `t.Setenv` (which auto-restores) instead of
  mutating process state directly. Tests that use `t.Setenv` must not call
  `t.Parallel()`.
- **File layout.** A package's tests live beside it as `<file>_test.go`.

## Reference examples

- [`internal/platform/config/config_test.go`](../internal/platform/config/config_test.go)
  — table-driven, black-box, environment isolation.
- [`web/components/hello_test.go`](../web/components/hello_test.go)
  — table-driven render output + HTML-escaping assertions.

## Running

```sh
make test    # go test -race -cover -coverprofile=coverage.out ./...
make cover   # per-function coverage summary from coverage.out
```

`make test` runs with the race detector and writes a coverage profile to
`coverage.out` (git-ignored). CI runs the same target and surfaces the profile
as a build summary.
