# Design principles

The canonical list of principles that govern the engine. They are distilled from Spirit's
philosophy (see spirit-architecture-notes.md) and
adapted for PostgreSQL. Everything in [low-level-design.md](low-level-design.md) and
the phased build plan should be traceable back to one of these.

> Principles say what we value; the **testable runtime MUST-statements** that follow from them —
> each with an enforcement point, a source, and a per-phase test obligation — live in the
> [invariant registry](invariants.md).

## Table of contents

- [Guiding philosophy (derived from proven OSS tools)](#guiding-philosophy-derived-from-proven-oss-tools)
- [Correctness and safety](#correctness-and-safety)
- [Classify-first (leverage native PostgreSQL)](#classify-first-leverage-native-postgresql)
- [Declarative, review-first workflow](#declarative-review-first-workflow)
- [PostgreSQL / Aurora-specific](#postgresql--aurora-specific)
- [Code and dependency maxims](#code-and-dependency-maxims)
- [Process and delivery](#process-and-delivery)

## Guiding philosophy (derived from proven OSS tools)

- **Safety over speed.** The consequences of a bug are data loss in production systems, so a
  feature must be *safe* and *safe-by-default* before it is fast. Speed work (parallelism,
  watermark optimization) only lands on top of a proven-correct path.
- **Decisions, not options.** Prefer sensible defaults over configuration knobs; non-default
  options are poorly tested and a source of surprise. The engine should make the right call
  for the user rather than expose another flag.
- **Mirror the design philosophy, not the package layout.** We port *how Spirit thinks* (the
  lifecycle, the gates, the refusals), not its directory structure — the code follows whatever
  is idiomatic for a Postgres + logical-decoding tool.
- **Operator mental-model parity with Spirit.** Teams that already operate Spirit for MySQL carry its operator model. This engine deliberately mirrors Spirit's *operator surface* — the same lifecycle
  stages, the same verbs (dry-run, defer-cutover, pause/resume, throttle, abort), the same
  refusal semantics, and the same status/observability shape — so an operator carries **one
  mental model across both engines**. Runbooks, incident response, and intuition transfer; the
  PostgreSQL-specific machinery (logical slots, `CONCURRENTLY`, `NOT VALID`) is encoded by the
  engine, not relearned by the operator. This parity is itself a reason to build rather than
  adopt a tool with a different operational shape (see
  tool-pg_osc.md).

## Correctness and safety

- **The checksum is a mandatory, non-skippable cutover gate.** A migration that cannot prove
  the shadow table equals the source **must refuse to cut over**. Correctness is never traded
  for completion.
- **Bound every exclusive lock.** Every `ACCESS EXCLUSIVE` (only the cutover swap in the happy
  path) and every catalog-flip runs under `lock_timeout` + bounded retry/backoff, so the
  engine never sits at the head of the lock queue and amplifies one slow transaction into an
  outage (see 12-mysql-vs-postgresql.md § Why DDL is dangerous: the lock queue).
- **Refuse the unsafe rather than guess.** Lossy conversions, PK changes, FK/trigger tables,
  and ambiguous renames are rejected up front with a clear reason — never silently attempted
  (see [low-level-design's requirements](low-level-design.md#table-requirements-and-unsupported-operations-postgresql-analogs)).
- **Fail safe, leave no mess.** On success, failure, *and* crash, the engine cleans up its
  artifacts — most importantly the replication slot, which otherwise pins WAL and can fill the
  disk (on Aurora, the cluster volume).
- **Preflight before you touch anything.** Validate every prerequisite *before* the first
  write and fail fast with a clear, actionable reason — never abort mid-migration on something
  knowable up front: `rds.logical_replication` / `wal_level = logical`, slot-creation privilege
  (`rds_replication`), a usable primary key and `REPLICA IDENTITY`,
  `max_replication_slots` / `max_wal_senders` headroom, and enough free disk for the shadow copy
  (copy-and-swap roughly **doubles** the table's storage). See
  [low-level-design's preconditions](low-level-design.md#configuration--privilege-preconditions).
- **Safety primitives land reviewed and dormant.** A proof type and its check can merge with
  no production caller: the primitive gets its own focused review and full test coverage, and
  the feature that later consumes it arrives as a smaller, safer diff.
- **Long migrations must be resumable.** A multi-hour/-day copy must survive process restarts:
  persist a durable checkpoint (`{copied-PK watermark, slot name, confirmed LSN}`) and resume
  with minimal lost work rather than restarting from zero — bounded by slot/WAL retention and,
  on Aurora Global Database, by region failover (see
  [low-level-design § design decisions](low-level-design.md#design-decisions-inherited-from-spirit-safety-over-speed)).
- **Bound the work per step — chunk by target time, not row count.** Size each copy/checksum
  chunk to a target duration (~500ms) and adjust it dynamically, so no single statement holds
  resources or drives replication lag unpredictably as row width varies. Fixed row-count
  batches are not used.
- **Reversibility is a property of the pattern, never a fabricated inverse.** copy-and-swap is
  transparent but **not reversible after cutover** — undoing it is a *new forward migration*,
  not an "undo," because the old physical table is gone. Only the expand/contract (pgroll)
  pattern offers true rollback, and only **within the rollout window** (before `complete`,
  while both schema versions are live). The engine therefore exposes a `revert` only where the
  chosen executor genuinely supports it and **refuses otherwise** — it never guesses an inverse
  `ALTER` that could lose data. Forward-fix is the default for the copy-and-swap path. See the
  [execution patterns](high-level-design.md#the-execution-patterns-and-when-each-is-chosen)
  and the orchestrator [revert mapping](schemabot-integration.md#verb-mapping-conceptual).

## Classify-first (leverage native PostgreSQL)

- **Classify before copy.** Parse the change, decide *native-safe sequence* vs *copy-and-swap*
  vs *refuse*, and take the cheapest correct path — the direct analog of Spirit attempting
  INSTANT/INPLACE before falling back to a copy.
- **Classify-first means users get the safe PostgreSQL idiom automatically, without knowing
  which intricacy applies.** A user who asks for an index, a constraint, or a fast-default
  column gets `CREATE INDEX CONCURRENTLY`, `ADD … NOT VALID` then `VALIDATE`,
  `ADD PRIMARY KEY USING INDEX`, or the PG11+ fast default — applied correctly and safely —
  without having to know that idiom exists. The engine encodes the expertise so the user
  doesn't have to.
- **Copy-and-swap is the last resort, not the default.** A full shadow-copy is reserved for
  changes that genuinely have no native online path (general `ALTER COLUMN TYPE`,
  volatile-default `ADD COLUMN`, `STORED` generated columns, repack). "Needs copy-and-swap? =
  No" never means "don't use the engine" — it means the engine runs the native idiom for you.
- **Advise, never silently run the dangerous literal; force is loud and explicit.** When a
  submitted statement is risky as written but has a safer native form (`CREATE INDEX` →
  `CREATE INDEX CONCURRENTLY`, etc.), the engine surfaces the recommendation and applies the
  safe idiom — it does **not** execute the risky literal behind the user's back. The safer
  form reaches the same end state but is not a semantic equivalent — it has different locking,
  transactionality, and failure modes, which is exactly why the engine (not the user) owns
  running it (see the
  [online DDL reference](postgres-online-ddl-reference.md)). Running a
  statement exactly as submitted requires an explicit `--force`, gated by prominent DANGER/CAUTION
  output, a typed acknowledgement (not a bare `-y`), and an audit log entry. Force is an escape
  hatch, not a convenience (see
  [high-level-design's advisory mode](high-level-design.md#advisory-mode-suggest-the-safe-rewrite-dont-silently-run-the-risky-one)).
- **One planner, pluggable execution backends — choose the right pattern per migration.** The
  shared front-end (parse → declarative diff when declarative → classify → route) decides *what* changes and *which strategy*
  fits; interchangeable executors decide *how*: native DDL, log-based copy-and-swap, and
  (later) expand/contract via pgroll for reversible breaking changes. We don't pick one pattern
  globally — the planner routes each migration to the executor whose tradeoffs fit, behind a
  single `Executor` interface (see
  [low-level-design's architecture](low-level-design.md#architecture-decoupled-planner-router-and-executors)).

## Declarative, review-first workflow

- **One pipeline, two front-ends — declarative first.** Build the declarative desired-state
  `diff` as the primary front-end; the imperative `--alter` path is then a thin add-on — the
  **same** parse → classify → route pipeline with the diff step skipped (the user's `ALTER`
  goes straight into the classifier). Both feed the identical executor.
- **Dry-run first.** `diff`/`--dry-run` prints the exact statements and their classification
  (native vs copy-and-swap) **without executing** — the natural review and CI hook.
- **Destructive diffs are gated; renames are never guessed.** Dropping a column/constraint
  requires explicit confirmation; a missing-plus-new column pair is a drop+add unless a rename
  is stated explicitly. Intent is required, not inferred.

## PostgreSQL / Aurora-specific

- **Log-based CDC, not triggers — but cluster-dependently so.** The future copy-and-swap backend
  captures concurrent writes via
  **logical decoding** (a replication slot), which adds near-zero synchronous overhead to the
  source — the key differentiator versus trigger-based tools like pg_osc. A trigger-based path is a
  **first-class fallback** (it survives failover and runs anywhere), not a vestige; the default is
  chosen per cluster, not absolutely — see the
  [change-capture trade-off](change-capture-tradeoff.md). Note neither mechanism removes the
  mandatory checksum.
- **Treat the replication slot as a managed, dangerous resource.** Temporary slot + a
  name-prefixed reaper + a hard slot-lag ceiling; never leave an orphaned slot retaining WAL.
- **Checksums must be deterministic across PostgreSQL quirks.** TOAST (including the
  unchanged-TOAST-on-UPDATE case), `STORED` generated columns, and non-deterministic
  collations must produce identical checksums on source and shadow, or the gate is meaningless.
- **Be Aurora-aware, not Aurora-only.** Throttle on Aurora reader lag, replication-slot lag,
  and WAL generation; use the RDS/Aurora CA bundle and the writer/reader split — while the
  core remains plain-PostgreSQL correct.

## Code and dependency maxims

Code-level rules of thumb. Each earns its place by having a pg-sprite-specific consequence —
this is not a proverb collection. The TCB-scoped versions (with the dependency rubric and the
enforcement mechanics) live in [tcb-model](tcb-model.md); the repo-process versions land in the repo's [`AGENTS.md`](../AGENTS.md).

- **"A little copying is better than a little dependency"** ([Go proverbs](https://go-proverbs.github.io/)) —
  small mechanics (retry/backoff, CA loading, keepalives, tiny helpers) are hand-written or
  copied with attribution, never imported; and specifically, **pg-sprite never imports
  `block/spirit` as a module** — we port ideas with citations, not code. The proverb cuts the
  *other* way for load-bearing expertise: the parser and the wire protocol are taken as pinned
  dependencies, because a hand-rolled substitute there is the unsafe choice. Full rubric:
  [tcb-model § dependencies](tcb-model.md#dependencies-inside-the-tcb-become-part-of-the-tcb).
- **Expose the smallest interface that does the job.** The planned `Executor` contract is
  `Plan`/`Execute`/`Status`/`Abort` and nothing more; packages export domain types and their
  validating constructors, not internals — the narrow interface is what keeps the
  [TCB boundary](tcb-model.md#the-boundary) small enough to audit.
- **Clear is better than clever.** No clever SQL, no dense compound predicates, no control flow
  that requires reconstructing the state machine in your head.
  During an incident this code is read under stress; readability is a safety property here, not
  taste — it is priority #2 in the [TCB ordering](tcb-model.md#rules-inside-the-tcb), above
  ease of use and performance.
- **Minimize state; derive rather than store.** If a value can be recomputed from the database
  or the checkpoint, don't persist it; small state is what makes an incident reasoned about by
  hand. The checkpoint carries the minimum resumable set and nothing else
  ([invariants ST-1](invariants.md#st-1--the-checkpoint-is-a-single-row-written-atomically)).

## Process and delivery

- **Test-first, against a real database.** Write the failing test before the implementation;
  core logic is validated by integration tests against a real Postgres, not mocks — and a
  phase is "done" only when the migration result is validated end-to-end (row counts,
  checksum, lock behaviour).
- **Each increment is independently useful.** The build is sequenced so that early phases
  (classify/print, then native-path execution) ship value on their own, and the highest-risk
  components (CDC, cutover) are added last on a proven foundation (see
  build-plan.md).
