# AGENTS.md

Guidance for AI coding agents working on pg-sprite — an online schema-change engine for Aurora
PostgreSQL. Deliberately short: don't restate what you can infer from the code.

## Read SAFETY.md first

This codebase is partitioned into a **safety-critical core** and a periphery.
[SAFETY.md](SAFETY.md) lists which packages are which and the stricter rules that apply inside
the core (proof types, bounded everything, `// INV:` locality, the core dependency list, the
never-import-`block/spirit` rule). Before touching a `pkg/` package, check its row in
SAFETY.md — the review bar and the AI-assistance posture differ by side.

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

## Go maxims

- **"A little copying is better than a little dependency."** Small mechanics (retry/backoff, CA
  loading, keepalives, tiny helpers) are hand-written or copied with an attributing comment —
  never imported. Take pinned dependencies only for load-bearing expertise (the parser, the wire
  protocol); a dependency inside a core package needs a recorded decision (see
  [SAFETY.md](SAFETY.md)). **Never import `github.com/block/spirit` as a module** — port ideas
  with citations, not code.
- **Expose the smallest interface that does the job.** Export domain types and their validating
  constructors, not internals; no re-exports or plain-delegation wrappers — callers import the
  source package.
- **Clear is better than clever.** No clever SQL, no dense compound predicates — extract a named
  helper for any 3+-term or state-machine conditional. Separate error handling from state
  decisions. This code gets read during incidents; readability outranks ease of use and
  performance here (correctness outranks both).
- **Minimize state; derive rather than store.** If a value can be recomputed from the database
  or the checkpoint, don't persist it.
- **Don't conflate causes.** No `if err != nil || value == nil` when the cases mean different
  things; no deduping unrelated branches with `||` — separate branches calling a shared helper.
- Concurrency: `wg.Go(...)`; `context.WithoutCancel(ctx)` for background goroutines that must
  outlive a request; snapshot shared state under one lock acquisition, not several.
- Cleanup: close errors are logged, not discarded — no bare `_ = x.Close()` (one exception: a
  redundant safety closer on a handle someone else owns discards its guaranteed
  already-closed error).
- State comparisons use typed constants and helpers, never raw string matching.

Design docs live in [docs/](docs/) — start at [docs/README.md](docs/README.md); the invariant
registry is [docs/invariants.md](docs/invariants.md).

> This file grows with the codebase. Keep it short: rules earn a line here only when an agent
> can't infer them from the code.
