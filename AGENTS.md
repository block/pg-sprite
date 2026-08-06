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
- Never increase timeouts to fix flakes; find the root cause — then prove the fix holds with
  `scripts/test-flaky.sh <TestName> [iterations] [package]` before declaring it fixed.
- Integration tests run against real PostgreSQL (testcontainers); `PG_VERSION` selects the
  major (default 16), CI runs the matrix 14 → 18. Core logic is validated against a real
  database — no mocked-DB tests for core logic.

## Conventions

- Say **"schema change"**, not "migration", in code, CLI output, error messages, and new docs —
  pg-sprite strings surface through orchestrators that ban "migration". Use "migration" only
  when citing external sources (Spirit's `pkg/migration`, peer tools, PostgreSQL docs).
- Use `pkg/dbconn` for connections — never raw `pgx` pools in production code (tests excepted).
  Every session runs under bounded `lock_timeout` / `statement_timeout`.
- Never build SQL by interpolating raw identifiers: any user-supplied or introspected name in
  generated SQL goes through `pgx.Identifier{...}.Sanitize()` (or `quote_ident()` server-side).
- Never string-manipulate connection strings/DSNs — parse (`pgx.ParseConfig`), modify fields,
  re-serialize; string ops break on passwords containing `/`, `@`, or `%`.
- All SQL parsing goes through the real PostgreSQL grammar via `wasilibs/go-pgquery` (Wasm
  `libpg_query`; the cgo `pg_query_go` is the API-compatible escape hatch, not the default),
  with `pkg/statement` as the parse boundary. No `strings.Split(";")`, no hand-parsing; a parse failure is an
  error surfaced to the caller. Shadow-table DDL and checkpoint fingerprints are derived by
  execute-and-introspect on the engine-owned scratch database, never by AST transformation
  (see [docs/low-level-design.md](docs/low-level-design.md#how-the-planner-understands-ddl-decided)).
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

## Logging and observability

- **stdout is the product's output; diagnostics go to stderr.** Command results (verdicts,
  status) are written to the injected writer only; everything diagnostic goes through
  `log/slog`. `--debug` on DB commands enables statement-level tracing (pgx tracelog via
  `pkg/dbconn`) plus lifecycle events; without it, diagnostics are discarded.
- **Log decisions and state transitions, not progress noise.** Static messages; the
  variability goes into attrs with stable snake_case keys, and the same key means the same
  thing everywhere (`schema`, `table`, `total_bytes`, `elapsed`).
- **Logs answer the triage question.** Error- and warn-path logs carry the identifiers an
  operator needs to act — schema, table, database, the operation being attempted — as
  attrs, not buried in prose.
- **One error, one log.** Errors are wrapped and returned; only the entry point logs or
  prints them. `pkg/` packages never log an error they also return.
- **Never log credentials or connection strings** — a DSN/URL carries a password; log host,
  database, and user as separate attrs when needed. Never log row data.
- **Log output is never a test surface.** Tests assert typed outcomes — `errors.Is`/`As`,
  verdict fields, exit codes, JSON output — never log text or human-facing wording. If a
  behavioral difference is visible only in prose, make it machine-readable first (a typed
  field or reason), then test that. The only exception is a renderer's own unit test.
- **Operational quantities ride on logs until there is a metrics runtime.** Durations,
  sizes, and retry counts are logged as attrs. When the long-running phases need real
  metrics, they arrive as OpenTelemetry instruments behind one engine-owned `pkg/metrics`
  with `Record*` helpers — dotted `pgsprite.` names with explicit units, low-cardinality
  snake_case attributes, counters for rare or dangerous branches operators can act on —
  never direct exporter imports in core (the dependency rule in SAFETY.md applies).

Mechanical style rules (doc comments on exported symbols, no `init()`, no package-level
mutable state, no `context.Context` in structs, static slog messages with snake_case keys,
no printing to process stdout) are enforced by `.golangci.yml`, not prose.

## Git and PRs

- Do not create PRs automatically — pushing a branch is fine; opening the PR is the author's
  decision. When asked, create PRs as drafts (`gh pr create --draft`); the author marks ready.
- Never squash or rewrite history after a human has reviewed (comments or approval) — add
  commits so reviewers can see increments. Squash freely before review.
- Agent disclosure lines (agent name + model) go at the *bottom* of PR bodies and issue
  bodies, after the content.
- Never reply to, post on, or resolve *human* review threads without the author's explicit
  approval — agents do not speak for the author. Automated reviewer (e.g. Copilot) comments
  may be replied to and resolved without separate approval, provided each reply describes the
  fix with a commit link (or a reasoned rejection), is prefixed 🤖, and carries the agent
  disclosure — resolve only after the reply is posted.
- After pushing new commits, refresh the PR title/summary to match — unless a human has
  edited it.
- Upstream large branches with the leaf approach: map the dependency graph, peel off leaf
  changes as small independent PRs first, in topological order.

Design docs live in [docs/](docs/) — start at [docs/README.md](docs/README.md); the invariant
registry is [docs/invariants.md](docs/invariants.md).

> This file grows with the codebase. Keep it short: rules earn a line here only when an agent
> can't infer them from the code.
