# AGENTS.md

Guidance for AI coding agents working on pg-sprite — an online schema-change engine for Aurora
PostgreSQL. Deliberately short: don't restate what you can infer from the code.

This file is canonical. `CLAUDE.md`, `GEMINI.md`, `.cursorrules`, `.goosehints`, and
`.github/copilot-instructions.md` are symlinks to it — edit only this file. Review-agent
checks live in [.agents/checks/review.md](.agents/checks/review.md).

## Read SAFETY.md first

This codebase is partitioned into a **safety-critical core** and a periphery.
[SAFETY.md](SAFETY.md) lists which packages are which and the stricter rules that apply inside
the core (proof types, bounded everything, `// INV:` locality, the core dependency list, the
never-import-`block/spirit` rule). Before touching a `pkg/` package, check its row in
SAFETY.md — the review bar and the AI-assistance posture differ by side.

## Build and test

```sh
make setup       # one-time: configure git hooks (core.hooksPath .githooks)
make build       # build ./... and bin/pg-sprite
make test        # full suite; integration tests need Docker
make test-unit   # SKIP_INTEGRATION=1, no Docker
make test-db     # suite against the compose DB (make db-up first); PG_DSN
make test-supported-postgres  # full suite on every major 14 -> 18
make lint        # golangci-lint
```

- **Coverage invariant:** no behavior lands without a test that would fail without it; bug
  fixes land with a regression test; the full suite (unit, integration, TLS, version matrix)
  is a merge gate. Full statement: [docs/testing.md](docs/testing.md#the-coverage-invariant);
  test-methodology rules (lifecycle fixtures, two-oracle SQL tests, real fault injection,
  convergence oracle) are the `TM-*` registry in the same doc.
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
- Tests use testify (`require` for setup, `assert` for verification), `t.Context()` (in
  cleanups, which run after the context is cancelled, use
  `context.WithoutCancel(t.Context())`), and named polling deadlines — no bare `time.Sleep`
  readiness waits.
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
- **Never panic in library code.** Invariant violations return `ErrInvariantViolation`
  fail-closed (see [SAFETY.md](SAFETY.md)); panics are reserved for provable programmer error
  at startup.
- **Match Postgres errors by SQLSTATE** (`errors.As` → `*pgconn.PgError`, branch on `.Code`),
  never by message text — error text varies by server version and locale. Sentinel/typed
  errors are compared with `errors.Is`/`As` at boundaries.
- **Every goroutine has an owner, a bounded lifetime, and a stop path** — no fire-and-forget
  `go func()`.
- **Core logic takes time from an injected clock**, not inline `time.Now()`/`time.Sleep` —
  deterministic tests depend on it.
- Comments describe *what* and *why*, never history — no bug/PR references, no
  "previously X" notes, no counts or thresholds that go stale; move comments with the code
  they explain.
- Tests assert specific values, not just existence; no negative regression tests for removed
  behavior. Log messages state what *will* happen, not what *might* ("will block", not
  "may be blocked").
- Never reference internal company details (cluster names, hostnames, org names) in code,
  comments, commits, or PRs — this is a public repo.

Mechanical style rules (doc comments on exported symbols, no `init()`, no package-level
mutable state, no `context.Context` in structs) are enforced by `.golangci.yml`, not prose.

Design docs live in [docs/](docs/) — start at [docs/README.md](docs/README.md); the invariant
registry is [docs/invariants.md](docs/invariants.md).

> This file grows with the codebase. Keep it short: rules earn a line here only when an agent
> can't infer them from the code.
