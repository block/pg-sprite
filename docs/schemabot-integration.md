# Future SchemaBot integration

pg-sprite is **orchestrator-neutral**: the engine is a standalone CLI, and a future orchestrator
will drive it through a thin SchemaBot-side adapter — nothing in the core engine changes for it.
No integration code lives in this repository today. The reference
orchestrator is [SchemaBot](https://github.com/block/schemabot), which already drives
[Spirit](https://github.com/block/spirit) for MySQL through a pluggable engine
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

The end goal is for SchemaBot to drive `pg-sprite` for PostgreSQL exactly the way it
drives Spirit for MySQL today — same PR workflow, same operator verbs, same status
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
                                       PostgreSQL
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
| `Apply` | start the native executor asynchronously and return immediately — the synchronous core is exported as `migrate.Run` in [`pkg/migrate`](../pkg/migrate/migrate.go) (parse via `statement.ParseOne`, gate via `migrate.Gate` before dialing, connect via `dbconn.NewPool`, policy via `migrate.DefaultOptions` tuned per table): one statement in, one `verdict.Verdict` out, with a three-shape contract — refusal (verdict, nil error), execution failure (failed verdict carrying the stable code and the committed prefix, plus the operational error), or an error with a zero verdict (stopped before executing). `Run` re-resolves the routing decision at execution time, so the adapter never trusts the stored plan-time verdict (see [execution-mode verdicts](#execution-mode-verdicts-and-direct-execution)). For the declarative flow the adapter does not iterate the plan itself: `migrate.RunDesired` takes the parsed desired schema, re-derives the convergence plan, and runs every planned statement back through `Run` — returning per-statement verdicts with committed-prefix semantics ([execution-model.md](execution-model.md)) — so plan-vs-execute drift and per-statement re-gating stay inside the engine, and the adapter can pin the reviewed plan with `ExpectedFingerprint` |
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
- **The engine role's access is tiered and per-target.** Target databases routinely have
  their DDL owned by a role that is not the orchestrator's connection user; the engine does
  not need to *be* that owner — it needs membership in the owning role, plus schema
  `CREATE` and replication access depending on the strategy (the full contract, including
  what the role must *not* have, is [engine-role.md](engine-role.md)). The adapter surfaces
  this per target: each configured database names its engine-role credentials, and a target
  whose grants stop at Tier 1 can still run in-place changes while copy-and-swap refuses
  with the exact missing `GRANT`.
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

### Routing the create path's refusals

A greenfield desired file — the table does not exist on the target — has no single routing
class, and an adapter must not fold its outcomes into one arm. `migrate.RunDesired` resolves
it to one of four things:

| Outcome | Routing class |
| --- | --- |
| Executed | The table and its indexes exist; a rerun converges to an empty plan |
| `create-collision` refusal | **Re-plan**: re-diff the live catalog — something now owns the name; never blindly retry |
| `insufficient-privileges` refusal (`*preflight.PrivilegeError`, `Tier == TierCreateTable`) | **Operator provisioning action**: the role needs the exact `GRANT` the error carries — not a desired-file fix, and not retryable until granted |
| Admission refusal (`unsupported-statement`) | **Author action**: the desired file states a shape the create path refuses; retrying unchanged cannot succeed |

Only the last is an author error. An adapter that surfaces every greenfield refusal as
"fix your desired file" gives operators the wrong instruction for the two middle rows. A
create that failed mid-sequence follows the
[committed-prefix contract](execution-model.md#the-committed-prefix) — the closing
paragraph of this section says what that means for the gate: it stays closed until the
live catalog is re-diffed; the failed run is never a no-op.

The greenfield `CREATE TABLE` path is a fixed call order, all inside the apply session:

1. `statement.ParseDesired` — parse and validate the desired file (refuses `REFERENCES`,
   `CONCURRENTLY`, qualified names).
2. `preflight.CheckTableAbsent` — mint the `AbsentTarget` proof for the table name.
3. `preflight.CheckCreatePrivileges` — mint the `CreationRole` proof for the target schema.
4. `executor.ExecuteCreate` — consume both proofs and run the set.

`migrate.RunDesired` runs this sequence itself when the plan is greenfield — the adapter
does not assemble it and must not mint either proof separately (a proof minted outside the
executing session proves nothing about it). The order decides which refusal wins when both
preflights would fail: absence is checked first, so an occupied name refuses as
`create-collision` even when the role also lacks `CREATE` — the collision is the more
actionable message (the change is not a create at all) and absence is the cheaper check.

Both proofs share one rule the adapter must respect: they are **minted inside the apply
session and consumed there** — never serialized into `SchemaChange.Metadata`, carried across
the plan/apply boundary, or reused across retries. Absence or privilege at plan time proves
nothing about apply time; the executor re-verifies inside the session that runs the
`CREATE`, the same way ST-7 re-verifies a `PreflightedTable`.

Each refusal from the preflight checks maps to a different orchestrator action — route
them, don't retry them uniformly:

| Refusal | What it means | Orchestrator action |
| --- | --- | --- |
| `ErrRelationExists` / `ErrTypeExists` (grouped by `preflight.IsNameOccupied`) | The name is already taken — this is not a create, it's a change to something that exists | Route to the diff/alter path, not to a failure state |
| `ErrSchemaNotFound` | The qualified schema does not exist on the target | Operator action (create the schema or fix the desired file); retrying cannot succeed |
| `ErrNoCreationSchema` | Unqualified name and the role's `search_path` yields no creation schema | Caller configuration: schema-qualify the name or fix the role's `search_path` |
| `*preflight.PrivilegeError` (`Tier == TierCreateTable`) | The role lacks `CREATE` on the schema (or `USAGE` reaching it); the error carries the exact missing grant | Operator action: provision the named `GRANT`, then retry |

`ExecuteCreate`'s own refusals and failures carry the same routing discipline
([outcome codes](execution-model.md#outcome-codes)):

| Outcome | What it means | Orchestrator action |
| --- | --- | --- |
| `ErrDuplicateCreateName` (`duplicate-create-name`) | The desired set claims one relation name twice — including a first-choice implicit constraint-index name; refused at admission, nothing ran | Fix the desired file; retrying unchanged cannot succeed |
| `ErrPartitionOfUnsupported` (`partition-of-unsupported`) | `PARTITION OF` binds to a live parent the absence proof does not cover | Fix the desired file; out of the create path's scope |
| `ErrIfNotExistsUnsupported` (`if-not-exists-unsupported`) | `CREATE ... IF NOT EXISTS` succeeds as a name-only no-op over a relation it cannot vouch for — the opposite of the absence proof's fail-closed contract; refused at admission, nothing ran | Fix the desired file: state the plain `CREATE`; the absence check owns collision handling |
| `ErrUnsupportedCreateStep` (`unsupported-create-step`) | A desired statement is not a shape the create path can run | Fix the desired file |
| `ErrCreateCollision` (`create-collision`) | A concurrent writer took a needed name after a valid proof | Re-diff the live catalog and re-plan — the world changed; never blindly retry the create |

A failed create is not rolled back wholesale: each step committed in its own bounded
transaction, so the steps before the failure remain
([the committed prefix](execution-model.md#the-committed-prefix)). A rerun's absence check
then refuses with `ErrRelationExists`, and the gate stays closed until the declarative
front door re-diffs the live catalog and converges the remainder — the orchestrator never
assumes the failed run left nothing behind.

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
- **Recommended call sequence: dry-run, then apply.** The typed manual path for a
  rewrite-required refusal (`guidance`), the plan `fingerprint`, and `table_exists` all live
  on the [plan report](plan-report.md) — the dry run's output. The run verdict carries the
  refusal reason as a typed field but its `detail` is display-only prose: an orchestrator
  that wants the typed guidance must obtain the plan report first (dry-run for the
  imperative front door, `diff --json` for the declarative one), then apply, and treat the
  verdict's `detail` as presentation until the verdict carries typed guidance too.
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
