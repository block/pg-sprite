# Future SchemaBot integration

pg-sprite is **orchestrator-neutral**: the engine is a standalone CLI, and a future orchestrator
will drive it through a thin SchemaBot-side adapter — nothing in the core engine changes for it.
No integration code lives in this repository today. The reference
orchestrator is [SchemaBot](https://github.com/block/schemabot), which already drives
[Spirit](https://github.com/block/spirit) for Aurora MySQL through a pluggable engine
abstraction; `pg-sprite` will ship as a new engine behind that same interface. This doc is the
**single home** for that integration — everywhere else the doc set says "the orchestrator" and
points here.

## Table of contents

- [Overview](#overview)
- [Verb mapping (conceptual)](#verb-mapping-conceptual)
- [The proposed SchemaBot-side contract](#the-proposed-schemabot-side-contract)
- [Execution-mode verdicts and direct execution](#execution-mode-verdicts-and-direct-execution)
- [Design constraints the integration imposes](#design-constraints-the-integration-imposes)

## Overview

The end goal is for SchemaBot to drive `pg-sprite` for Aurora PostgreSQL exactly the way it
drives Spirit for Aurora MySQL today — same PR workflow, same operator verbs, same status
surfacing:

- SchemaBot's **migration-engine interface** is the plug-in boundary: plan, apply, progress,
  and the control verbs (stop / start / cutover / cancel / revert / volume). Spirit
  (`type: mysql`) and PlanetScale (`type: vitess`) are existing implementations; a future
  adapter will register `pg-sprite` as a third (e.g. `type: postgres`).
- The **orchestration layer** is engine-neutral. It hands each change to the engine, polls
  progress, and turns control requests (`stop` / `start` / `cutover`, volume 1–11) into
  durable, owner-processed operations. Engines are selected by database `type`.

```diagram
   PR comment / CLI / API
            │
   ╭────────▼─────────╮     ╭─────────────────────────────────╮
   │  orchestration   │     │ migration-engine impls          │
   │  layer           │────▶│  • spirit      (type: mysql)    │
   │  plans, applies, │     │  • planetscale (type: vitess)   │
   │  control reqs    │     │  • pg-sprite   (type: postgres) │ ◀── this engine
   ╰──────────────────╯     ╰────────────────┬────────────────╯
                                             ▼
                                   Aurora PostgreSQL
```

## Verb mapping (conceptual)

The decoupled [planner → router → executor](high-level-design.md) design exists partly to make
this mapping clean — the orchestrator's engine verbs line up almost one-to-one with the layers:

| Engine verb | `pg-sprite` responsibility |
| --- | --- |
| plan | **Parse → declarative diff (when applicable) → classify → route**; declarative desired DDL executes only in the rolled-back scratch environment for introspection, with no writes to the target schema; return an engine-neutral table change |
| apply | **Router + Executor**: run native changes **asynchronously**; return a refusal verdict for non-native-safe changes until later-phase executors land |
| progress | per-table rows-copied / total / percent / ETA / checksum state |
| stop / start | checkpoint and resume (slot + copy + applier state) |
| cutover (+ deferred cutover) | the deferred, operator-gated atomic swap |
| volume | chunk-time / parallelism / throttle level (1–11) |
| cancel | abort and **guarantee logical-slot + shadow-table cleanup** |
| revert | only if the chosen executor supports it (Spirit declines this; see the [reversibility principle](design-principles.md#correctness-and-safety)) |

## The proposed SchemaBot-side contract

pg-sprite does not implement or import this interface today. **The proposed interface for the
SchemaBot-side adapter to implement** is `engine.Engine` in `github.com/block/schemabot/pkg/engine`:
`Name`, `Plan`, `Apply` (async), `Progress`, and the controls `Stop` / `Start` / `Cutover` /
`Cancel` / `Revert` / `SkipRevert` / `Volume` (1=slowest … 11=fastest). A PostgreSQL stub
already exists at `pkg/engine/postgres` (`postgres.New()`, methods currently return *"postgres
engine not implemented"*) with a compile-time `var _ engine.Engine` assertion — that stub is
where the adapter is filled in. (Re-verify these specifics against SchemaBot's current API when
the integration phase starts; they drift.)

**Verb → engine mapping (concrete):**

| `engine.Engine` method | `pg-sprite` implementation |
| --- | --- |
| `Name()` | a stable identifier, e.g. `"pg-sprite"` |
| `Plan` | run parse → declarative diff when applicable → classify → route — exported as `diffplan.Plan` in [`pkg/diffplan`](../pkg/diffplan/diffplan.go) (parse via `statement.ParseDesired`, connect via `dbconn.NewPool`, inputs named by `diffplan.Request{Schema, Desired}`); return a `PlanResult` whose `SchemaChange.TableChanges` are `engine.TableChange{Table, Operation (statement.StatementType), DDL, IsUnsafe, UnsafeReason, ExecutionMode, ModeReason}`; map a **not native-safe** refusal to `engine.ExecutionModeBlocked` with the refusal reason as `ModeReason` (see [execution-mode verdicts](#execution-mode-verdicts-and-direct-execution)) |
| `Apply` | start the native executor asynchronously and return immediately; re-resolve the routing decision at execution time rather than trusting the stored plan-time verdict (see [execution-mode verdicts](#execution-mode-verdicts-and-direct-execution)) |
| `Progress` | per-table rows-copied / total / percent / ETA / checksum state |
| `Stop` / `Start` | checkpoint and resume (slot + copy + applier watermark) |
| `Cutover` | the deferred, operator-gated atomic swap |
| `Cancel` | abort and **guarantee logical-slot + shadow-table cleanup** |
| `Volume` | map 1–11 onto chunk-time target / parallelism / throttle |
| `Revert` / `SkipRevert` | decline for the copy-and-swap path (like Spirit); only the expand/contract backend could honour them |

A refusal is a first-class planning verdict, not a delegation fallback: it includes the reason,
notes that copy-and-swap support arrives in later phases, and names a safer native alternative
where one exists. The adapter maps that verdict to `engine.ExecutionModeBlocked` at plan time; it
never invokes pg-osc or another external copy tool.

**Adapter design notes for the `Plan` row** (recorded here so they are not rediscovered at
implementation time):

- **Disposition mapping is safety-critical.** `DispositionRefuse` maps to execution mode
  `blocked`. `DispositionUnavailable` — a backend the plan needs has not landed — is *not*
  "this change is fine" and must also surface as `blocked`, never as a green plan a merge
  gate can pass. Pin this with a test at the adapter boundary, not just a mapping table.
- **Plan needs a write-capable connection.** SchemaBot's plan path is generally understood as
  read-only against the target, but `diffplan.Plan` executes the desired DDL in an
  always-rolled-back scratch schema on the target database, so the engine role needs `CREATE`
  there and cannot use a hot standby (the live table is never written). This is a
  credential-posture question for every deployment — raise it while the adapter is a design,
  not during a least-privilege review.
- **Fan-out is per table.** A `DesiredSchema` is one `CREATE TABLE` plus its indexes, while
  SchemaBot's declarative roots are directories of many tables: the adapter loops `Plan` per
  table and merges into one `PlanResult`. One pool serves the whole fan-out (`Plan` does not
  close it), but each table costs one scratch transaction per plan, and cross-table ordering
  is the adapter's responsibility.
- **Store the whole `plan.Report`, not just its statements.** Engine-specific fields
  (`ServerVersion`, `Fingerprint`, `TableExists`, `Disposition`) ride in
  `SchemaChange.Metadata`. `plan.Fingerprint` is deterministic across front doors, so it is
  exactly the re-plan comparison SchemaBot needs when a PR head moves and it must decide
  whether the new plan differs materially from the one an operator approved.

**Registration / selection.** The orchestrator core is `pkg/tern`; `tern.NewLocalClient` has
built-in branches for `storage.DatabaseTypeMySQL` (Spirit) and `storage.DatabaseTypeVitess`
(PlanetScale), and looks up everything else in `LocalConfig.EngineFactories[type]` (an
`EngineFactory func(LocalConfig, *slog.Logger) (engine.Engine, error)`). Server embedders
register via `serve.WithEngine(databaseType, factory)` so the core doesn't import the engine
package. Landing this is one of:

- register a `postgres` `EngineFactory` (no core changes), or
- add a built-in branch + a `storage.DatabaseType…`/`storage.Engine…` constant + a
  `tern.proto` `enum Engine` value and its `engineNameToProto` mapping.

**Optional capability interfaces** (type-asserted by `LocalClient`):

- `engine.ExternallyAuthoritativeProgress` — return `true` only if progress is read from
  durable state any instance can query (PlanetScale returns `true`).
- `engine.DeferredCutoverSignalChecker` — `DeferredCutoverSignalExists` for durable
  deferred-cutover recovery (Spirit implements it).
- `engine.Drainer` — `Drain()` to flush in-flight background work on sequential resume.

## Execution-mode verdicts and direct execution

SchemaBot records a per-statement **execution-mode verdict** on each planned `TableChange`:
empty means the engine's default path, `blocked` means the engine deterministically refuses
(the apply will fail), and `direct` means a standing `direct_execution` policy routes the
refused statement to native DDL run directly on the target. The verdict vocabulary lives in
[`pkg/engine`](https://github.com/block/schemabot/blob/main/pkg/engine/engine.go), so the
adapter speaks it without touching anything MySQL-specific, and the engine-adoption contract
is codified upstream in SchemaBot's
[direct-execution doc](https://github.com/block/schemabot/blob/main/docs/direct-execution.md#engine-compatibility)
— written with PostgreSQL explicitly in mind (it names the `pg_class.reltuples = -1` sentinel
and PostgreSQL `lock_timeout`). Contract points for the PostgreSQL adapter:

- **Refusal source.** On the Spirit side the refused-statement signal is Spirit's own refusal;
  here it is pg-sprite's classify verdict. `Plan` maps a pg-sprite *refuse* →
  `engine.ExecutionModeBlocked` with the refusal reason as `ModeReason`. Only after that
  mapping could a direct-execution policy upgrade the verdict to `direct` — and the v1
  decision is that PostgreSQL targets do **not** get `direct_execution` at all: on MySQL a
  small-table direct ALTER blocks only writes, but the PostgreSQL native route holds
  `ACCESS EXCLUSIVE` and blocks reads too, so the policy's risk framing does not transfer.
- **Plan-time verdicts are advisory; the executor re-resolves.** The stored verdict exists so
  operators learn about engine limitations at plan time, but it is estimate-based and stale by
  apply time. SchemaBot's own engines re-evaluate the refusal/policy predicate per statement
  at apply time, and the adoption contract requires the same of any adopting engine:
  pg-sprite's `Apply` must re-resolve the routing decision at execution time and act on that
  fresh decision — never treat the stored plan-time verdict as execution authority. This is
  [OC-4](invariants.md#oc-4--toctou-discipline-on-all-async-state) applied to the verdict.
- **Partition ordering.** SchemaBot partitions the ALTER phase and fixes the execution order:
  all direct-routed statements run first, then all engine-driven ones, regardless of their
  relative order in the plan — and a statement in one partition that depends on one in the
  other fails the apply (fail closed, never reorder to satisfy a dependency). pg-sprite's
  router must mirror that ordering or explicitly document its own.
- **Transactional DDL is a PostgreSQL advantage here.** The MySQL executor keeps no durable
  record of completed direct statements, so a resume/retry of a failed apply can re-execute
  one — a contract question every adopting engine inherits. On PostgreSQL the answer is
  cleaner: run the direct statement inside a transaction, and a failed apply leaves nothing
  behind — the only residual ambiguity is the commit boundary on a dropped connection, which
  [LK-4](invariants.md#lk-4--an-ambiguous-cutover-outcome-is-resolved-by-inspection-never-assumed)
  already resolves by catalog inspection.
- **Consent copy is engine-keyed, and PostgreSQL's must not inherit MySQL's revertibility
  framing.** SchemaBot pins a disclosure to its consent comment whose wording is selected per
  database type; unregistered engines get a conservative engine-neutral fallback: "each table
  is unavailable while its statement runs" + "not revertible". For PostgreSQL the availability
  half is exactly right (`ACCESS EXCLUSIVE` blocks reads and writes), but the blanket "not
  revertible" is conservatively wrong for the **failure** case: PostgreSQL DDL is
  transactional, so a failed or cancelled direct statement rolls back cleanly. When the
  PostgreSQL consent copy is registered, the outcome sentence should split — *if it fails it
  rolls back cleanly; if it succeeds it is not revertible* — so the transactional-DDL
  advantage is visible in the disclosure rather than flattened into MySQL's indeterminacy.
  MySQL-only semantics ("revertible via rollback", "reads still work") must never leak into
  the shared fallback.

## Design constraints the integration imposes

These shape the engine's state and API surface from day one, long before the adapter exists —
they are registered as the `OC-*` invariants in
[invariants § orchestration / control-plane](invariants.md#orchestration--control-plane-oc):

- **Keep PostgreSQL-only machinery inside the adapter.** When the copy-and-swap backend and
  adapter exist, logical-decoding slot create/cleanup, `REPLICA IDENTITY`, logical-replication
  preflight, and the trigger fallback will all live behind `Apply`/`Stop`/`Cancel` so the
  engine-neutral orchestration layer stays untouched
  (OC-6).
- **Shared types stay engine-agnostic** — engine-specific data rides in generic
  `Metadata map[string]string` fields (OC-6).
- **ID namespaces never conflate** — the engine's migration identifier is an opaque
  `external_id` to the orchestrator; the orchestrator's user-facing identifier is never routed
  to the engine (OC-5).
- **The orchestrator is an untrusted request source** like the CLI: control requests are
  re-validated against current engine state, fail-closed (OC-1..OC-4; see
  [tcb-model.md](tcb-model.md)).
