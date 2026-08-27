# Review Checks

Checks for AI review agents. The authoritative rules live in [SAFETY.md](../../SAFETY.md),
[AGENTS.md](../../AGENTS.md), and [docs/tcb-model.md](../../docs/tcb-model.md) — this file is
the reviewer's distillation.

- Judge the change through both project lenses: (a) OSS-first — pg-sprite as the preferred
  standalone PostgreSQL online-DDL tool (CLI usable without an orchestrator, external-user
  docs and errors, no Block-internal assumptions); (b) clean SchemaBot integration — a stable
  adapter-friendly seam (library API, verdict/plan JSON, error taxonomy) with the core never
  depending on SchemaBot. Flag changes that serve one lens at the other's expense without a
  recorded decision.
- Look up every touched `pkg/` package in the SAFETY.md partition table first — the review bar
  differs between the safety-critical core and the periphery. Flag core changes with 🌶️ and
  state the blast radius (data corruption, lost writes, wrong-table swap, stranded slot).
- Core packages: every loop, queue, retry, and wait must be bounded. An unbounded anything in
  a core package is a review-blocking defect.
- Dangerous APIs accept proof types (`statement.Classified`, `PreflightedTable`,
  `AbsentTarget`, `VerifiedShadow`, `CleanWatermark`, `TableLock`) with package-private
  constructors — never a
  raw string or bool that a caller could fabricate. Core code re-verifies its own
  preconditions; it never trusts that the planner or CLI checked.
- Invariant enforcement points carry a `// INV: <id>` comment matching
  [docs/invariants.md](../../docs/invariants.md); violations use `ErrInvariantViolation`
  naming the ID and abort fail-closed — never a warning, never retried.
- New dependencies inside a core package require a recorded decision (core dependency list:
  `pgx/v5`, `pglogrepl`, stdlib). `github.com/block/spirit` must never be imported as a
  module — ideas are ported with citations, not code.
- Connections go through `pkg/dbconn` (bounded `lock_timeout` / `statement_timeout`) — flag
  raw `pgx` pools in production code.
- SQL parsing goes through `wasilibs/go-pgquery` (Wasm `libpg_query`); flag
  `strings.Split(";")`, any hand-parsing, and imports of the cgo `pg_query_go` (documented
  escape hatch, not the default). A parse failure is an error surfaced to the caller.
  Shadow-table DDL and checkpoint fingerprints come from execute-and-introspect on the scratch
  database — flag AST surgery that constructs the shadow schema or fingerprints SQL text.
- Generated SQL quotes every user-supplied or introspected identifier
  (`pgx.Identifier{...}.Sanitize()` / `quote_ident()`) — flag raw interpolation of names into
  SQL. Connection strings are parsed and re-serialized (`pgx.ParseConfig`), never
  string-manipulated.
- Terminology: "schema change", not "migration", in code, CLI output, error messages, and new
  docs — flag new occurrences except citations of external sources.
- Errors: wrapped with context and identifiers; no log-and-continue, no silent branch cases,
  no discarded `Close()` errors, no `nolint`, no `--no-verify`. No panics in library code —
  invariant violations return `ErrInvariantViolation` fail-closed. Postgres errors are matched
  by SQLSTATE (`errors.As` → `*pgconn.PgError`, `.Code`), never by message text.
- Goroutines: every goroutine has an owner, a bounded lifetime, and a stop path — flag
  fire-and-forget `go func()`. Core logic takes time from an injected clock — flag inline
  `time.Now()`/`time.Sleep` in core packages.
- Comments describe *what* and *why*, never history — flag bug/PR references,
  "previously X" notes, and counts or thresholds that will go stale. Log messages state what
  *will* happen, not what *might*. No internal company details (cluster names, hostnames,
  org names) in code, comments, commits, or PRs.
- Tests: real PostgreSQL for core logic (no mocked-DB tests), testify, `t.Context()`
  (cleanups use `context.WithoutCancel(t.Context())`), named polling deadlines — flag bare
  `time.Sleep` readiness waits and any timeout increase that masks a flake instead of fixing
  the root cause.
- CI coverage: behavior that varies by PostgreSQL major must be exercised across the
  supported matrix (14–18), not just the default version.
