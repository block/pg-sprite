# AGENTS.md

Guidance for AI coding agents working on pg-sprite — an online schema-change engine for Aurora
PostgreSQL. Deliberately short: don't restate what you can infer from the code.

## Read TCB.md first

This codebase is partitioned into a **trusted computing base** and an untrusted periphery.
[TCB.md](TCB.md) lists which packages are which and the stricter rules that apply inside the
boundary (proof types, bounded everything, `// INV:` locality, the TCB dependency list, the
never-import-`block/spirit` rule). Before touching a `pkg/` package, check its row in TCB.md —
the review bar and the AI-assistance posture differ by side.

## Build and test

```sh
make build       # build ./... and bin/pg-sprite
make test        # full suite; integration tests need Docker
make test-unit   # SKIP_INTEGRATION=1, no Docker
make lint        # golangci-lint
```

- Always run the full `make test` when the scope of a change is unclear.
- Never assume a test failure is unrelated to your change; investigate it.
- Never increase timeouts to fix flakes; find the root cause.
- Integration tests run against real PostgreSQL (testcontainers); `PG_VERSION` selects the
  major (default 16), CI runs the matrix 14 → 18. Core logic is validated against a real
  database — no mocked-DB tests for core logic.

## Conventions

- Use `pkg/dbconn` for connections — never raw `pgx` pools in production code (tests excepted).
  Every session runs under bounded `lock_timeout` / `statement_timeout`.
- All SQL parsing goes through `pg_query_go` (once `pkg/statement` exists). No
  `strings.Split(";")`, no hand-parsing; a parse failure is an error surfaced to the caller.
- Tests use testify (`require` for setup, `assert` for verification), `t.Context()` (except in
  cleanups, which run after the context is cancelled), and named polling deadlines — no bare
  `time.Sleep` readiness waits.
- Errors: wrap with context and identifiers (`fmt.Errorf("create slot %s: %w", name, err)`);
  never log-and-continue; no silent branch cases; no `nolint`; no `--no-verify`.

> This file grows with the codebase (see the research build-tracker task for the full
> AGENTS.md derivation from schemabot's). Keep it short: rules earn a line here only when an
> agent can't infer them from the code.
