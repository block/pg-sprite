# Low-level design: a decoupled schema-migration engine for Aurora PostgreSQL

> **This is the detailed / low-level design** — package layout, the `Executor` interface,
> library choices, the copy-and-swap lifecycle internals, the full coverage matrix, table
> requirements, and the open decisions to settle before writing core code. For the conceptual
> overview (the problem, the three-layer architecture, when each pattern is used) start with
> the **[high-level design](high-level-design.md)**. This doc is what you read when
> designing the interfaces and packages.

Working name: **`pg-sprite`**. It is a **separate, purpose-built PostgreSQL tool**, not a port of
[Spirit](https://github.com/block/spirit) and not "Spirit with PostgreSQL support" (Spirit stays
MySQL-only — too many MySQL-isms to retrofit cleanly). The goal is a **decoupled planner → router →
executor** engine in which **copy-and-swap** is only one of several execution strategies. pg-sprite
**derives design practices from several tools**: the copy-and-swap lifecycle and operator model
from Spirit, the shadow-table approach from pg-osc/pg_repack, and the expand/contract executor
from pgroll. See spirit-architecture-notes.md
for how the Spirit original works and tool-pgroll.md for pgroll.

## Table of contents

- [Architecture: decoupled planner, router, and executors](#architecture-decoupled-planner-router-and-executors)
  - [Proposed architecture (end-to-end)](#proposed-architecture-end-to-end)
  - [Routing view (which executor handles what)](#routing-view-which-executor-handles-what)
  - [How the planner understands DDL (decided)](#how-the-planner-understands-ddl-decided)
  - [Why this is the right shape](#why-this-is-the-right-shape)
  - [The honest tradeoffs (why this is an *option*, not a free win)](#the-honest-tradeoffs-why-this-is-an-option-not-a-free-win)
  - [v1 stance](#v1-stance)
- [Declarative mode (desired-state schema diff)](#declarative-mode-desired-state-schema-diff)
- [Advisory mode and the force escape hatch](#advisory-mode-and-the-force-escape-hatch)
- [Copy-and-swap executor: lifecycle](#copy-and-swap-executor-lifecycle)
- [Copy and apply ordering (the core correctness subtlety)](#copy-and-apply-ordering-the-core-correctness-subtlety)
- [Coverage and limitations (does this cover all of Aurora PostgreSQL?)](#coverage-and-limitations-does-this-cover-all-of-aurora-postgresql)
- [Table requirements and unsupported operations (Aurora PostgreSQL analogs)](#table-requirements-and-unsupported-operations-aurora-postgresql-analogs)
- [Illustrative component layout (one possible structure, not a goal)](#illustrative-component-layout-one-possible-structure-not-a-goal)
- [Library choices (Go)](#library-choices-go)
- [Design decisions inherited from Spirit (safety over speed)](#design-decisions-inherited-from-spirit-safety-over-speed)
- [Postgres-specific risks to design around](#postgres-specific-risks-to-design-around)
- [Failover during migration: what survives and what doesn't](#failover-during-migration-what-survives-and-what-doesnt)
- [Open decisions (need a call before writing core code)](#open-decisions-need-a-call-before-writing-core-code)
  - [1. CDC mechanism — logical decoding vs triggers vs both](#1-cdc-mechanism--logical-decoding-vs-triggers-vs-both)
  - [2. Scope of v1](#2-scope-of-v1)
  - [3. Repo location / language](#3-repo-location--language)
  - [4. Expand/contract (pgroll) as a second execution backend](#4-expandcontract-pgroll-as-a-second-execution-backend)
  - [5. Declarative diff engine — build on go-pgquery vs wrap pg-schema-diff](#5-declarative-diff-engine--build-on-go-pgquery-vs-wrap-pg-schema-diff)
- [Next step](#next-step)

## Architecture: decoupled planner, router, and executors

This is **not** a single-purpose Spirit port. The system is deliberately split into three
decoupled layers, so that copy-and-swap is only *one* of several interchangeable execution
strategies rather than the whole product:

- **Planner** — parse the change (imperative `--alter` or declarative desired-state diff),
  introspect the live schema, and **classify** every operation as *native-safe*,
  *needs-rewrite*, or *refuse*. The planner decides **what** must change. It has no idea
  *how* any executor works.
- **Router** — given the classified plan plus policy and cluster facts (reversibility
  required? app schema-version aware? logical replication available? table shape?), **choose
  the executor** for each change. The router decides **which strategy**, and is the single
  place migration policy lives.
- **Executors** — interchangeable implementations behind one `Executor` interface
  (`Plan`/`Execute`/`Status`/`Abort`) that decide **how**: `native` DDL,
  **copy-and-swap** (the heavy rewrite executor), and **expand/contract** (pgroll-derived, reversible).
  New strategies can be added without touching the planner or router.

Spirit informs the *philosophy* (safety over speed, checksum gate, decisions-not-options)
and the *copy-and-swap executor specifically* — not the overall shape. The `decode.Source`
seam inside the copy-and-swap executor is the same idea applied one level down.

### Proposed architecture (end-to-end)

```
        user: --alter "..."  OR  --desired schema.sql
                         │
        ╭────────────────▼─────────────────────────────────────────────────────╮
        │  CLI  (Kong)   migrate · diff · fmt · lint · status                  │
        ╰────────────────┬─────────────────────────────────────────────────────╯
                         │
   ┌─────────────────────▼──────────────────── PLANNER / front-end (shared) ────┐
   │  pkg/statement   parse ALTER/CREATE (go-pgquery)                           │
   │  pkg/schemadiff  introspect live schema → diff vs desired → ordered ALTERs │
   │  classifier      per op: native-safe | needs-rewrite | refuse              │
   │  pkg/lint        reject unsafe/unsupported up front                        │
   │      │                                                                     │
   │      ▼  Plan (ordered steps, classified per operation)                     │
   └──────┬─────────────────────────────────────────────────────────────────────┘
          ▼
   ┌──────────────────── ROUTER  (pick an executor per change) ─────────────────┐
   │  policy + cluster facts: reversibility? app version-aware? logical repl?   │
   │  table shape? → native now; copy-and-swap / expand-contract in later phases│
   └──────┬─────────────────────────────────────────────────────────────────────┘
          │  pkg/executor — Executor{ Plan, Execute, Status, Abort }
   ╭──────┴───────────────┬──────────────────────────────┬─────────────────────╮
   ▼                      ▼                               ▼                     ▼
 native                copy-and-swap (Pattern A)       expand/contract        refuse with
 executor              executor — later phase           via pgroll (later)     verdict
 ┌──────────────┐      ┌───────────────────────────-┐   ┌──────────────────┐
 │CONCURRENTLY  │      │1 create shadow table       │   │versioned views,  │
 │NOT VALID +   │      │2 pkg/decode  logical slot  │   │dual schema, app  │
 │  VALIDATE    │      │3 pkg/copier  chunked copy  │   │coordinated,      │
 │USING INDEX   │      │4 pkg/applier ON CONFLICT   │   │reversible        │
 │fast default  │      │5 pkg/checksum gate         │   └──────────────────┘
 │(lock_timeout │      │6 cutover: ACCESS EXCLUSIVE │
 │ + retry)     │      │   swap (lock_timeout+retry)│
 └──────┬───────┘      │  + checkpoint / resume     │
        │              └─────────────┬──────────────┘
        │                            │
   ┌────┴────────────────────────────┴──── cross-cutting ──────────────────────┐
   │  pkg/dbconn   pgx pool · TLS/RDS CA · pg_terminate_backend · retries      │
   │  pkg/throttler   Aurora reader lag · replication-slot lag · WAL gen       │
   └────┬──────────────────────────────────────────────────────────────────────┘
        │
   ╭────▼──────────────────────── Aurora PostgreSQL ───────────────────────────╮
   │  WRITER  (DDL + copy + cutover; logical replication slot lives here)      │
   │  READERS (lag signal for throttling; reached via reader endpoint)         │
   ╰───────────────────────────────────────────────────────────────────────────╯
```

### Routing view (which executor handles what)

```
                ╭──────────────────────────────────────────────╮
                │  Planner (shared): parse · introspect ·      │
                │  declarative diff · classify · lint/refuse   │
                ╰───────────────────────┬──────────────────────╯
                                        │ Plan
                                        ▼
                ╭──────────────────────────────────────────────╮
                │  Router: pick an executor per change         │
                ╰───────────────────────┬──────────────────────╯
                                        │ routes each change to the best executor
        ╭───────────────┬───────────────┼─────────────────────────┬─────────────╮
        ▼               ▼               ▼                         ▼             ▼
   native DDL     log-based         expand/contract            (future          refuse /
   executor       copy-and-swap     via pgroll                  backends)        manual
   CONCURRENTLY   executor          (Pattern B, reversible)
   NOT VALID …    (Pattern A)
```

### How the planner understands DDL (decided)

The DDL-understanding mechanism — the equivalent of Spirit's TiDB parser — is a **layered
hybrid**, decided deliberately rather than inherited, mapping each function to the mechanism
that serves it best:

- **Classification** parses with the real PostgreSQL grammar via
  [`wasilibs/go-pgquery`](https://github.com/wasilibs/go-pgquery) — `libpg_query` compiled to
  WebAssembly, executed in-process by wazero. Same grammar as the server, pure-Go builds (no
  cgo toolchain), and a parser crash is a recoverable Go error rather than a process-wide
  segfault — containment that matters in a shared process owning in-flight migrations. The Wasm
  module is embedded at build time (`go:embed`); nothing is downloaded at runtime. The cgo
  [`pg_query_go`](https://github.com/pganalyze/pg_query_go) is the documented escape hatch —
  API-compatible, a one-file swap.
- **Shadow-table DDL and checkpoint fingerprints** come from **execute-and-introspect**: apply
  the change inside a rolled-back transaction on the engine-owned
  [scratch database](#plan-time-prerequisite-the-scratch-database) hydrated to the
  before-schema, then read the canonical after-state back from the catalogs (`pg_get_*def`).
  No AST surgery — correctness of the after-schema is delegated to PostgreSQL itself, and the
  resume fingerprint (ST-2) hashes the introspected model, not SQL text.
- **Refusals (RF-1..5)** use both layers: parse-level lint for what the AST shows (dangerous
  literals, ambiguous renames), scratch execution for semantic truth (syntax *and* semantics,
  with the server's own SQLSTATEs). Refusal messages quote the server error where one exists.

Classification still *predicts* lock/rewrite behaviour — an empty scratch table reveals nothing
about a 2 TB rewrite — so execute-and-introspect complements the classifier, never replaces it.
A structured operations DSL (pgroll-style) and catalog-snapshot diffing are noted as possible
future *additional* front doors for declarative mode, not the primary.

### Why this is the right shape

This is exactly the answer to *"why build only copy-and-swap when pgroll already wins some
cases?"* — **we don't have to choose globally.** A shared planner lets us pick the best
pattern *per migration*:

- **native** for the majority (the ➖/❌ rows in [postgres-online-ddl-reference](postgres-online-ddl-reference.md));
- **log-based copy-and-swap** for transparent, heavy physical rewrites (`int→bigint`, repack)
  where the change is invisible to the app — see tool-pgroll's comparison;
- **expand/contract via pgroll** for prod-critical breaking changes where **instant
  reversibility** and **two live schema versions** matter more than transparency.

The classifier, declarative diff, linting, dry-run, and status reporting are written **once**
and shared by every backend. An `Executor` interface (`Plan`, `Execute`, `Status`, `Abort`)
is the contract; native is the first implementation. Until the in-house copy-and-swap executor
lands in a later phase, every `needs-rewrite` change is refused as **not native-safe** rather than
delegated to an external tool. pgroll remains a possible still-later backend.

### The honest tradeoffs (why this is an *option*, not a free win)

Routing to pgroll buys reversibility, but the patterns are not silently interchangeable — the
**operational contract differs by backend**, so the choice surfaces to the user:

- **App-awareness is pattern-intrinsic, not a tool feature.** copy-and-swap is transparent
  (same table name, no app changes); pgroll's reversibility *requires* the app to be
  schema-version aware (`search_path`, two versions live, `start`→rollout→`complete`). You get
  reversibility **or** transparency per migration — not both at once — because they are
  properties of the chosen pattern.
- **Reversibility is bounded to the rollout window.** pgroll's instant rollback applies
  *before* `complete`; once contracted, rolling back is another migration — same as
  copy-and-swap. The benefit is real but time-boxed.
- **Two lifecycles to unify.** copy-and-swap is one-shot; pgroll is a stateful
  start/complete/rollback machine with its own migration history. A shared `status`/resume
  layer must model both, which is real complexity.
- **Dependency coupling.** pgroll is Go (reusable), but wrapping it (library or subprocess)
  brings its version surface, its trigger-based backfill, and its error model along.
- **Tension with "decisions, not options."** A second pattern is a real user-facing choice. To
  stay faithful to the philosophy it needs a **clear default and a narrow, well-signposted
  opt-in** (e.g. auto-route, with `--strategy=expand-contract` only when reversibility is
  explicitly requested), not a bare menu of equal options.

### v1 stance

Build the **planner + router + native executor** first. A `needs-rewrite` result is a first-class
**not native-safe** refusal, with the reason, a note that in-house copy-and-swap arrives in later
phases, and a safer native alternative where one exists. It is never delegated to an external
copy tool. Design the `Executor` interface from day one so copy-and-swap and, later,
expand/contract can be added without reworking the front-end.

## Declarative mode (desired-state schema diff)

The engine supports **two ways to express a change**, mirroring Spirit's imperative
(`migrate --alter`) and declarative (`diff` / `fmt` over canonical schema files) front-ends:

- **Imperative** — the user supplies the `ALTER` directly.
- **Declarative** — the user supplies the **desired end-state** as a `CREATE TABLE` (typically
  a checked-in `.sql` schema file), and the engine **derives the `ALTER`** by diffing it
  against the live table. This is the analog of Spirit's `diff`/declarative schema workflow.

Declarative mode is a **front-end that produces statements**, which then flow into the exact
same pipeline as imperative input (classify → native-safe DDL, or shadow-table copy):

```
desired CREATE TABLE (file)  ─┐
                              ├─▶  diff engine  ─▶  derived ALTER / CREATE INDEX / ...
live schema (introspected)  ─┘                          │
                                                        ▼
                              (same as imperative)  classify ─▶ native DDL  | shadow copy
```

### How the diff is derived

1. **Parse desired state** with `go-pgquery` into a normalized table model (columns, types,
   defaults, nullability, identity/sequences, constraints, indexes).
2. **Introspect live state** from the catalogs (`pg_attribute`, `pg_constraint`, `pg_index`,
   `pg_attrdef`, …) into the same normalized model.
3. **Canonicalize both** so the comparison ignores cosmetic differences — type aliases
   (`int4` ↔ `integer`, `varchar` ↔ `character varying`), default formatting, column order
   where it doesn't matter, implicit names. (This is what a `fmt` subcommand also does to a
   schema file on its own.)
4. **Diff** the two models and emit the minimal set of statements: `ADD/DROP/ALTER COLUMN`,
   `ADD/DROP CONSTRAINT`, `CREATE/DROP INDEX`, default/nullability changes, etc., in a
   **dependency-correct order** (e.g. add a column before an index that references it).
   Columns are compared **by name**: a live table whose columns are ordered differently from
   the desired file converges to "no changes". Attribute order carries no semantics in
   PostgreSQL and cannot be changed in place, so — unlike some declarative MySQL tooling —
   column order is deliberately out of scope for convergence.
5. **Hand the derived statements to the same classifier**, so a declarative change that turns
   out to be, say, a binary-coercible type widening still takes the native fast path, and only
   a genuine rewrite triggers a copy.

### Safety rules (inherited philosophy: surprise-free, decisions-not-options)

- **Destructive diffs are gated.** Dropping a column, constraint, or index — anything that
  loses data or a guarantee (a unique index discards the same uniqueness guarantee as a unique
  constraint) — requires an explicit confirmation flag — never inferred silently from "it's
  missing in the desired file".
- **Unsupported constructs are refused, never guessed.** The desired file admits one
  unqualified `CREATE TABLE` plus `CREATE INDEX` statements on it; each rule is a typed error.
  Foreign keys are refused at admission — a `REFERENCES` clause cannot be faithfully executed
  in the transaction-scoped scratch schema (an unqualified reference resolves against the
  scratch search_path, not the target schema), and FK support needs its own design. Changes
  the plan cannot express — identity or generation changes on an existing column, adopting a
  sequence-backed (serial) default whose sequence only existed in the rolled-back scratch
  transaction — are refused as unsupported rather than emitted as an unexecutable plan.
- **Renames are ambiguous and are not guessed.** A column present in live but absent in desired
  plus a new column in desired is, by default, a *drop + add*, not a rename. Rename intent must
  be stated explicitly (the engine will not heuristically pair columns), mirroring Spirit's
  refusal to auto-handle dangerous rename patterns.
- **Dry-run first.** `diff` prints the derived statements (and whether each takes the native or
  copy path) without executing — the same review step as Spirit, and the natural hook for CI.
- **Out-of-band drift is surfaced, not steamrolled.** If the live table differs from what the
  desired file's base assumed, the diff makes that visible rather than blindly forcing the
  end state.

### Why this matters

Declarative mode lets schemas live as **reviewed, version-controlled `.sql` files** and lets
CI compute "what would change" — while reusing the entire safe execution path (classify,
native-vs-copy, checksum, cutover). It is purely additive: the imperative `--alter` path
remains the primitive that everything ultimately runs through.

### Build or wrap?

[stripe/pg-schema-diff](https://github.com/stripe/pg-schema-diff) already implements most of
this front-end: introspection, canonicalization (by applying the desired DDL to a **temp
database** and letting the server itself canonicalize), dependency-ordered emission of the same
safe idioms, per-statement timeouts, typed **hazard annotations**, and **plan validation**
against the temp database. Whether `pkg/schemadiff` wraps it or builds on `go-pgquery`
directly is
[open decision #5](#5-declarative-diff-engine--build-on-go-pgquery-vs-wrap-pg-schema-diff).
Either way its output flows through our classifier and executors unchanged — planner output is
a request, not a permission.

## Advisory mode and the force escape hatch

The [advisory behaviour](high-level-design.md#advisory-mode-suggest-the-safe-rewrite-dont-silently-run-the-risky-one)
is a property of the **planner's classifier output**, not a separate code path. Every classified
operation carries a recommendation, and the CLI decides whether to apply it, suggest it, or
refuse based on mode and flags.

### What the classifier emits per operation

For each parsed statement the classifier produces a record along the lines of:

- `original` — the statement as the user wrote it.
- `class` — `native-safe` · `needs-rewrite` (refused until in-house copy-and-swap lands) ·
  `refuse`.
- `recommended` — the safe rewrite when the literal is risky but has a native equivalent
  (e.g. `CREATE INDEX` → `CREATE INDEX CONCURRENTLY`; `ADD CONSTRAINT` → `ADD … NOT VALID` +
  `VALIDATE`; `ADD PRIMARY KEY` → unique index `CONCURRENTLY` + `ADD PRIMARY KEY USING INDEX`).
- `risk` — what the literal would do (lock mode held, whether it blocks reads/writes, expected
  duration class) and *why* the recommendation is safer.
- `reversible` / `requires_app_coordination` — populated when a pattern other than native is in
  play (feeds the router).

This is what `pkg/lint` and `pkg/statement` already need to compute in order to route; advisory
mode just **surfaces** it instead of consuming it silently.

### CLI behaviour (modes)

| Invocation | Behaviour |
| --- | --- |
| `suggest` / `lint` / `diff --dry-run` | Print `original` → `recommended` + `risk` for every op. **Never executes.** Exit non-zero if any op needs a riskier path than policy allows (the CI gate). |
| `migrate` (default) | If a safer `recommended` form exists, **apply the recommended idiom** (classify-first) and report what was substituted. If the literal is not native-safe, **refuse** with the reason, a later-phase copy-and-swap note, and a safer native alternative where one exists. The dangerous literal is **never** run by default, and no external copy tool is invoked. |
| `migrate --force` | Run each statement **exactly as submitted**, bypassing the safe rewrite. Gated — see below. |

The distinction the user cares about: a plain `CREATE INDEX` is never executed verbatim by
default. The engine either applies `CREATE INDEX CONCURRENTLY` for you (and says so) or, in
`suggest`/dry-run, hands back the recommendation without touching the database.

### The `--force` gate

`--force` is deliberately high-friction:

1. Print a prominent **DANGER / CAUTION** block: the exact statement, the lock it will take, what
   it blocks (reads? writes?), and the expected/worst-case duration and lock-queue impact
   (cross-link to 12-mysql-vs-postgresql.md § the lock queue).
2. Require an **explicit typed acknowledgement** (e.g. type the table name, or
   `--i-understand-the-risk`), not a bare `-y`/`--yes`.
3. Still wrap the statement in `lock_timeout` + bounded retry unless the user *also* opts out of
   that explicitly (a second, separate flag) — force means "run my statement", not "remove every
   guardrail".
4. **Log the override** (who, when, what statement, what the recommendation was) for audit.

Force exists for the rare legitimate case (e.g. a maintenance window where the table is known
idle and a plain rewrite is acceptable); it is an escape hatch, not a shortcut, consistent with
*decisions, not options*.

## Copy-and-swap executor: lifecycle

> This section details the later-phase **copy-and-swap executor**, the planned in-house heavy
> strategy for genuine table rewrites. Until it lands, those rewrites receive a **not
> native-safe** refusal. The `native` and `expand/contract`
> executors are described in the architecture section and in
> tool-pgroll.md. The per-primitive **Spirit (MySQL) → Aurora
> PostgreSQL mapping** this executor is built on lives in
> 12-mysql-vs-postgresql.md § primitive mapping.

```
+----------------------------------------------------------------------+
| 1. Parse ALTER. If it maps to a SAFE native pattern                   |
|    (CONCURRENTLY / NOT VALID+VALIDATE / fast-default / USING INDEX /  |
|     binary-coercible type change) -> run it directly. Done.           |
|    (= Spirit's "attempt INSTANT/INPLACE")                             |
+----------------------------------------------------------------------+
                              | otherwise (table rewrite needed)
                              v
+--------------+  +-------------------+  +------------------+  +---------------+
| 2. Create    |  | 3. Start CDC:     |  | 4. Chunked copy  |  | 5. Drain CDC  |
| shadow table |->| logical slot      |->| source->shadow,  |->| backlog +     |
| w/ new schema|  | (snapshot LSN) +  |  | N parallel       |  | checksum gate |
|              |  | buffer changes    |  | workers, dynamic |  | (correctness) |
|              |  | from snapshot LSN |  | chunk sizing     |  |               |
+--------------+  +-------------------+  +------------------+  +-------+-------+
                                                                      v
                                        +-------------------------------------+
                                        | 6. Cutover (one transaction):       |
                                        |   SET lock_timeout                  |
                                        |   LOCK src ACCESS EXCLUSIVE         |
                                        |   final CDC drain                   |
                                        |   RENAME swap                       |
                                        |   COMMIT                            |
                                        |   then: recreate FKs referencing    |
                                        |   src, fix sequence ownership, drop |
                                        |   old table, drop slot              |
                                        +-------------------------------------+
```

Key correctness gate (same as Spirit): the **checksum must pass before cutover**. With
`--defer-cutover`, a continuous checksum loop runs while waiting on a sentinel, exactly like
Spirit's deferred-cutover mode.

## Copy and apply ordering (the core correctness subtlety)

The copier and the applier write the **same shadow rows concurrently**, and their interleaving —
not the decoding, not the SQL — is where a copy-and-swap engine is most likely to be silently
wrong. This is where Spirit/gh-ost carry their most delicate logic, and the protocol must be
specified and tested explicitly
(build-plan Phase 6), not left
implicit in the implementation. The races to design against:

- **Stale-image overwrite.** A chunk read from the source at time T₁ lands in the shadow *after*
  the applier already applied a newer captured change for the same row. If the copier overwrites,
  the shadow regresses to the older image.
- **Ghost-row resurrection.** The copier reads a row, the applier applies that row's `DELETE`,
  then the chunk insert lands — re-inserting a row that no longer exists on the source.
- **The same races during reconciliation.** The checksum-repair pass after
  [slot loss](#failover-during-migration-what-survives-and-what-doesnt) re-copies divergent
  chunks while the new slot's stream is being applied — the same two races, a second exposure.

The invariants that resolve them (Spirit's model, translated):

- **The copier never overwrites**: chunks land with `INSERT … ON CONFLICT (pk) DO NOTHING`, so a
  newer applied image is never regressed (the `INSERT IGNORE` analog).
- **The applier always overwrites**: captured changes land with
  `INSERT … ON CONFLICT (pk) DO UPDATE` plus explicit `DELETE` handling (the `REPLACE INTO`
  analog), keyed by PK and deduplicated to the latest image per key before each flush.
- **The watermark orders the two**: captured changes for PK ranges the copier has already passed
  are applied; changes *above* the copier's watermark can be discarded for a monotonic integer PK
  (the copier will read the current row anyway — the high-watermark optimization) and must be
  queued for composite / non-memory-comparable PKs.
- **Deletes must not be lost to an in-flight chunk**: a delete for a key inside a chunk that is
  currently being copied must be re-applied *after* that chunk lands (tombstone retention until
  the covering chunk completes), or chunk copy and backlog flush must be mutually excluded per
  overlapping key range.

The **mandatory checksum remains the backstop, not the mechanism** — it catches a protocol bug
before cutover, but the protocol must converge without it. The precise rule set (flush scheduling
vs chunk boundaries, tombstone lifetime, the composite-PK queue) is a Phase 6 deliverable with a
dedicated convergence test per race above. The trigger fallback has its own analog of this race —
see risks-and-mitigations § trigger-specific risks; this
section is the logical-decoding counterpart.

## Coverage and limitations (does this cover all of Aurora PostgreSQL?)

**No — and no single tool does.** The one-line pitch ("multi-threaded chunked copy +
log-based CDC + checksum-gated atomic cutover + checkpoint/resume, tuned for Aurora")
describes the *happy-path mechanism*, not universal coverage of every Aurora PostgreSQL
deployment topology, configuration, and schema shape. Being explicit about the supported
matrix is part of the "decisions, not options" philosophy.

### Deployment topologies

| Topology | v1 support | Notes |
| --- | --- | --- |
| Aurora PostgreSQL provisioned (1 writer + N readers) | ✅ Primary target | Logical decoding runs against the **writer** endpoint |
| Aurora Serverless v2 | ✅ With caveats | Logical replication supported; heavy copy can drive ACU scaling/cost; pin min ACUs |
| Aurora Serverless v1 | ❌ | Logical replication not available; deprecated |
| Aurora Global Database | ⚠️ Writer region only | Slots exist only on the primary region writer; a region failover invalidates the slot → resume not possible across regions |
| RDS PostgreSQL (non-Aurora) | ✅ Bonus | Same engine + `rds.logical_replication`; the engine should work unmodified |
| RDS Proxy in front of the cluster | ⚠️ | The **replication** connection must go **direct** to the instance endpoint, not through the proxy (proxy doesn't support the replication protocol / pinning) |
| Babelfish (TDS/SQL-Server surface) | ❌ | Out of scope |
| Blue/Green Deployments | ⚠️ | Conceptually overlapping; running both at once needs care around slots/triggers |

### Configuration / privilege preconditions

| Precondition | Required for | If absent |
| --- | --- | --- |
| `rds.logical_replication = 1` (static → reboot) ⇒ `wal_level = logical` | logical-decoding CDC path | Fall back to **trigger-based** CDC |
| `rds_replication` role granted (Aurora gives no `SUPERUSER`) | creating slot / starting replication | Use trigger fallback, or request the grant |
| Ownership / `CREATE` on the schema | shadow table, triggers, swap | Migration cannot run |
| `max_replication_slots` / `max_wal_senders` headroom | concurrent migrations | Serialize migrations |
| `REPLICA IDENTITY` = PK (default) or `FULL` | correct UPDATE/DELETE capture; unchanged-TOAST columns | v1 requires a PK, so default identity suffices |

### Schema shapes

| Schema feature | v1 | Notes |
| --- | --- | --- |
| Single-column integer/`bigint`/`identity` PK | ✅ Fast path | Best watermark + chunking |
| Composite or `uuid`/`text` PK | ✅ Slower path | Composite chunker; weaker watermark optimization |
| **No** primary key / no unique-not-null key | ❌ | Required, same constraint as Spirit |
| Foreign keys **referencing** the table | ❌ v1 | FKs must be re-pointed at cutover — defer to v2 |
| Triggers on the table | ❌ v1 | Must be recreated on the shadow with correct ordering |
| Views defined on the table | ❌ v1 | PostgreSQL binds a view to the table's **OID**, not its name — after the rename-swap the view silently follows the renamed *old* table. Refuse in v1 (pg_repack avoids this only by swapping relfilenodes under one OID; recreating views inside the cutover txn is later work) |
| Table is in a logical **publication** | ❌ v1 (preflight refusal) | Publication membership is also OID-bound; after the swap, downstream consumers (DMS, CDC pipelines) silently stop receiving the real table's changes |
| `STORED` generated columns | ⚠️ | Copy must omit them and let them recompute; checksum must account for them |
| Partitioned (declarative) tables | ❌ v1 | Root vs leaf publication, `publish_via_partition_root`, per-partition swap — complex |
| Large objects (`pg_largeobject`) | ❌ | Not represented in table-level logical decoding |
| Exotic types / domains / non-deterministic collations | ⚠️ | Must produce a deterministic checksum on both source and shadow |

### Operational caveats

- **Unchanged-TOAST on UPDATE**: with default replica identity, an `UPDATE` that doesn't
  touch a TOASTed column won't emit that column's value. The applier must handle this (carry
  forward, or use `REPLICA IDENTITY FULL`) or the shadow can diverge — the checksum is the
  backstop, but design for it explicitly.
- **No DDL during migration**: logical decoding does not stream DDL. Concurrent schema
  changes to the source mid-migration are unsupported and must be blocked.
- **Multi-TB tables**: a multi-day copy means the slot retains WAL for the whole window →
  disk/lag risk (see risks below).
- **Multi-statement / multi-table atomic changes**: out of v1 scope.
- **Shadow-table fidelity beyond columns**: the shadow must explicitly replicate the source's
  **owner, GRANTs/ACLs, row-level-security policies, comments, and storage parameters** — none
  of which comes along by creating a table with the right columns. Miss the grants and
  application roles **lose access at the instant of cutover**. The cutover refuses to swap until
  this fidelity checklist passes; OID-bound dependents (views, publications) are refused up
  front in v1 (see the schema-shape matrix above). The shadow's column definition itself comes
  from [execute-and-introspect](#how-the-planner-understands-ddl-decided) on the scratch
  database, not from AST transformation of the user's `ALTER`.

### What "tuned for Aurora" actually means here

Aurora-specific handling (throttle on Aurora reader replica lag and on replication **slot**
lag, RDS CA bundle for TLS, `pg_terminate_backend` to bound the cutover lock, awareness of
the writer/reader split) — **not** a claim that every Aurora edition/topology above is
covered. The unsupported rows are explicit non-goals for v1.

## Table requirements and unsupported operations (Aurora PostgreSQL analogs)

Spirit publishes a short, deliberate list of things it **requires** of a table and things it
**refuses to do** (see [its README](https://github.com/block/spirit#unsupported-features) and
spirit-architecture-notes.md). These are not arbitrary —
each maps to a property the copy/CDC/cutover machinery depends on. Below is the faithful
translation of each constraint to Aurora PostgreSQL, **with the Postgres-specific reason**
(not just "because Spirit does it"). The coverage matrix above states *what* is supported;
this section states *why* and pins the analog to the underlying primitive.

### Table-shape requirements (preconditions to even start)

| Spirit (MySQL) requirement | Aurora PG analog (v1) | Postgres-specific reason |
| --- | --- | --- |
| Table **must have a PRIMARY KEY** | Require a PK, or a `NOT NULL` `UNIQUE` key usable as one | Chunking needs a deterministic, range-scannable key to slice `WHERE pk BETWEEN …`; the applier needs a stable conflict target for `INSERT … ON CONFLICT (pk) DO UPDATE`; resume needs a watermark. No PK ⇒ would need `REPLICA IDENTITY FULL`, full-row matching on apply/delete, and a synthetic `ctid`-based chunker (unstable across `VACUUM`/rewrite) — unsafe for v1. |
| PK should ideally be a single memory-comparable integer | `bigint`/`identity`/`serial` single-column PK is the fast path; composite / `uuid` / `text` PK is the slower path | The high-watermark optimization (discard captured changes above the copier's position) and the optimistic chunker rely on a monotonic, cheaply-comparable key. `uuid`/`text`/collated keys force the composite/queue path, exactly as in Spirit. |
| `binlog_row_image=FULL` (full before/after image) | `REPLICA IDENTITY` = PK (default) is enough for v1; `FULL` only if no PK | PG only logs the replica-identity columns for `UPDATE`/`DELETE` by default; that is sufficient when a PK exists. The **unchanged-TOAST** wrinkle (a TOASTed column not in the update isn't streamed) is the PG-specific gotcha the applier must handle. |

### Unsupported / refused operations (mirror Spirit's blocklist)

| Spirit refuses | Aurora PG engine v1 stance | Postgres-specific reason |
| --- | --- | --- |
| **ALTER / DROP PRIMARY KEY** | Refuse — PK must be unchanged by the migration | The PK is simultaneously the chunk key, the CDC conflict target, and the resume watermark. Changing it mid-flight breaks all three. (A PK *change* can still be done as a separate expand/contract migration.) |
| **FOREIGN KEYS or TRIGGERS on the migrated table** | Refuse in v1 | Inbound FKs (other tables referencing this one) must be re-pointed at cutover under the `ACCESS EXCLUSIVE` window — error-prone and lengthens the lock. Triggers/rules on the source would also have to be recreated on the shadow with exact firing order, and could fire during the copy. Both are deferred, same as Spirit. |
| **RENAME column** (dangerous overlap cases) | Refuse the dangerous cases; allow only simple, unambiguous non-PK renames | A rename that reuses an old name (`RENAME a→b, ADD a …`) makes column identity ambiguous between the source row image and the shadow schema, risking silent data misplacement during apply. Same correctness hazard exists in PG. |
| **Lossy conversions** (shorten `VARCHAR` below longest value, add `NOT NULL` w/o default, add `UNIQUE` on non-unique data) | Refuse; require the data be fixed first | These can fail or truncate *during the copy or the constraint validation*, after work is spent. PG surfaces them as `VALIDATE CONSTRAINT` / cast failures; better to reject up front. |
| Read-replica `<10s` lag fidelity | Not a goal | Like Spirit, the engine prioritizes copy throughput; it observes Aurora reader/slot lag only to throttle and protect DR, not to guarantee replica freshness. |

### Plan-time prerequisite: the scratch database

[Execute-and-introspect](#how-the-planner-understands-ddl-decided) (semantic validation,
shadow-DDL derivation, checkpoint fingerprints) needs a scratch database **on the target
cluster** — server version and extension parity hold by construction, and the storage cost is
schema-only (no data ever lands in scratch). Preflight (ST-6) verifies one of two acceptable
states and refuses with a stated reason otherwise:

1. **`pg_sprite_scratch` is pre-provisioned** (engine-role-owned), or
2. the engine role holds **`CREATEDB`**, so preflight can self-provision it.

The scratch database is engine-owned and disposable: preflight may reset it (drop/recreate
contents) at any time. Restricted environments that won't grant `CREATEDB` pre-provision
instead.

**Plan-time diffing uses a lighter mechanism.** `pkg/schemadiff` materializes the desired
state inside a single always-rolled-back transaction in the *target* database, in a
randomly named transaction-scoped schema (`pgsprite_scratch_<random>`). This keeps the
same-server semantic-truth property (same version, extensions, and defaults as the live
table) while requiring no `CREATEDB`, no pre-provisioning, and leaving zero footprint —
appropriate because diffing is read-only planning. The durable `pg_sprite_scratch`
database above is required only by the migration path proper (shadow-DDL derivation and
checkpoint fingerprints), where objects must outlive a transaction.

### Postgres-only preconditions Spirit has no analog for

These have **no MySQL counterpart** but are hard requirements for the logical-decoding path:

- **`rds.logical_replication = 1`** (⇒ `wal_level = logical`); static, needs a reboot. Without
  it the engine must fall back to trigger-based CDC.
- **`rds_replication` role** (Aurora grants no `SUPERUSER`) to create the slot and start
  replication.
- **Replication-slot / `max_wal_senders` headroom**, and the slot must be on the **writer**
  (and reached **directly**, not via RDS Proxy).
- **No concurrent DDL on the source** during the migration — logical decoding does not stream
  DDL, so a mid-flight schema change to the source is unsupported.

> Net: the v1 supported surface is intentionally close to Spirit's — *single table, has a PK,
> no FKs/triggers, no PK change, no lossy change* — with the Postgres-specific additions of
> logical-replication enablement, slot/role privileges, and the unchanged-TOAST handling.

## Illustrative component layout (one possible structure, not a goal)

> We intend to mirror Spirit's **design philosophy** (see
> [Design decisions inherited from Spirit](#design-decisions-inherited-from-spirit-safety-over-speed)),
> **not** its package layout. The structure below is just one illustrative way to organise
> the components so the responsibilities are clear; the real code will follow whatever is
> idiomatic for a Postgres + logical-decoding tool.

```
cmd/pg-sprite/         -> CLI (migrate, diff, fmt, lint, status) - Kong, like Spirit
pkg/planner/          -> shared front-end: parse/introspect/diff + classify each op -> Plan
pkg/router/           -> picks an Executor per change from policy + cluster facts
pkg/executor/         -> Executor interface (Plan/Execute/Status/Abort) + implementations:
                           native (CONCURRENTLY / NOT VALID …), copyswap (Pattern A),
                           later: expandcontract (wraps pgroll, Pattern B / reversible)
pkg/migration/        -> orchestrator + runner + cutover (drives the copyswap executor)
pkg/decode/           -> logical-decoding client (replaces Spirit's pkg/change/binlog)  <- the hard part
pkg/copier/           -> parallel chunked copy (INSERT...SELECT...ON CONFLICT)
pkg/applier/          -> ON CONFLICT upsert + delete apply
pkg/table/            -> PK-range chunkers (optimistic + composite), dynamic sizing
pkg/checksum/         -> md5/row-text chunked verification
pkg/dbconn/           -> pgx pool, retries, lock_timeout, RDS CA, pg_terminate_backend
pkg/statement/        -> go-pgquery parsing + "is this natively safe?" classifier
pkg/schemadiff/       -> declarative mode: introspect live schema, diff vs desired
                         CREATE TABLE, derive ordered ALTER/CREATE statements (+ fmt)
pkg/lint/             -> unsafe-DDL linters (PG flavored)
pkg/throttler/        -> Aurora PG replica-lag / slot-lag throttle
```

## Library choices (Go)

- **`pgx/v5`** — Postgres driver + connection pool; also exposes the low-level
  `pgconn`/`pglogrepl` building blocks for logical replication.
- **`github.com/jackc/pglogrepl`** — start replication, parse `pgoutput`/`wal2json`
  messages, send standby status (LSN flush) updates. This is the binlog-syncer analog.
- **`github.com/wasilibs/go-pgquery`** — parse `ALTER`/`CREATE TABLE` with the actual Postgres
  grammar (`libpg_query` compiled to Wasm, executed by wazero: pure-Go builds, parser crashes
  contained). Analog of Spirit's TiDB parser. The cgo `github.com/pganalyze/pg_query_go/v5` is
  the API-compatible escape hatch (see
  [How the planner understands DDL](#how-the-planner-understands-ddl-decided)).
- **`github.com/stripe/pg-schema-diff`** *(candidate — open decision #5)* — declarative diff
  engine: introspection + dependency-ordered plan emission with hazard annotations and
  temp-database plan validation; would power `pkg/schemadiff` instead of building the diff on
  `go-pgquery` directly.
- **`github.com/alecthomas/kong`** — CLI, same as Spirit.

## Design decisions inherited from Spirit (safety over speed)

> The full, categorized list lives in
> [design-principles.md](design-principles.md). The decisions below are the concrete
> v1 engineering choices that follow from those principles.

- **Primary key required**; PK cannot be changed by the migration (v1).
- **No FKs / triggers on the migrated table** in v1 (Postgres FKs that reference the table
  being swapped require careful re-pointing — defer to v2).
- **Checksum is mandatory and cannot be skipped** — it is the cutover correctness gate.
- **Dynamic chunking by target time** (default ~500ms), not a fixed row count.
- **Checkpoint/resume**: persist `{last copied PK watermark, slot name, confirmed LSN}` so an
  interrupted migration resumes with ~1 minute of lost work. (Postgres-specific risk: the
  slot must survive; see open decisions.)

## Postgres-specific risks to design around

The full enumeration of risks — **common to any copy-and-swap**, **logical-decoding-specific**,
and **trigger-specific** — together with the **mitigation** for each, has moved to its own register
so it can be maintained as a single source of truth: see
risks-and-mitigations.md. The two most design-shaping ones,
**slot loss on failover** and the reconcile-don't-recopy recovery it forces, are detailed below
because they drive the checkpoint/resume state machine.

## Failover during migration: what survives and what doesn't

> **Short answer to "does a failover mean we resume from scratch?": no for the bulk copy, but
> yes for the logical-decoding catch-up state — on Aurora a failover can cost the slot, and with
> it the incremental progress, forcing a reconciliation pass (not necessarily a full re-copy).**

A copy-and-swap migration has two kinds of progress, and they have very different durability:

| State | Where it lives | Survives Aurora failover? |
| --- | --- | --- |
| **Copied-PK watermark** (how far the bulk copy got) | the engine's durable checkpoint store | ✅ yes — it's our own data |
| **Shadow table contents** | a regular table | ✅ yes — replicated by Aurora storage |
| **Logical slot + `confirmed_flush_lsn`** (CDC position) | the writer's replication slot | ⚠️ **not guaranteed** — see risk #7 |
| **In-flight decode buffer / un-applied changes** | engine memory | ❌ no |

The trap: the slot's LSN is what makes the shadow *trustworthy*. If the slot is lost, you cannot
simply create a new slot and continue — a fresh slot starts at the **current** LSN, leaving a
**gap** of changes between the old `confirmed_flush_lsn` and the new slot, so the shadow is now
silently diverged from the source. So on slot loss the engine has three honest options, in order
of preference:

1. **Reconcile, don't re-copy (preferred).** Keep the shadow and the durable copy watermark,
   create a new slot at the current LSN, then run a **full checksum + repair pass** (re-sync only
   the chunks that differ) before resuming CDC from the new slot. This salvages days of bulk-copy
   work; the cost is one comparison sweep, bounded by *checksum* cost, not *copy* cost. The
   mandatory checksum gate already exists — this reuses it as a repair primitive.
2. **Restart from scratch.** New slot + new snapshot + full re-copy. Always correct, but throws
   away all progress; acceptable only for small tables.
3. **Don't use logical decoding here.** For clusters where failover risk during a multi-day
   migration is unacceptable, route to the **trigger fallback** (open decision #1): the trigger +
   queue table are ordinary data that survive failover, so the migration simply resumes.

Design implications the rest of the system must honour:
- **Model slot loss as a first-class state transition** in checkpoint/resume (build-plan
  Phase 8), distinct
  from a process crash (where the slot *does* survive and we resume cleanly).
- **Detect it:** watch for the slot disappearing / becoming inactive and for writer-identity
  changes, and enter reconcile mode rather than blindly continuing.
- **Bound the blast radius:** the hard slot-lag ceiling and the name-prefixed reaper (risk #1)
  must also clean up the *orphaned* slot a failover can strand on the demoted instance.
- **Make the trade explicit to the operator:** on logical-decoding clusters, a long migration is
  exposed to failover/maintenance windows; the trigger path is the robustness escape hatch.

## Open decisions (need a call before writing core code)

### 1. CDC mechanism — logical decoding vs triggers vs both

- **Logical decoding** (recommended primary): faithful Spirit port, **no synchronous write
  overhead** on the source; this is what makes the tool better than pg_osc. Costs: needs
  `rds.logical_replication=1` (reboot), a slot (WAL-retention risk), `REPLICA IDENTITY`, and the
  failover/TOAST/DDL wrinkles in the
  logical-decoding-specific risks.
- **Triggers** (pg_osc style): no parameter/reboot, works anywhere; **survives failover** (the
  queue table is ordinary data). The cost is not just write amplification: capture lives **inside
  the write path**, so a trigger error can abort the application's write (an availability coupling),
  and writes made under `session_replication_role = replica` silently bypass capture — see the
  trigger-specific risks. It is the
  robustness escape hatch for clusters that can't enable logical replication or can't accept
  [slot loss on failover](#failover-during-migration-what-survives-and-what-doesnt) during a
  multi-day migration — not a strictly-safer option.
- **Recommendation:** define a `decode.Source` interface (mirroring Spirit's `change.Source`
  seam) with **logical decoding as primary and trigger-based as fallback** — and treat the
  fallback as a first-class robustness path on Aurora, not a vestige. The full comparison
  (overhead, failover survival, and why neither lets us drop the checksum) is in
  [change-capture-tradeoff.md](change-capture-tradeoff.md).

### 2. Scope of v1

Target the highest-value rewrite cases first: general `ALTER COLUMN TYPE`, volatile-default
`ADD COLUMN`, `STORED` generated column, and full-table repack — plus the **classifier** that
routes natively-safe operations to direct DDL. PK-required, no-FK-on-migrated-table for v1.

**Build the declarative front-end first; imperative is the thin add-on** — settled, matching the
[README TL;DR](README.md#tldr-recommendation),
[high-level-design](high-level-design.md#two-front-ends-declarative-and-imperative), and
build-plan Phase 2. Declarative
does the harder work (introspect + diff + ordering) and exercises the full
classify → route → execute pipeline; the imperative `--alter` path is the **same** pipeline with
the diff step skipped, so it falls out almost for free. The imperative statement remains the
primitive that everything ultimately executes — declarative only *produces* statements — which is
why building declarative first costs nothing on the execution side.

### 3. Repo location / language

Go (reuse `pgx` + `pglogrepl` + `go-pgquery`; matches Spirit's language and idioms). Fresh
standalone repo — this repository.

### 4. Expand/contract (pgroll) as a second execution backend

Whether to ship the engine as a **planner + pluggable executors** (see
[Architecture](#architecture-decoupled-planner-router-and-executors)) so prod-critical
breaking changes can route to pgroll's reversible expand/contract pattern while heavy physical
rewrites use copy-and-swap.

- **Recommendation:** yes in principle — define the `Executor` interface from day one — but
  **defer the pgroll backend itself** past v1. The differentiated, currently-missing
  capability is the log-based copy-and-swap executor; pgroll already exists and can be used
  directly today. Adding it as a backend is mostly about a unified planner/UX, not new
  capability.
- **Open sub-questions:** wrap pgroll as a Go library vs subprocess; how to unify the
  one-shot vs start/complete/rollback lifecycles under one `status`; and the default routing
  policy (auto-route vs explicit `--strategy`) given "decisions, not options".

### 5. Declarative diff engine — build on go-pgquery vs wrap pg-schema-diff

Whether `pkg/schemadiff` builds the desired-vs-live diff on `go-pgquery` + our own schema
model, or wraps [stripe/pg-schema-diff](https://github.com/stripe/pg-schema-diff) (MIT, Go,
PG 14–17, actively maintained) as the diff engine.

- **Wrap (evaluate first):** it already does the hardest parts — introspection,
  server-canonicalized desired state ("parse by executing" on a temp database),
  dependency-ordered plans emitting the same safe idioms our classifier chooses, typed hazard
  annotations (≈ our advisory mode, with an `--allow-hazards`-style CI gate), and plan
  validation against the temp database. Plan generation is cleanly separated from application,
  so our executors keep our own timeout/lock/retry discipline. It is a **periphery** dependency
  (plan generation), so the TCB bar does not apply — pinned like any load-bearing dep.
- **Costs of wrapping:** the temp-database factory is an operational precondition — answered:
  the engine-owned [scratch database](#plan-time-prerequisite-the-scratch-database) is already
  a preflight-verified prerequisite for execute-and-introspect, so wrapping adds no new
  operational demand; renames surface as drop+add and **must** sit behind our
  destructive-diff gate and never-guess-renames refusals; type support beyond enums is missing;
  its embedded timeout policy is replaced by ours at execution.
- **Build:** full control and no temp-database precondition — at the cost of the hardest code
  in Phase 2 and permanent drift risk between a hand-rolled schema model and PostgreSQL's real
  canonicalization.
- **Recommendation:** prototype the wrap behind our own `SchemaDiff` seam in Phase 2; adopt the
  **hazard taxonomy** (advisory-mode vocabulary) and **plan-validation-on-a-throwaway-schema**
  (a verification-ladder rung) regardless of which way this lands. Its declared non-goal —
  *"stateful online migration techniques, like shadow tables, aren't yet supported"* — is
  exactly the gap this engine's copy-and-swap fills, so the tools compose rather than compete.


## Next step

Once the decisions above are settled, scaffold:

- CLI skeleton (`migrate` subcommand),
- `decode.Source` interface + a first logical-decoding implementation,
- PK-range chunker + parallel copier,
- transactional cutover,
- an end-to-end working path for a single `ALTER COLUMN TYPE` against a local Postgres in
  Docker, with an integration test.
