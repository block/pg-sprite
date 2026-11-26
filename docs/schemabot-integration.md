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
- [Outcomes of a checksum pass under the stream](#outcomes-of-a-checksum-pass-under-the-stream)
- [Invalid-index outcomes of a concurrent index build](#invalid-index-outcomes-of-a-concurrent-index-build)
- [Execution-mode verdicts and direct execution](#execution-mode-verdicts-and-direct-execution)
- [Stopping, resuming, and starting over](#stopping-resuming-and-starting-over)
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
| apply | **Router + Executor**: run native changes **asynchronously**, and copy-and-swap changes when the run opens that route; return a refusal verdict for a change no route carries |
| progress | per-table rows-copied / total / percent / ETA / checksum state |
| stop / start | checkpoint and resume (slot + copy + applier state) |
| cutover | the gated atomic swap, taken as soon as the gates pass; an operator-gated deferred cutover is not in v1 ([D10](copy-and-swap-design.md#d10--cut-over-as-soon-as-the-gate-passes)) |
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
| `Apply` | start the native executor asynchronously and return immediately — the synchronous core is exported as `migrate.Run` in [`pkg/migrate`](../pkg/migrate/migrate.go) (parse via `statement.ParseOne`, gate via `migrate.Gate` before dialing unless `migrate.GateReadsCatalog` names the statement — a single-index `DROP INDEX`, whose refusal `Run` or `migrate.GateWithCatalog` completes from the index's kind —; connect via `dbconn.NewPool`, policy via `migrate.DefaultOptions` tuned per table): one statement in, one `verdict.Verdict` out, with a three-shape contract — refusal (verdict, nil error), execution failure (failed verdict carrying the stable code and the committed prefix, plus the operational error), or an error with a zero verdict (stopped before executing). `Run` re-resolves the routing decision at execution time, so the adapter never trusts the stored plan-time verdict (see [execution-mode verdicts](#execution-mode-verdicts-and-direct-execution)). For the declarative flow the adapter does not iterate the plan itself: `migrate.RunDesired` takes the parsed desired schema, re-derives the convergence plan, and runs every planned statement back through `Run` — returning per-statement verdicts with committed-prefix semantics ([execution-model.md](execution-model.md)) — so plan-vs-execute drift and per-statement re-gating stay inside the engine, and the adapter can pin the reviewed plan with `ExpectedFingerprint` |
| `Progress` | per-table rows-copied / total / percent / ETA / checksum state |
| `Stop` / `Start` | checkpoint and resume (slot + copy + applier watermark). `Start` reads the row back through `checkpoint.Store.Inspect`, hands a non-terminal row to `schemachange.InspectResume`, and on `ResumeReconcile` runs `schemachange.Reconciler.Reconcile` before any stream reopens; the slot a reconcile returns in `Reconciled.Slot` is open and the adapter closes it. A reconcile that fails leaves the row in `reconciling`, which `InspectResume` answers as reconcile again, so repeating it is safe. How to classify each refusal is in [the resume path's refusals](#routing-the-resume-paths-refusals) |
| `Cutover` | the gated atomic swap, taken as soon as the gates pass; v1 has no operator-gated deferred cutover ([D10](copy-and-swap-design.md#d10--cut-over-as-soon-as-the-gate-passes)) |
| `Cancel` | abort and **guarantee logical-slot + shadow-table cleanup**; for a running concurrent index build the stop path is `progress.Tracker.CancelBuild`, whose outcomes the adapter maps with the [cancel outcome table](progress-report.md#cancelling-a-concurrent-index-build) — test for `ErrCancelNotDispatched` before any context error, and record the operator's cancel as possibly delivered only when that sentinel is absent |
| `Volume` | map 1–11 onto chunk-time target / parallelism / throttle |
| `Revert` / `SkipRevert` | decline for the copy-and-swap path (like Spirit); only the expand/contract backend could honour them |

Connect through `dbconn.NewPool`, as `Plan` and `Apply` do: a pool from `dbconn.NewPool` drops a shadowing `pg_catalog` search_path entry on every session, while any other pool relies on each read being `pg_catalog`-qualified ([CO-9](invariants.md#co-9--decision-reads-resolve-to-the-real-catalog)). Either way, a read that runs under a path pg-sprite builds that lists the application schema is only as safe as its own qualification. The **Which pool** note under [what a live `Run` holds open](#what-a-live-run-holds-open) lists what holds on a pool built another way.

A refusal is a first-class planning verdict, not a delegation fallback: it includes the reason
and names a safer native alternative where one exists. The adapter maps that verdict to
`engine.ExecutionModeBlocked` at plan time; it never invokes pg-osc or another external copy
tool.

**Adapter design notes for the `Plan` row** (recorded here so they are not rediscovered at
implementation time):

- **Disposition mapping is safety-critical.** `DispositionRefuse` maps to execution mode
  `blocked`. `DispositionUnavailable` — the change needs a path its backend does not carry in
  this build — is *not* "this change is fine" and must also surface as `blocked`, never as a
  green plan a merge gate can pass. Pin this with a test at the adapter boundary, not just a mapping table.
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
  with the exact missing `GRANT`. `schemachange.CheckCopySwap` is the one call the adapter
  makes for the copy-and-swap route, as `pkg/migrate` does: it folds the privilege, shape,
  and environment checks and returns the `CopySwapTarget` every writing step requires. For a
  live run, pass in `CopySwapEnvironment.SlotLagCeilingBytes` the ceiling the catch-up will
  enforce: the disk check counts it beside the shadow, zero counts
  `preflight.DefaultSlotLagCeiling` (the catch-up's default), and a negative value is
  refused. Between the shape and the environment check it fills `CopySwapEnvironment.Held`
  from `schemachange.MeasureResumeHeld` (the shadow's and the slot's sizes, read from the
  server, zero for a fresh run), so a resume is asked only for what is left to grow. A
  `cutover` row whose swap the catalog shows committed — the read `InspectSwapped` makes —
  is reported as `Held.SwapCommitted`, and the disk check then asks for neither the copy's
  headroom nor the slot lag ceiling: the run that finishes it builds nothing and decodes no
  WAL, it only drops the slot, drops the retained source (or under `KeepOld` takes its
  write fence off), and saves `done`, so a volume tight because the retained source is
  still on it admits the cleanup. The target it returns reports `FinishesCommittedSwap`,
  and a `Runner` handed it refuses with `finish-admission-stale`, writing nothing, when the
  row it finds under its lock asks for more than that cleanup. A `cutover` row whose swap
  did not commit, or whose outcome that read cannot call yet, is measured as any other
  resume: a swap that rolled back verifies and swaps again over the shadow, and a retained
  source renamed away or under another owner is refused by the run
  (`cutover-relation-replaced`) once the disk check passes. The check takes no statement,
  so at plan time, on a table with a stopped row, it describes that row's run: a different
  statement is refused by the run as `resume-row-mismatch`, and its own copy was not
  measured.
  `preflight.CheckCopySwap` runs the same three checks without that measure, so it asks a
  resume for the whole requirement again. Pass `schemachange.CopySwapRestart` for a run
  with `RunOptions.Restart` set and `schemachange.CopySwapResume` for every other run; the
  zero value is refused. A restart is checked as a fresh run with `Held` zero, because its
  discard has not freed what the stopped run holds when the preflight runs, and counting it
  would ask for less than the run will have. The call writes nothing, so it is safe before
  the first write; it may wait under its context on a lock on the shadow while it measures
  it. Measure the free disk right before the call: a target checked at plan time is a
  forecast, and the apply checks again. The slot headroom check still asks a resume for one
  free replication slot, though a resume reuses the slot it holds; with
  `max_replication_slots` fully used, the stopped run's slot included, a resume is refused as
  `copy-and-swap-slot-headroom`. Do not rebuild that chain by hand. How to classify each
  refusal of these checks is in
  [refusal-classes.md](refusal-classes.md#copy-and-swap-refused-keyed-on-copyswaprefusalcause).
  A shape refusal is a `*preflight.UnsupportedCopySwapShapeError`; its `NoPrimaryKey` is
  true when the cause is `copy-and-swap-pk-unsupported` because the table has no primary
  key at all, the one case where adding a key online is the next step.
- **Fan-out is per table.** A `DesiredSchema` is one `CREATE TABLE` plus its indexes, while
  SchemaBot's declarative roots are directories of many tables: the adapter loops `Plan` per
  table and merges into one `PlanResult`. One pool serves the whole fan-out (`Plan` does not
  close it), but each table costs one scratch transaction per plan, and cross-table ordering
  is the adapter's responsibility.
- **So is the set of tables.** A live table with no desired file is invisible to a per-table
  diff, and under a declarative model its only convergence is `DROP TABLE`. pg-sprite never
  plans one, so there is no `verdict.Verdict` to map. The adapter enumerates the namespace's
  live tables itself with `schemadiff.ListManagedTables`, the catalog query `pull` uses. Its
  exclusions are explained under
  [Deliberately operator-owned](capabilities.md#deliberately-operator-owned).

  For each undeclared table the adapter synthesizes an `engine.TableChange` that drops it:
  `IsUnsafe`, with a data-loss `UnsafeReason`, behind the same unsafe acknowledgment as
  SchemaBot's MySQL path. It records the table's definition at plan time with
  `schemadiff.DroppedTableDefinition` and runs the acknowledged drop with
  `executor.ExecuteDropTable`, which refuses if the table changed since the review.
  Store the rendered string, not the model: the model's JSON leaves out
  `IncomingForeignKeys` and `TableDependents`, so it does not render back to the same
  definition. Never render it again just before the drop, which would compare the table
  with itself. `verdict.ReasonDestructiveChange` is not reused for the row: that reason is scoped to a
  plan pg-sprite produced.

  Onboarding follows from the rule: an existing database is entirely undeclared tables, so
  the adapter declares them first (`pull` writes one desired file per table) or scopes
  which namespaces it owns.
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
class, and an adapter must not fold its outcomes into one arm. The plan resolves
shape-decidable author errors before the apply window, and `migrate.RunDesired` re-checks
them when admitting execution. A greenfield run resolves to one of these outcomes:

| Outcome | Routing class |
| --- | --- |
| Executed | The table and its indexes exist; a rerun converges to an empty plan |
| `create-collision` refusal | **Re-plan, then fix the occupant**: the table name or a claimed index, constraint-index, or sequence name is occupied. Re-diff the live catalog to see what holds it; re-planning alone reproduces the refusal — drop or rename the occupant, name a constraint's index explicitly, or for a sequence use an explicitly named sequence or a non-serial column. Never blindly retry |
| `insufficient-privileges` refusal (`*preflight.PrivilegeError`, `Tier == TierCreateTable`) | **Operator provisioning action**: the role needs the exact `GRANT` the error carries — not a desired-file fix, and not retryable until granted |
| Plan/admission refusal (`unsupported-statement`) | **Author action**: the desired file states a shape the create path refuses; retrying unchanged cannot succeed. The [plan report](plan-report.md) (`format_version` 3 and later) names the shape as the refused statement's typed `cause` — read that field rather than matching the executor's sentinels or the verdict's prose |
| `declarative-foreign-key` refusal (`cause` `foreign-key`) | **Unsupported**: the new table declares a foreign key, and desired-state execution does not run foreign-key changes. The plan still shows the `CREATE TABLE`; create the table without the foreign key, then add it with `migrate --alter` (`ADD FOREIGN KEY` runs as `NOT VALID` + `VALIDATE`) |
| `create-name-mismatch` failure (exit 1, `failed_step` 1) | **Operator action on a table that now exists**: the `CREATE TABLE` committed but a claimed first-choice constraint-index or sequence name went to an occupant inside the probe's window, so the server suffixed it. The executor leaves the table; an operator frees the claimed name and renames the owned relation to it, or drops the table, then re-diffs. A rerun does not re-enter the create path: it diffs the live table, and converging the suffixed relation onto its claimed name is a constraint drop the planner refuses as `destructive-change` (a suffixed sequence is a sequence change the planner does not converge), so nothing runs until the operator acts |
| `create-names-unverified` failure (exit 1, `failed_step` 1) | **Operator action on a table that now exists**: the `CREATE TABLE` committed but the read of the names the table owns did not complete, so the claims are unproven — not failed. An operator compares the table's constraint-index and sequence names against the desired file, renames or drops, then re-diffs; a rerun diffs the live table exactly as above |
| `insufficient-privileges` refusal naming a create owner that does not exist | **Operator configuration action**: `Options.CreateOwner` names a role the server does not have. Nothing ran; create the role or correct the configured owner. Not retryable until one of those changes |
| `create-owner-mismatch` failure (exit 1, `failed_step` 1) | **Operator action on a table that now exists**: the `CREATE TABLE` committed under `SET LOCAL ROLE`, but its catalog owner is not the configured `CreateOwner`. The executor never repairs ownership; an operator inspects, transfers with `ALTER TABLE ... OWNER TO`, or drops, then re-diffs. Permanent: a blind rerun finds the name occupied and reports a `create-collision` instead of the ownership fault |
| `commit-outcome-unknown` failure (exit 1, `failed_step` 1) | **Operator inspection before any retry**: the `CREATE TABLE` step's `COMMIT` did not return success, so the table may or may not exist. Inspect the catalog for it; if it exists, re-diff (a rerun diffs the live table, as above), and if not, the create can run again |
| `create-owner-unverified` failure (exit 1, `failed_step` 1) | **Operator action on a table that now exists**: the `CREATE TABLE` committed but the read of its owner did not complete, so the owner is unproven — not wrong. An operator compares the table's owner against the configured one (`\dt` shows it), then re-diffs |

Greenfield refusals have deterministic precedence: a decidable shape refusal comes before the
table-absence and privilege checks because it needs no connection.

Only the plan/admission refusal is an author error. An adapter that surfaces every
greenfield refusal as "fix your desired file" gives operators the wrong instruction for the
collision and privilege rows, and the two failure rows are not refusals at all — a table now
exists. A create that failed mid-sequence follows the
[committed-prefix contract](execution-model.md#the-committed-prefix) — the closing
paragraph of this section says what that means for the gate: it stays closed until the
live catalog is re-diffed; the failed run is never a no-op.

The greenfield `CREATE TABLE` path is a fixed call order, all inside the apply session:

1. `statement.ParseDesired` — parse and validate the desired file (refuses
   `CONCURRENTLY` and qualified names, including a qualified `REFERENCES` target).
2. `preflight.CheckTableAbsent` — mint the `AbsentTarget` proof for the table name.
3. `preflight.CheckCreatePrivileges` — mint the `CreationRole` proof for the target schema.
4. `executor.ExecuteCreate` — consume both proofs, admit the set, probe `pg_class` for every
   index, constraint-index, and sequence name the set claims, then run the set.

For the shapes the create path admits, the plan's `exec_sql` is the statement as written —
the plain `CREATE TABLE` and index builds this path runs, with each build's decision
reclassified `metadata-only`; it never substitutes `CONCURRENTLY` for an index on the table
born in that run.

`migrate.RunDesired` runs this sequence itself when the plan is greenfield — the adapter
does not assemble it and must not mint either proof separately (a proof minted outside the
executing session proves nothing about it). The order decides which refusal wins when more
than one check would fail. The table name is checked first, so an occupied **table** name
refuses as `create-collision` even when the role also lacks `CREATE` — the collision is the
more actionable message (the change is not a create at all) and absence is the cheaper
check. Claimed **index** names are probed last, inside `ExecuteCreate`: an occupied index
name with a role lacking `CREATE` surfaces as `insufficient-privileges`, and the index
collision appears only once the grant is in place.

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
([outcome codes](execution-model.md#outcome-codes)). The four shape refusals are also decided
in the plan, where the refused statement's `cause` names the shape
([create-shape causes](execution-model.md#create-shape-causes)); the apply-time sentinel is
the re-check, not the first notice:

| Outcome | What it means | Orchestrator action |
| --- | --- | --- |
| `ErrDuplicateCreateName` (`duplicate-create-name`) | The desired set claims one relation name twice — including a first-choice implicit constraint-index name; refused in the plan and re-checked at apply, nothing ran | Fix the desired file; retrying unchanged cannot succeed |
| `ErrPartitionOfUnsupported` (`partition-of-unsupported`) | `PARTITION OF` binds to a live parent the absence proof does not cover; refused in the plan and re-checked at apply | Fix the desired file; out of the create path's scope |
| `ErrIfNotExistsUnsupported` (`if-not-exists-unsupported`) | `CREATE ... IF NOT EXISTS` succeeds as a name-only no-op over a relation it cannot vouch for — the opposite of the absence proof's fail-closed contract; refused in the plan and re-checked at apply, nothing ran | Fix the desired file: state the plain `CREATE`; the absence check owns collision handling |
| `ErrUnsupportedCreateStep` (`unsupported-create-step`) | A desired statement is not a shape the create path can run; refused in the plan and re-checked at apply | Fix the desired file |
| `ErrCreateCollision` (`create-collision`) | A claimed index, constraint-index, or sequence name was already occupied, or a concurrent writer took a needed name after the absence checks | Re-diff the live catalog to see what holds the name, then drop or rename the occupant, name a constraint's index explicitly, or for a sequence use an explicitly named sequence or a non-serial column — re-planning alone reproduces the refusal; never blindly retry the create |
| `ErrCreateNameMismatch` (`create-name-mismatch`) | The `CREATE TABLE` committed, but a first-choice constraint-index or sequence name the desired file claimed went to an occupant inside the probe's window and the server chose a suffixed name; `*CreateNameMismatchError` lists the `Missing` claims and the `Unclaimed` names the table owns instead | **Refuse-and-leave — the table exists**: surface the mismatch to the author as a failed apply. A rerun re-diffs the live table rather than re-entering the create path, and converging the suffixed relation is a destructive drop the planner refuses — so nothing runs until an operator frees the first-choice name and renames the owned relation to it, or drops the born table; then re-diff |
| `ErrCreateNamesUnverified` (`create-names-unverified`) | The `CREATE TABLE` committed but the read of the names the table owns did not complete, so the claims are unproven; the read's own failure — a cancelled context, a lost connection, a table no longer at its name — is the cause in the chain, never the code | **Refuse-and-leave — the table exists**: an operator compares the table's constraint-index and sequence names against the desired file by hand (`\d` shows them), renames or drops, then re-diffs; a rerun re-diffs the live table exactly as for a mismatch |
| `ErrCreateOwnerMismatch` (`create-owner-mismatch`) | The `CREATE TABLE` committed under `SET LOCAL ROLE` but its catalog owner is not the `Options.CreateOwner` the proof named; `*CreateOwnerMismatchError` carries `Expected` and `Actual` | **Refuse-and-leave — the table exists**: the executor never repairs ownership. Surface both owners; an operator transfers or drops, then re-diffs. Permanent — do not retry |
| `ErrCreateOwnerUnverified` (`create-owner-unverified`) | The `CREATE TABLE` committed but the read of its owner did not complete; the read's own failure is the cause in the chain, never the code | **Refuse-and-leave — the table exists**: an operator compares the table's owner against the configured one, then re-diffs |
| `preflight.ErrCreateOwnerNotFound` (surfaced as an `insufficient-privileges` refusal) | `Options.CreateOwner` names a role the server does not have; decided before anything runs | **Operator configuration action**: create the role or correct the configured owner; not retryable until one changes |

Before the first step, `ExecuteCreate` probes `pg_class` in one schema-scoped catalog
snapshot for every relation name the desired file states — explicit `CREATE INDEX` names
and the first-choice names of implicit constraint indexes and column-owned sequences. Names
the server invents rather than names the desired file states, such as an unnamed `CREATE
INDEX ON t (v)`, are outside this coverage because the probe has nothing to check.
`CheckTableAbsent` separately covers the table relation and composite type. Duplicate-name
SQLSTATEs backstop races for explicit names. For server-chosen names, the probe narrows the
race to the time-of-check window; the owned-name verification below catches a name taken
inside it.

`preflight.LookupOwnedRelationNames` reads the constraint-index and column-owned-sequence
names a table actually owns — whether the server invented them or the desired file named
them — so a caller can compare them against the names a create claimed; foreign keys do
not contribute the indexes they borrow from the referenced table. `ExecuteCreate` runs
that comparison itself inside step 1, after its `CREATE TABLE` commits: a claimed
first-choice name the table does not own is a `*CreateNameMismatchError` (sentinel
`ErrCreateNameMismatch`, code `create-name-mismatch`) wrapped in a `SequenceStepError`
at step 1, and the executor leaves the born table in place rather than dropping a relation
it cannot prove nobody has started to use. A read of the owned names that does not complete
is the same step-1 failure wrapped in `ErrCreateNamesUnverified` under its own code,
`create-names-unverified`, with the read's own failure as the cause in the chain — never a
pass, and never a mismatch it could not observe.

A `create-collision` can identify a name the table needs — an index, constraint index, or
sequence — rather than the table name itself.

A failed create is not rolled back wholesale: each step committed in its own bounded
transaction, so the steps before the failure remain
([the committed prefix](execution-model.md#the-committed-prefix)). A rerun's absence check
then refuses with `ErrRelationExists`, and the gate stays closed until the declarative
front door re-diffs the live catalog and converges the remainder — the orchestrator never
assumes the failed run left nothing behind.

### Routing the resume path's refusals

`Start` on a copy-and-swap target is two reads and, when the second says so, one write:
`checkpoint.Store.Inspect` for the row, `schemachange.InspectResume` for the mode, and
`schemachange.Reconciler.Reconcile` when the mode is `ResumeReconcile`. The row's phase is
dispatched first — `Phase.Terminal()` has nothing to resume, `cutover` is `InspectSwapped`'s
question — and only then is the slot read. Both `InspectResume` and `Reconcile` refuse with a
`*RefusalError` keyed on `RefusalCause`; each cause means a different action, so route them.
Route on `RefusalCauseOf(err)`, not on the sentinel: every refusal answers
`errors.Is(err, ErrInvariantViolation)`, the `environmental` ones below included, so an adapter
that tests the sentinel first would send an operator-dropped shadow or a held slot down its
"report a bug" path. The class each cause carries is the
[refusal-classes table](refusal-classes.md#shadow-operation-refusals-keyed-on-refusalcause):

| Refusal | What it means | Orchestrator action |
| --- | --- | --- |
| `resume-row-terminal` | The row is `done` or `failed`; there is no run to resume | Report the recorded outcome; a new run needs `Store.Delete` of the row the caller was shown |
| `resume-row-cutover` | The row says a swap began and nothing says how it ended; neither a clean resume nor a reconcile may touch the slot or the shadow until that is known. The refusal is answered before the slot is read, so it says nothing about whether the slot survived | Call `InspectSwapped` first; a committed swap continues from `DropOldTable`. A rolled-back swap (`ErrNotSwapped`) is a run still catching up: write the row back to `catching_up` and call `InspectResume` again — it answers clean when the slot is intact, and reconcile when the slot was lost during the cutover window, in which case `Reconcile` repairs the gap before the cutover is retried |
| `restart-swap-committed` (from `Runner`, with `RunOptions.Restart`, and from `Runner.Discard`) | A run asked to start over, or a discard, met a `cutover` row whose swap the catalog shows committed: the shadow already bears the source's name, so there is nothing to discard and the change is past the point of starting over. Nothing is written | Run the same statement without the restart; `InspectSwapped` answers that the swap committed and the run finishes from `DropOldTable` — drops the slot of a live run, drops or keeps the retained source, writes the row `done` |
| `finish-admission-stale` (from `Runner`) | The target was admitted only to finish a committed swap, with no disk space asked, and the row the run found under its lock asks for more: none, a `done` row, a row a restart discards, or a row whose swap has not committed. Nothing is written | Call `CheckCopySwap` again, which measures the row as it is now, and run on the new target |
| `resume-row-mismatch` | The row is for another table, names another slot, or (in `Reconcile` and `Runner`) carries another statement's shadow name, fingerprints, or shadow proof; from `Runner` also a row whose recorded statement is not the one `Run` was given, compared as parsed, or a row that records no statement | Configuration or operator action: the target and the row disagree; retrying cannot succeed. A caller that means a second, different change on the same table deletes the row (`Store.Delete`) — and drops the shadow and the fence — first |
| `shadow-statement-target` (from `Runner`, on resume) | The statement `Run` was given names another table than the proven target, caught before the row's statement is compared with it | Configuration: the statement and the target disagree; retrying cannot succeed |
| `resume-row-decoding` (from `Runner`) | The row was written by the other kind of run: a target proven without logical decoding was handed a row that names a slot or publication, or is in `catching_up` or `reconciling`, and has no slot to continue it with; a target proven for decoding was handed a row that names no slot, whose source was fenced, not captured | Run the row under the kind of `CopySwapTarget` proof that made it; the proof, not the caller, chooses the route |
| `shadow-unrecorded` (from `Runner`) | A relation bears the target's derived shadow name and no row records a run for the target | `DropShadow` the leftover and run again, or run again with `RunOptions.Restart`, which drops it under the run's lock first; a relation the engine did not make is an operator's to rename |
| `resume-shadow-gone` (from `Runner`) | The row's swap has not committed (any phase from `copying` to a `cutover` whose swap rolled back) and it records a shadow under which no relation stands; `ErrShadowNotFound` is reachable through the refusal, and nothing was written | Run the statement again with a restart (`--restart`, `RunOptions.Restart`), which discards the row, its slot or fence, and starts over. By hand, a row of a run that blocked writes: `Store.Delete` the row and run again; the next run builds a fresh shadow, and the source stays fenced until `DropWriteFence` or that run's swap. Live row (it names a slot): `decode.DropSlot` the row's slot, then `Store.Delete` the row, and run again; the slot retains WAL until it is dropped, and a run started while it stands fails creating its own. A live source has no fence |
| `resume-slot-foreign` | The slot of the derived name is physical or another database's; the engine may neither decode from it nor drop it | Operator action on the slot; retrying cannot succeed while it is there |
| `resume-slot-held` | The engine's own slot is held by another backend — a previous run still draining, or a tool reading it | Wait and inspect again; a holder that never lets go is an operator action |
| `cutover-relation-replaced` (from `Reconcile`) | The source or the shadow is no longer the relation the build recorded, read before the reconcile wrote anything | Stop; the table under the names is not the one this run proved — operator action |

`Runner` also ends with errors that are not refusals but still route differently; each leaves
the row at its last save and the shadow in place, with the source fenced (quiesced target) or
the slot under the row (decoding target):

| `Run` outcome | What it means | Orchestrator action |
| --- | --- | --- |
| `*decode.SlotExistsError` (decoding target, first start) | A logical slot of the engine's derived name already exists and no row names it: a run that ended before its row landed, or a slot made by hand. The run made nothing and dropped nothing — the slot, its publication, and the absence of a row are as the run found them | Run `Store.ReapOrphanSlots` (or `pg-sprite reap` on the same database; `--dry-run` shows what it would drop), which drops the slot once nothing holds it, and then `Run` again; a slot that stays held is an operator's to inspect |
| `*checksum.DivergenceError` | The verification pass found the shadow differing from the source — under a fence nothing should have written either, and under a stream a difference the catch-up did not settle is a confirmed divergence — so the run stops rather than repair (CO-3 `DivergenceAbort`) and the row stays in `verifying` | Do not retry blindly: a retry runs the same pass over the same shadow and stops the same way. Start the run over — `Run` again with `RunOptions.Restart`, which discards the shadow, the fence or the slot, and the row under the table lock before building afresh; or by hand, `DropShadow`, `DropWriteFence` or `decode.DropSlot`, `Store.Delete`, then `Run` — a difference that recurs under a fence is a writer bypassing it (a role that owns the table and disables it) to find. A writer in `session_replication_role = replica` — a logical-replication subscription's apply worker, a bulk restore with triggers disabled — does not bypass the fence, which is enabled `ALWAYS`: it is refused by it, and a subscription that errors and retries for the run's length is that refusal, not a divergence |
| `cutover-unfenced` (quiesced target, from `Cutover`, through `Run`) | The swap, under its lock, found the engine's write fence on the source disabled, dropped, replaced by a trigger of another shape, or not enabled `ALWAYS`; nothing is swapped and the row is in `cutover` | Retry `Run`: it installs the fence again, verifies again, and swaps; a write that landed while the fence was down makes the verification pass stop on a `*checksum.DivergenceError`. Find what changed the source's triggers |
| `*checksum.UnresolvedError` (decoding target) | The live pass could not settle a chunk: the catch-up did not confirm past the read within the wait (`lagging`), or more keys than `Live.CarryKeys` (default `checksum.MaxCarryKeys`) changed between every two rereads (`hot`) — up to that many are left for the swap to compare instead; nothing was found wrong and nothing was minted, and the row stays in `verifying`. The bound is `LiveOptions.Attempts` fresh rereads of one chunk (default `checksum.DefaultSettleAttempts`), each waited on for the catch-up up to `LiveOptions.SettleWait`, and each reread judges the chunk key by key, so a chunk written somewhere between every two of its rereads still settles as long as each key holds still across one pair of reads; a key written again between every two rereads never settles and is left for the swap, so the error needs more such rows than the swap will compare, whatever the table's total write rate. `UnresolvedError.Keys` is how many keys were still changing at the last reread | Retry `Run`: the resumed row catches up again and runs a fresh pass; that many rows written faster than the pass can read them twice is a load to wait out, write less often, or give more `Attempts`. Whether a load raises this depends on the host as well as the table: the load proof's default load, 800 writes a second spread over the table, raised none in five runs at 50,000 and 100,000 rows on a laptop with `fsync=off` ([the record](copy-and-swap-design.md#evidence-key-by-key-settling-under-the-load-proofs-default-load)), which says nothing of a slower disk or a throttled sender, so handle the stop whatever the table's size |
| `cutover-carried-diverged` (decoding target) | A key the live pass left for the swap still differed between the source and the shadow under the swap lock, after the drain; the swap rolled back, the source is live, and the row is in `cutover` | Stop and report: the stream was applied to its end with no writer on either relation, so the difference is a defect in the copy or the apply. A retried `Run` returns the row to `catching_up` and verifies again; start over with `RunOptions.Restart` |
| `cutover-drain-deferred` (decoding target) | The swap's drain met a unique-key move it could not order into the shadow; the swap rolled back and the row is in `cutover` with the position the drain reopened from | Retry `Run`: `InspectSwapped` answers `ErrNotSwapped`, the row goes back to `catching_up`, and the same changes are drained again once the rows the move touched have settled |
| `*decode.SlotLagExceededError` (decoding target) | The slot's WAL crossed `CatchupOptions.SlotLagCeiling` while the capture ran; the run stopped with the slot in place under the row. `Cause` says which measure crossed; switch on the constants, with a default branch, since the set can grow, or test `Cause.RetainsWAL()` to act on both retained-WAL causes at once: `decode.SlotLagStreamBehind` (`stream-behind`) means the stream itself had more WAL unconfirmed than the ceiling — the capture fell behind the source's writes; `decode.SlotLagWALRetained` (`wal-retained`) means the stream was within the ceiling but the server kept more WAL back than it for longer than its release interval allows — either a transaction on the source holds the slot's restart position, or the cluster writes more WAL per release interval and cycle than the ceiling holds (on a busy source the server releases the slot only to the running-transactions record before the one it acts on, so the slot keeps between one and two release intervals of the cluster's WAL, the copy's own writes included, and the ceiling must hold more than a release interval and a cycle of it, with margin); `decode.SlotLagWALRetainedHardLimit` (`wal-retained-hard-limit`) means a reading found the server keeping more than three times the ceiling (`decode.RetainedWALHardMultiple`; `decode.RetainedHardLimit` gives it in bytes for a ceiling, without overflow) before that release interval had passed — a healthy slot peaks at about twice a ceiling sized with margin just before a release, so more than three times it is WAL a transaction holds back or WAL written faster than the server releases it. Three times the ceiling is the threshold a reading is judged against, not the most the slot can hold: the slot also keeps what the cluster writes in the cycle before that reading, and it keeps retaining WAL after the stop until the run resumes or is abandoned. The error carries both measures (`Unconfirmed`, `Retained`) and the ceiling. The run holds its own copy back while the stream has more than a quarter of the ceiling unconfirmed ([D12](copy-and-swap-design.md#d12--throttle-by-chunk-time-and-slot-lag)), so the copy outrunning the apply slows the copy before it ends the run `stream-behind`; the pace does not read retained WAL, so a `wal-retained` stop is unchanged by it. `CopyPaced` is how far this run's copy was held back before the stop, the same `copier.Paced` a completed run returns on `Result.CopyPaced`: a `stream-behind` stop with a positive `CopyPaced.Waits` fell behind with the copy already slowed, and the zero `Paced` means the run made no copy or its copy was never held back. `Deadlocks` (`decode.CatchupDeadlocks`) is the catch-up's run of deadlocked flushes at the stop: `Consecutive` is how many flushes in a row the server had rolled back as a deadlock victim, and `BackingOff` says the rolled-back batch was still waiting out its `CatchupOptions.DeadlockBackoff`, which holds the confirmation below it while the source keeps writing; the zero value means no deadlock was in progress. The field is set on every cause, but only a nonzero one on a cause whose `RetainsWAL` is false adds a clause to the error text: on a retained-WAL cause the stream was within the ceiling, so the deadlocks did not cross it | Route on `Cause` first, then on `Deadlocks.Consecutive > 0`: on `stream-behind` a positive count is also an `applier.ErrBatchDeadlocked` cause, since the rolled-back flushes left the shadow unchanged, so retry once the unique-value churn eases, as for that error; on a retained-WAL cause act on the cause alone, since a retry under the same ceiling stops again. `stream-behind`: retry `Run` once the source's write rate allows; the stream reopens at the row's position and drains the backlog. `wal-retained`: look for the transaction holding WAL back — an open write transaction (`pg_stat_activity` where `backend_xid IS NOT NULL`, oldest `xact_start` first) or a prepared one (`pg_prepared_xacts`; a read-only transaction does not hold it) — end it, then retry `Run`; if none is found, the write rate is the cause and the run needs a higher `SlotLagCeiling`, above the `Retained` the error reports (`migrate.CopySwapOptions.SlotLagCeilingBytes` through `pkg/migrate`, `--slot-lag-ceiling SIZE` on the CLI). `wal-retained-hard-limit`: look for the holding transaction first, as for `wal-retained`, and end it, then retry `Run`; if none is found, the write rate is the cause, and the run needs a higher `SlotLagCeiling` or a source that writes less. Abandoning the run (`DropShadow`, `decode.DropSlot`, `Store.Delete`) releases the WAL in either case |
| `applier.ErrBatchDeadlocked` (decoding target) | The copy and the catch-up kept writing the same unique values into the shadow, and the server rolled the catch-up's flush back to break the deadlock `CatchupOptions.DeadlockAttempts` times in a row, each retry waiting one `CatchupOptions.DeadlockBackoff` longer than the one before; the rolled-back flushes committed nothing, the confirmed position never passed their changes, and the run stopped with the slot in place under the row. The error answers `errors.Is(err, applier.ErrBatchDeadlocked)`, and `errors.As` to `*pgconn.PgError` gives the server's `deadlock_detected` (SQLSTATE `40P01`) | Retry `Run`: the stream reopens at the row's position, at or below what the catch-up confirmed, and flushes the same changes again. Deadlocks that recur run after run are a source whose writes keep moving unique values through the range the copy is filling; retry once those writes ease, or raise `RunOptions.Catchup.DeadlockAttempts`. The n-th retry waits n `DeadlockBackoff`s, so a whole run of deadlocks waits `DeadlockBackoff × DeadlockAttempts × (DeadlockAttempts − 1) / 2` in total: about 11 s at the defaults (10 attempts, 250 ms), 47 s at 20 attempts, 5 minutes at 50. The slot holds WAL for all of it, with the slot lag ceiling still judged every cycle, so raising the attempts can turn a deadlock stop into a `*decode.SlotLagExceededError` stop; that stop carries the run of deadlocks on its `Deadlocks` field — the count in a row and whether the batch was backing off — so a host can still name the deadlocks. While a cycle waits out a backoff, `Status.BackingOff` is set |
| `ErrCutoverRolledBack` / `ErrLockRetriesExhausted` | The swap did not commit; the source is still live and the row is in `cutover` | Retry `Run`: the row is answered by `InspectSwapped`, which sends the run back through verify and gate to a fresh swap attempt |
| `ErrTableLockLost` (`shadow-lock-lost`) | The lock session dropped mid-run; whatever was in flight was cancelled with it | Retry `Run` once the database is reachable; the row says where to continue |
| An error wrapped `after the checkpoint in phase …` | The row saved; `AfterCheckpoint` returned an error after it | The row and the run are consistent; retry `Run` when the hook's cause is cleared. A hook that fails at the first save leaves the shadow, since the row records it. A hook that fails at the done save stopped nothing: `Run` returns the completed `Result` beside the error, with `Result.Checkpoint.Phase` in `PhaseDone`, and a retry would start a new change |
| `context.Canceled` / `DeadlineExceeded` | The caller stopped the run; a cancelled copy saves what its committed chunks landed before returning. A caller that cancelled with a cause of its own (`context.WithCancelCause`) gets both: the error answers `errors.Is` for the cause and for `context.Canceled` | Retry `Run`; the copy continues from the saved watermark |
| `*schemachange.CatchupWaitError` (decoding target) | The caller stopped the run while it waited for the stream to confirm past a marker — before the verification pass, or again after a resume — and the error carries what the capture was waiting on at that moment: `Marker`, `ShortOfMarker()` (the bytes the confirmed position trailed it, known even before the catch-up's first cycle), and `Status` with the confirmed position and the counts of changes the source had not yet made visible (`Invisible`), deferred behind an in-flight chunk or a batch a constraint refused (`Deferred`), or held until the stream passes a read the flush made (`Held`). `Err` is what ended the wait: the context's error, or whatever an injected `RunOptions.Sleep` returned. It still answers `errors.Is` for `context.Canceled` / `DeadlineExceeded` and for the caller's own cause, so routing is unchanged; the row keeps the phase it waited in, `catching_up`, or `verifying` for a row resumed there | One error is one snapshot: store `Status.Confirmed` from each stopped run and compare it with the next. A confirmed position that moved is a slow copy or a busy source, and `Run` again continues it; one that never moved while `Invisible` stayed above zero is a commit the source will not make visible — a writer waiting on `synchronous_standby_names` with no standby to confirm — and no retry drains it until the source does |

A refusal from either read leaves the row and the slot as they were. A `Reconcile` that fails
after its marker leaves the row in `reconciling`, and `InspectResume` answers that row as
reconcile whatever the catalog shows under the slot name, so the adapter repeats `Start`
rather than deciding anything itself. A `Reconcile` whose clear of the shadow above the
watermark cannot take its fence within the caller's `lock_timeout` — an earlier run's
transaction still writing into the shadow — returns the server's `55P03` as a
`*pgconn.PgError`, not a refusal. It fails before the marker and before any slot command, so
the row, its watermark, and the shadow are as they were, and repeating `Start` once the
writer has ended is safe.

#### What a live `Run` holds open

A `Run` on a decoding target keeps server sessions outside the pool it is handed, and draws on
the pool besides; an importer running many tables from one process sizes for both:

- **Outside the pool, dialled from the `dbconn.Config`:** the table-lock session for the whole
  run; one replication connection to create the slot on a first start, closed once the row
  naming it commits; one for the capture's stream while the row is in `copying`,
  `catching_up`, or `verifying`; and one for the drain's stream during each swap attempt,
  opened after the capture has ended — one at a time, so the two free `max_wal_senders`
  preflight requires leave one as headroom; one plain connection that
  holds the slot's advisory lock only while the run makes its slot and commits its first row
  (`Store.WithSlotLock`); and one plain connection for the drain's marker and slot reads during
  each swap attempt. None of these waits on the pool, so a claim or a swap never holds a pool
  connection while waiting for another — as many live runs at once as the pool has connections
  all finish (`TestRunnersOnOnePoolDoNotStarveEachOther` runs four on a pool of four;
  `TestSlotLockLeavesThePoolToTheCallback` runs a claim on a pool of one).
- **From the pool:** `Copier.Workers` connections (default `copier.DefaultWorkers`) while the
  copy runs, one at a time for the capture's flushes and the checkpoint saves, one for the
  verification pass, and one for the swap transaction. A pool of only the workers would
  leave the catch-up's flushes waiting behind the copy while the slot's lag grows, so no
  entry point admits one. The floor is
  `schemachange.MinPoolConns(Copier)`, which `migrate.CopySwapMinConns(Copier)` returns
  too, and the sizing rule does not depend on which entry point a host calls: `Runner.Run`
  refuses a smaller pool before it takes the table lock
  (`TestRunnerRefusesAPoolBelowTheFloorBeforeCreatingAnything`,
  `TestRunnerRefusesAPoolBelowTheFloorBeforeTakingTheTableLock`), and the front doors
  (`migrate.Run` and `migrate.RunDesired`) refuse it earlier still, before the preflight; in
  every case nothing is created and the error matches
  `errors.Is(err, executor.ErrPoolTooSmall)`. Only the front doors turn the refusal into a
  verdict (`code: pool-too-small`, with the remedy in `detail`); `Runner.Run` returns the
  wrapped sentinel alone, so an adapter on it maps the error itself, and
  `executor.OutcomeCode(err)` answers `pool-too-small` for it. The error's text carries only
  the connection counts (needed, copy workers, held), no table or connection names, so it is
  safe to render. A nil pool is refused with `schemachange.ErrInvariantViolation`. The floor
  is the workers plus two, six with the default workers: one connection beyond the workers
  keeps the flushes landing while every worker holds its connection
  (`TestRunCopySwapFlushesAtThePoolFloorWhileEveryWorkerWaits`),
  and the second is headroom so the flushes and the checkpoint saves and progress reads do
  not queue behind one another. The check also runs when a stopped
  run resumes or is restarted, so a run stopped before the pool was shrunk is refused, and
  keeps its slot or fence and its checkpoint row, until the pool is raised again. While a
  stopped live run is refused this way its slot keeps retaining WAL on the source, so a host
  alerts on the slot's lag and raises the pool promptly. The check runs before the row is
  read, so it applies whatever the row still needs: a row past its swap that has only the old
  table left to drop, and a `Restart` that only discards, are refused on a pool below the
  floor too. The floor
  is per run: a host running several copy-and-swap runs at once from one pool sizes it for
  all of them. A host that builds its pool with
  `pkg/dbconn` sizes it with `dbconn.Config.MaxConnsFloor`, which raises only the default
  size and keeps a `pool_max_conns` the URL sets. A host that opens its pool another way
  sets the pool's maximum (`pool_max_conns` on the DSN, or `pgxpool.Config.MaxConns`) to at
  least `schemachange.MinPoolConns(Copier)`: `pgxpool`'s default is the larger of 4 and the
  machine's CPU count, below the default floor of six on a machine with fewer than six CPUs,
  and every run on such a pool is refused.
- **Which pool:** build it with `dbconn.NewPool`. That pool bounds every session's
  `lock_timeout` and `statement_timeout`, proves session affinity, and drops a `pg_catalog`
  entry that `search_path` lists after a user schema, so no lookalike catalog name can answer
  the engine's reads. `checkpoint.NewStore` accepts any `*pgxpool.Pool`. On a pool built another
  way, these reads spell every relation, function, cast, and operator through `pg_catalog`, so
  they hold whatever its `search_path`: `Store.Ensure`, `Store.Load`, the reaper, the slot
  lock, `decode.MarkWAL`, `applier.ReadSnapshot`, and the catch-up's position read. `Store.Save`
  and `Store.Delete` qualify their own statements, but first confirm the table lock through a
  `pkg/dbconn` read whose comparisons are not qualified yet, so a lookalike `=` on that path makes
  them refuse rather than write. Such a pool also brings its own timeouts or runs without them.

### What `Runner.Run` returns

`Run` returns a `Result` only with a nil error; with an error it returns the zero `Result`, so
read no field of it then. A returned `Result` means the swap committed and the row is `done`
(`Result.Checkpoint`). What the adapter can record about the swap depends on how the run met it:

| How the run met the swap | `Swapped` | `SwapProven` | `OldTableDropped` | `Verification` | `CarriedCompare` |
| --- | --- | --- | --- | --- | --- |
| This run's cutover made it | The proof of the renames | `true` | `true`, or `false` under `KeepOld` | This run's pass | The swap's compare; zero when it compared no key |
| A resume found it committed with the retained source still there | The proof re-derived from the catalog | `true` | `true`, or `false` under `KeepOld` | Zero: no pass was made | Zero: the catalog does not record it |
| A resume found it committed with the retained source already dropped | Zero `SwapProof` | `false` | `true` | Zero: no pass was made | Zero: the catalog does not record it |

`SwapProven == false` does not mean the swap is in doubt: it committed, but the renames it
performed can no longer be read back once one side of them is gone, so there is no proof to
persist; `Shadow` still names the table that is now live. Record the swap's renames only when
`SwapProven` is true. `Reconciled` and `Reconciliation` say whether a live run replaced a lost
slot on its way there. `CopyPaced` says how far the stream's lag held this run's copy back:
how many chunk claims waited, how many of those waits ran out, and the time spent waiting. It is
the zero `copier.Paced` for a quiesced run, for a run that resumed past its copy, and for a copy
the stream kept up with. Like every field it is not returned with an error; a run the slot lag
ceiling stopped carries the same count on the `*decode.SlotLagExceededError` it returns
(`CopyPaced`), so a run that ended `stream-behind` still says whether its copy was held back.
`CarriedCompare` says what
the swap paid under its `ACCESS EXCLUSIVE` lock to compare the rows a live verification pass
could not judge because the application kept rewriting them: `Keys`, how many it read on
each side, and `Elapsed`, how long those reads ran inside the lock. That is the compare's
share of the lock, not the whole hold, which also covers the drain, the checks, the renames,
and the commit. It tells a swap that compared keys from a plain one; a key that differed
would have refused the swap as `cutover-carried-diverged`, so a returned `Result` always
found them equal. It is zero for a quiesced run, for a live pass that left no key, and for a
run that resumed past its swap. A swap whose commit the run never heard back about, and
then found committed in the catalog, keeps the compare its attempt ran. `pkg/migrate` puts
the same count and time in the verdict's `Detail`.
The compare reads at most `checksum.MaxCarryKeys` rows on each side, bounded otherwise only
by the swap's `statement_timeout`, which rolls the swap back with the source live.

### Routing the checkpoint store's own refusals

Before `Load` can be guarded on the statement's fingerprints, the resume reads the row with
`checkpoint.Store.Inspect`, guarded on the row format and the phase alone, because the row's phase and
shadow proof are what tell it which statement it is resuming. `Inspect`, `Load`, and `Save`
share a set of typed errors about the table and the row format that are about the engine
installation, not the run, and each means a different action:

| Error | What it means | Orchestrator action |
| --- | --- | --- |
| `ErrTableMissing` | The `pgsprite.pgsprite_checkpoint` table does not exist in this database; `Ensure` has never run here | Run `Store.Ensure` (the engine role needs database-level `CREATE`, or the table is [pre-provisioned](engine-role.md)) and read again |
| `ErrTableBehind` | The table exists but lacks a column this engine uses: the engine was upgraded and `Ensure` has not run since | Run `Store.Ensure`, which adds the missing columns, and read again; a retry without `Ensure` reproduces it |
| `ErrTableNewer` (`*checkpoint.TableNewerError`, `Unknown` naming the columns) | The table carries columns this engine does not know: a newer engine shaped it, or an operator added a column by hand. `Ensure` ran no DDL | Operator action — run the newer engine, roll the table back to this engine's shape, or drop the hand-added column; retrying this engine cannot succeed while the columns are there |
| `*IncompatibleError` with `Mismatch == MismatchFormat` and `Stored.FormatVersion < checkpoint.FormatVersion` | The row was written by an older engine whose row format this engine does not read | Operator action: finish or abandon the old run. When no run is in flight, `Store.Delete(Stored)` is the fresh start; the row is never silently rewritten |
| `*IncompatibleError` with `Mismatch == MismatchFormat` and `Stored.FormatVersion > checkpoint.FormatVersion` | The row was written by a newer engine | Upgrade the engine; this one must not touch the row |
| `*IncompatibleError` with `Mismatch == MismatchPhase` | The row stands in a phase this engine has no name for (`Have` is the stored name, `Want` the names this engine knows): a newer engine of the same row format wrote it, since the phase set only grows within a format | Upgrade the engine, or finish the run with the engine that started it. This engine must not touch the row: `Save` refuses it the same way, and a restart (`--restart`, `RunOptions.Restart`) cannot clear it, since the refusal comes before any discard. Abandoning the run by hand needs more than the row, and this engine cannot tell from an unknown phase whether the newer engine's swap already committed: if no relation bears the shadow's name, the swap may have committed — a retained `_old` relation says it did — and the run is the newer engine's to finish. Otherwise, with no run in flight, `DropShadow` the shadow, `decode.DropSlot` the row's slot or `DropWriteFence` a fenced source, as for `resume-shadow-gone`, and only then `Store.Delete(Stored)` |

Releases through v0.3.3 write format 2, whose model fingerprints leave out the table's
storage parameters and its columns' sequence declarations. A copy-and-swap run in flight across the upgrade to format 3 surfaces as
`MismatchFormat` with `Stored.FormatVersion == 2`: finish it with the release that started
it, or, once no run is in flight, `Store.Delete(Stored)` and start over.

The release that adds the shadow proof's `conversion_fingerprint` keeps the row format, but a
copy-and-swap run whose statement retypes a column with `USING` and that is in flight across
the upgrade cannot resume on the new engine: the row's proof carries no fingerprint, the
resume re-derives one, and the proofs differ, so the resume is refused as
`resume-row-mismatch`. Finish such a run with the release that started it, or abandon it with
a restart (`--restart`, `RunOptions.Restart`), which drops the shadow and copies the table
again. Runs whose statement has no `USING` conversion resume unaffected.

`ErrTableBehind` wraps the server's own `42703` undefined-column error, so an adapter that
logs the chain sees both the engine's classification and the server's words. `ErrTableNewer`
comes from `Ensure`'s read of the table's shape, before any statement could fail, so it
carries no server error; the `*TableNewerError` names the columns instead.

### Verdicts of a statement routed to copy-and-swap

The tables above are the engine's own vocabulary — `Runner.Run`'s errors and refusals — for
an adapter that drives the copy-and-swap packages directly. The imperative front door,
`migrate.Run`, folds that vocabulary into the same one-verdict contract every other route
reports through, so an adapter that already consumes `verdict.Verdict` reads a copy-and-swap
run without a second error taxonomy. A statement the router assigns to the copy-and-swap
backend ends in exactly one of three verdicts:

| Verdict | What it means | Orchestrator action |
| --- | --- | --- |
| `outcome: executed-via-copy-and-swap` | The shadow was built, filled, verified, and swapped in under the source's name; `detail` says how many rows and chunks the verification pass covered, whether the retained source was dropped or kept under its `_old` name, whether the run first reconciled a lost slot, and — for a run that blocked writes (`--block-writes`) — that the source's writes were fenced for the run's length, since exit 0 otherwise reads as online-safe; `writes_blocked: true` says the same for a machine, on this outcome and on a failed run whose fence went on: one that left its fence standing, or that was abandoned after the fence went on. A blocked-writes run that stopped before its fence went on, for example at a table lock another session holds, reports `false`, since the table kept taking writes; its detail says so and that running the statement again starts from the beginning. A run whose done row is saved is this outcome even when what came after failed — `OnCheckpoint` handed the done row, or the release of the lock session; the detail then names that error. `swap_lsn` is the server's WAL position, as `pg_lsn` text, read inside the swap transaction under its lock right before it committed: a write to the table that committed below it is in the retained source and was carried over, and writes after it exist only in the new table. It is absent when the run did not hear its own swap commit — a run resumed after the swap, or one that learned of the commit from the catalog after losing its connection. `verification` carries the verification pass's figures — `rows`, `chunks`, `settled`, `unsettled`, and `carried` (`detail` states only the first three), the number of keys the live pass left for the swap to compare under its lock (a count, never the keys), every figure present, zero included — so an adapter records them without parsing prose; it is absent when the pass compared no chunk: a run resumed after its swap, or an empty table, and from a pg-sprite build older than the object, so an adapter that must tell those apart records the binary's version beside the verdict. On this outcome a non-zero `unsettled` or `carried` is a record, not a warning: the swap compared those keys under its lock and would have refused had any differed. The error is nil | Done. Exit 0 at the CLI; the outcome token is distinct from `executed-natively` so an adapter can tell the two paths apart in its record |
| `outcome: refused`, `reason: copy-and-swap-refused` | The route's own gates declined: the run did not open the route (`cause: copy-and-swap-not-enabled`, `class: capability-boundary` — the route is in preview and runs only when `CopySwap.Enabled` is set; raised before the route's preflight runs or anything is written, after target resolution and planning have read the catalog), the preflight found a table shape or an environment it will not run against, or a shadow operation refused mid-run to keep an invariant. `cause` is the route's token unchanged — a `copy-and-swap-…` cause from the gate or the preflight, a `shadow-…`, `cutover-…`, `resume-…`, or `copy-…` cause from the shadow operations — and `class` is its routing class. The error is nil | Route on `cause` with the two tables above and [refusal-classes.md](refusal-classes.md#copy-and-swap-refused-keyed-on-copyswaprefusalcause). The not-enabled refusal and a preflight refusal touched nothing. A refusal raised after the row was saved — a `cutover-…` or `resume-…` cause — leaves the row, the shadow, and the run's hold on the source in place: the write fence of a run that blocked writes, or the replication slot of a live run, which pins WAL until it is dropped; the cause says whether the next run clears it or the run is abandoned first. A `migrate.Run` verdict refused `copy-and-swap-pk-unsupported` for a table with no primary key carries `guidance` `add-integer-primary-key`, the online key addition after which the change runs again and meets the table's other shape checks; render it beside `cause` and `detail`. `migrate.RunDesired` carries the same cause and class and no `guidance`. A missing privilege is not this reason: it is the same `insufficient-privileges` refusal the native route reports, with the exact `GRANT` in `detail` |
| `outcome: failed`, with `code` | The run stopped on an error that is not a refusal; the error is returned alongside the verdict. `code` is `cancelled-by-caller` when the caller's context ended — whatever cause the caller cancelled it with: the code says only that the context ended, so an adapter that cancels with reasons of its own (a lost lease, a failed health gate) tells them apart with `errors.Is(err, <its cause>)` on the returned error, `invariant-violation` when the engine caught a breach of its own contract, `data-fault` when the server rejected the rows as the statement would shape them — a `*pgconn.PgError` in SQLSTATE class `22` or `23`: a value the new type will not hold, a constraint the copied rows break — `pool-too-small` when the pool holds fewer than `migrate.CopySwapMinConns(Copier)` connections — refused before the preflight with nothing created, the error matching `errors.Is(err, executor.ErrPoolTooSmall)`; resize the pool rather than retry — and `execution-failed` for everything else — a divergence, an unsettled chunk, a rolled-back swap, a lost lock session, exhausted lock retries, a slot past its lag ceiling | `detail` says what the stop left and what to do with it. A data fault would stop the same way on every resume, so the Runner abandons the run itself before returning (`*schemachange.AbandonedError`): the shadow is dropped, the fence is taken off the source or the slot is dropped, and the checkpoint row is deleted — nothing is held; the verdict says so with `abandoned: true`; correct the data or the statement, then run it again without any acknowledgement, and the run starts from the beginning. Every other stop leaves the checkpoint row at its last save with the run's hold on the source — the fence or the slot: a divergence (`*checksum.DivergenceError`) is wedged in `verifying`, and a data fault whose abandon itself failed (the same `code: data-fault` with `abandoned` unset; the detail says the run "could not be abandoned") is held the same way; for both the detail says to correct the data or the statement and then start the run over with the restart acknowledgement (`--restart SCHEMA.TABLE`; `CopySwapOptions.Restart`), which drops the shadow, takes the fence off the source or drops the slot, deletes the row, and runs the change from the start. Any other stop says that running the same statement again resumes from the row. The detail is written for the CLI operator and names CLI flags (`--restart SCHEMA.TABLE`, `--slot-lag-ceiling SIZE`), so an adapter that shows its own users a remedy builds it from the returned error, such as `*decode.SlotLagExceededError` and its `Cause`, rather than relaying `detail`. The `Run` outcome table above is the finer map. Match the wrapped error with `errors.Is`/`errors.As` for the finer routing — the verdict's `code` is the coarse class |

The adapter supplies the route's policy on `migrate.Options.CopySwap`: `Enabled`, which opens
the route for the run (the route is in preview; the zero value keeps it closed, so an adapter
that has not decided to run copy-and-swap gets the `copy-and-swap-not-enabled` refusal for
every statement the router assigns to the backend, and every native statement runs as
before); the connection the pool was dialed with (the route opens dedicated sessions — the table lock, the copy workers,
the replication stream — so the pool alone does not suffice; a statement that routes to
copy-and-swap with the connection unset is a caller wiring error: `Run` returns
`migrate.ErrCopySwapConnectionRequired` with no verdict, before the preflight, so match it with
`errors.Is` and fix the wiring rather than routing it as an outcome; the connection must also
reach the pool's database, because the table lock is an advisory lock taken on a session
opened from it, and an advisory lock excludes only runs in the database it was taken in. A
connection that is set but reaches another database than the pool — another database of the
cluster, or a database of the same name on another cluster — is the other wiring error,
`*migrate.CopySwapConnectionMismatchError`, returned the same way after the route has compared
both sides' `pg_control_system()` system identifier and `current_database()`, so the run stops
before it takes its lock or writes its checkpoint row. Match it with `errors.As` and do not
retry it: like `ErrCopySwapConnectionRequired` it is an error with no verdict, and no retry
corrects the wiring. Its message names both databases and both clusters' system identifiers,
a stable fingerprint of each server, so an adapter that posts results to a shared surface
logs the typed fields and shows a fixed line. The comparison cannot tell a cluster from a
physical copy of it — a standby, or a server restored from a base backup or a snapshot keeps
the source's system identifier — and a run wired across such a pair stops later instead, as
the environmental `shadow-lock-unconfirmed` refusal, when its first working transaction
finds no lock to confirm. A host that calls `schemachange.Runner` directly skips this
comparison, so a lock connection and a pool on different databases end the same way:
`shadow-lock-unconfirmed` on the first working transaction, before anything is written.
Although the class is `environmental`, re-acquiring the lock does not help there — every
attempt confirms from the wrong database — so escalate it as a wiring error. The check reads
`pg_control_system()`, which PostgreSQL grants to `PUBLIC`; a server that withholds it stops the route with an ordinary error), the free space on the table's
volume for the preflight's disk-headroom check, whether to keep the retained source under
its `_old` name, and whether the run blocks the table's writes — offline mode, the fenced path
(a quiesced target, in the engine's terms) that needs no logical decoding but refuses every
write to the table with `55006` until the swap — or is live. `BlockWrites` is a typed acknowledgement, not a switch: it must name the resolved target
as `SCHEMA.TABLE`, the same way `Restart` does, or `Run` returns an error before anything
executes, and an acknowledged run records a warn-level audit naming the table before the
fence goes on. `RunDesired` rejects both `BlockWrites` and `Restart`, as it rejects every
acknowledgement ([below](#rundesired-takes-no-acknowledgement)). A stopped desired-plan copy is
resumed by running the plan again, or started over by running the stopped statement through
`Run` with `CopySwap.Restart`. An adapter that builds one `Options` for both front doors passes
`RunDesired` `opts.WithoutAcknowledgements()`, which clears both, and reads which ones it
set aside from `opts.Acknowledgements()` ([below](#rundesired-takes-no-acknowledgement)). Three further
fields let the adapter size and watch the run:
`Copier` (`copier.Options`) sizes the copy — workers, chunk sizes, the chunk time they are
throttled toward — with zero values taking the copier's defaults, and its `Pace` left zero, since a
live run paces its copy by its own stream's lag and refuses a caller's; `CheckpointInterval` is how often
a copy in progress saves its landed watermark (zero takes the Runner's default); and `OnCheckpoint` is
the run's phase signal. A `Copier` value the copier would refuse and a negative interval are
wiring errors refused before the preflight, like the missing connection: an error with no
verdict, matched with `errors.Is(err, schemachange.ErrInvalidOptions)` (a refused `Copier`
value also matches `copier.ErrInvalidOptions` or `copier.ErrInvalidChunkerOptions`). The native route's budget, retry policy, and size ceiling do not govern
this route: the Runner's sessions carry `pkg/dbconn`'s bounded `lock_timeout` and
`statement_timeout`, the cutover runs its own bounded lock retry, and the size ceiling guards
the native route's blind bounded attempt, which this route never makes. A statement routed to copy-and-swap and run with `--force`
does not take this route at all: `--force` keeps its one meaning, the submitted form run
natively as a bounded blind attempt, and the audit record says so.

Progress comes from one `*progress.Tracker` the adapter builds per call and hands to the
front door as `Options.Progress`, then polls with `Tracker.Progress` from its own goroutine
while `Run` or `RunDesired` blocks; the front door calls `Start` and `Finish`, so the adapter
calls neither. The steps are the statements the call runs — one for `Run`, one per planned
statement for `RunDesired` — and the detail is what executes inside the current one: a safer
sequence's statements in turn, a concurrent index build with the server's build counters, and
for a copy-and-swap the `copy`, a live run's `catch-up`, and the `checksum` pass, each with its
engine-measured counters ([the contract](progress-report.md#obtaining-progress)). The tracker
ends `finished` only for an executed outcome; a refusal or a failure ends it `failed`. Rows
copied come from the tracker, not from the `OnCheckpoint` hook, whose rows carry a key
position. `Options.Progress` replaces `Copier.Tracker`, and a policy that sets both is an
error before anything runs: a tracker reads one work source at a time, and the front door
hands it the one the phase in progress owns. A tracker serves one call at a time.
`rows_copied` counts this run's rows, rising monotonically, and
`rows_total` is the source's catalog estimate at the copy's start — `0` for a table `ANALYZE`
has never visited, and possibly below `rows_copied` once the table has grown or on a resumed
run — so a display treats `0` as unknown and a ratio over one as possible. The verdict's
`detail` reports the rows the verification pass covered once the run ends.

`OnCheckpoint` is called after every successful save of the checkpoint row with the row as
written: `UpdatedAt` is zero (use the adapter's own clock for a save time), the
`ShadowProof` bytes are shared with the run (treat a retained row as read-only), and the
slot name, publication name, and applied LSN are PostgreSQL's, to be mapped into generic
metadata before the row reaches an engine-neutral record. A live run that reconciles a lost
slot writes its `reconciling` marker and the resumed row straight to the store, and the hook
sees only the row the reconcile leaves. The hook runs on the run's goroutines and holds the
run up while it runs — on the goroutine that called `Run` for the phase saves, with the
table's lock session held and, for a run that blocks writes, the source fenced, and on the copy's
saving goroutine for the periodic watermark saves, where a slow call delays the next save —
so it must be safe to call from either and should return promptly. An error the hook returns
stops the run at that save as a `failed` verdict with the `after the checkpoint in phase …`
error of the `Run` outcome table above and the row as that save left it; the next `Run` over
the same statement resumes from the row. The done save is the exception: the swap has
committed and the row is done, so the verdict is `executed-via-copy-and-swap` with the hook's
error named in `detail` and a nil error, and running the statement again would start a new
change.

Divergence policy is not a knob on this seam: the front door runs the verification pass under
`DivergenceAbort` (CO-3), and a divergence is a `failed` verdict whose remedy is the
`Run`-outcome table's — start the run over — never an in-place repair. Stopping a run from
another process is likewise not a verb here: a run stops when its context ends, and the next
`migrate.Run` over the same statement resumes it; starting over instead is the restart
acknowledgement, `CopySwapOptions.Restart`, described under
[stopping, resuming, and starting over](#stopping-resuming-and-starting-over).

### Outcomes of a checksum pass under the stream

In the catch-up stage the checksum runs with `checksum.Verifier.CheckLive` while the source
is written and `applier.Catchup` applies the stream, under the policy its setting names
(`abort` or `repair`; `recopy` is refused). A caller that runs `CheckLive` under `repair`
hands the catch-up as both the `ConfirmedSource` and the `FlushGate` that holds its flushes
off the shadow while a repair recopies (`LiveOptions.Flushes`, required under `repair` and
unused under `abort`); `schemachange.Runner` runs the pass under `DivergenceAbort` and hands
only the `ConfirmedSource`, refusing a caller-supplied `Live.Confirmed` or `Live.Flushes`; it lets the pass leave up to `checksum.MaxCarryKeys` keys for its swap unless `Live.CarryKeys` names a smaller bound. Zero there means `MaxCarryKeys`, the opposite of zero on `LiveOptions` itself, and a Runner cannot be asked to leave none: a busy table that used to stop with `*UnresolvedError` (`hot`) now goes on to the swap, which may refuse it with `cutover-carried-diverged` instead, so route that refusal to a person, not to a retry. A pass returns one of six outcomes, each a typed error or a proof, and
each asks for a different action. Whether the pass wrote the shadow is part of the outcome:

| Outcome | What it means | Orchestrator action | Shadow written? |
| --- | --- | --- | --- |
| `Outcome` with `VerifiedShadow` minted (`Live()` true) | Every chunk was equal or settled against the catch-up's confirmed position; the shadow equals the source as of those positions and every change since is in the stream | Hand the proof to `GateCutover`; `Cutover` requires a `DrainFunc` that applies the stream through the final position under the swap lock, and refuses the proof without one (`cutover-undrained`) | No |
| `Outcome` with `VerifiedShadow` minted and `Carried()` naming keys (only when the caller set `LiveOptions.CarryKeys`) | Every key was equal or settled except the named ones, which changed between every two rereads; no divergence was confirmed and no chunk was lagging. The proof says nothing about the named keys, and no `CleanWatermark` is minted | Hand the proof to `GateCutover` and `Cutover` as above. Under the swap lock, after the drain, `Cutover` reads each named key on both sides and swaps only if all are equal; one that differs is `cutover-carried-diverged` with the swap rolled back. A caller that runs its own swap transaction calls `checksum.CompareCarried` there. `schemachange.Runner` sets `CarryKeys` itself | No |
| `*UnresolvedError`, no repairs | A chunk could not be settled: the catch-up did not confirm past the read within the wait (`lagging`), or some key of the chunk changed between every two rereads (`hot`, with `Keys` the number still changing at the last one and `HotKeys` the lowest few of them, so one row written over and over reads apart from churn across the chunk; both describe the chunk the error names, the first that did not settle, and `HotKeys` is diagnostic only: it feeds no decision, and its values are row identifiers, so a host that shows the message beyond its operators shows `Cause`, `Rereads` and `Keys` and keeps `HotKeys` for its logs) and the pass may not leave it for the swap: `CarryKeys` is zero, or more keys are still changing than it allows. Not a finding about the data | Run the pass again — after the catch-up has confirmed further, or when the source is quieter; `Outcome.Report.Unsettled` counts the chunks that did not settle | No |
| `*DivergenceError` (policy `abort`) | At least one chunk held still on both sides across a confirmed position and still differed; the stream delivered everything and the shadow does not match. `Report.Unsettled` may also be non-zero | Stop and report; a divergence under the stream is a defect in the copy/apply ordering [CO-4](invariants.md#co-4--the-copyapply-ordering-invariants) enforces, not a retry: a second pass reads the same shadow and stops the same way, so start the run over (the `*checksum.DivergenceError` row above) | No |
| `*UnresolvedError` with `Outcome.Repairs` listed (policy `repair`) | Every confirmed divergence was recopied and read back clean, but another chunk did not settle | The repairs are committed; run the pass again as above. No proof was minted (CO-2) | Yes — the listed repairs |
| `*RepairError` (policy `repair`) | A repaired chunk was a confirmed divergence again after its recopy | Stop and report; the recopy committed and is listed in `Outcome.Repairs`, and something other than the stream is writing the shadow | Yes — the listed repairs |

A pass that returns a context error, `ErrTableLockLost`, or `ErrPassRunning` decided nothing
about the data and wrote nothing beyond any repairs it lists. Progress during a pass is the
tracker's: `chunks_mismatched` counts the chunks the sweep found differing, which under the
stream are chunks being re-checked rather than confirmed divergences, `chunks_reread`
every settling reread, and `settle_rows_read` the rows the settling read, the sweep's row-by-row
read of each differing chunk included; the pass's final figures are the `Report` it returns.

## Invalid-index outcomes of a concurrent index build

`BuildIndexConcurrently` never drops an index. When an invalid index occupies the requested
name — or the target table carries debris a previous recovery quarantined under
`pgsprite_abandoned_<oid>` — the build refuses with a `*executor.InvalidIndexError` whose
`Cleanup` sentinel and outcome code say how strong the proof of abandonment is, and
`Recoverable()` says whether `executor.RebuildAbandonedIndex` (same statement, same budget,
a pool one connection larger) will remove it. The refusal is deterministic: **retrying the
build unchanged reproduces it**, so an adapter that classifies these codes as transient
operational errors retries forever. Route them ([runbook](invalid-index-recovery.md)):

| Outcome code | `Recoverable()` | Orchestrator action |
| --- | --- | --- |
| `invalid-index-own-leftover`, `invalid-index-abandoned`, `invalid-index-builder-unobservable` | yes | Run `RebuildAbandonedIndex` with the same statement (opt in explicitly — the build never does it for you); it removes the entry only after proving it abandoned under the table lock, or reports `*BudgetError` (`CauseLock`) and touches nothing when a build it cannot see holds the lock. When the apply was cancelled and must not resume — the leftover is to be removed, not rebuilt — run `DropAbandonedIndex` instead: the same statement, budget, and proof, stopping after the drop, on a pool of two connections rather than three |
| `data-fault` (the build failed on the table's rows: duplicate values under a unique index, or an index expression raising on a row) | no | Never rebuild: it meets the same rows and fails the same way. Surface to an operator to correct the rows; the error's `Cleanup` still says what the build left — its own leftover (`ErrBuildLeftInvalidIndex`) occupies the name until it is dropped, by `DropAbandonedIndex` or the operator's `DROP INDEX CONCURRENTLY` ([runbook](invalid-index-recovery.md#a-build-that-failed-on-its-rows-data-fault)). A rerun before the rows are corrected and the leftover dropped is refused as `invalid-index-abandoned`, which the row above sends to a rebuild; that rebuild returns `data-fault` again, and its result is the one to act on. Routing on the typed error (`errors.As` to `*executor.InvalidIndexError`, then `Recoverable()`) needs neither code |
| `invalid-index-build-in-flight` | no | Wait and retry later; the error names the backend PID |
| `invalid-index-other-table` | no | Not this change's index; surface to an operator |
| `invalid-index-not-droppable` | no | A partitioned table's index, an index partition, or a constraint's index — never debris; surface to an operator |
| `invalid-index-unproven` | no | Fail closed; surface to an operator with the catalog queries from the runbook |

`RebuildAbandonedIndex` and `DropAbandonedIndex` refuse with the same codes when the proof does not hold
(the table renamed or replaced, the entry changed, a builder visible), and their
`IndexRecoveryReport` carries what it dropped, what it skipped (quarantined entries the
server will not drop concurrently, left for an operator), and the recovery's whole
`Duration` against the budget the adapter sized as its lease. An adapter that enumerates
`executor.Codes()` exhaustively must map every row above; the set is the contract, and
[execution-model](execution-model.md#outcome-codes) is its canonical list. The rows are
not one retry class: `invalid-index-other-table` and `invalid-index-not-droppable` are
permanent — `Code.Permanent()` says so — while the rest wait on a recovery, a builder, or a
re-taken proof. An adapter sorting these codes into "retry" and "refuse" groups starts from
`Permanent()`, not from the family name — with one exception: the outcome-unknown codes
(`commit-outcome-unknown`, `blocking-outcome-unknown`, `row-security-outcome-unknown`) are
not permanent, yet belong in neither group. The change may have committed, so a blind retry
can replay it onto its own result or resume a sequence from a step that already ran; route
them to catalog inspection before any retry.

## Outcomes of a partitioned index build

`executor.BuildPartitionedIndex` builds one index on a partitioned table, partition by
partition, under the table's advisory lock. It stops at the first error and keeps what it
finished: every partition index already attached stays attached, and the parent's index stays
invalid until every partition has one. **Running the same statement again resumes from there**,
so "what persists" below is also what the rerun builds on. Route each outcome by its code
(`executor.OutcomeCode`), never by error text:

| Outcome | Persists | Retry | Orchestrator action |
| --- | --- | --- | --- |
| `not-create-index` | nothing | no, permanent | The statement is not a `CREATE INDEX`; fix the caller, not the database |
| `unnamed-index`, `if-not-exists-unsupported`, `unqualified-table` | nothing | no, permanent | The `CREATE INDEX` does not name its index, uses `IF NOT EXISTS`, or does not schema-qualify its table. The build needs all three to prove what it creates; fix the statement in the desired file, then re-plan |
| `pool-too-small` | nothing | no, permanent | The pool cannot hold a partition's build session and the reserved verdict session at once; give the pool more connections, then rerun |
| `not-partitioned-table` | nothing | no, permanent | The target is not a partitioned table (it changed since the plan, or it was never one); re-plan so the change routes to the concurrent build instead |
| `foreign-table-partition` | nothing when the tree is first read; whatever was finished when a later pass finds it | no, permanent | A foreign table in the tree cannot carry an index, so the parent's index could never become valid. The build reads the tree again on every pass, so a foreign table attached mid-run is refused after the parent's index and some partitions' indexes have committed. Surface to an operator to detach or replace that partition, then re-plan |
| `partitioned-index-tablespace` | nothing | no, permanent | The statement has a `TABLESPACE` clause, which would reach the parent's index and not the partitions' indexes; remove the clause from the desired file (a partition's tablespace is chosen when the partition is created), then re-plan |
| `unique-index-partition-key` | nothing | no, permanent | The unique index does not contain the partition key of the parent and of every sub-partitioned partition among its plain key columns. `migrate --alter` ends in an `unsupported-statement`, `by-design` refusal; `*executor.UniqueIndexPartitionKeyError` names each table (`Schema`, `Table`) and the columns its key lacks (`Missing`). A host learns the refusal before applying by calling `migrate.AdmitPartitionedUniqueIndex` with a pool from `pkg/dbconn`. A desired-state plan stops earlier, with the server's own error from the scratch build. Add the missing columns to the index, then re-plan |
| `partition-detach-pending` | nothing when the tree is first read; whatever was finished when a later pass finds it | no, permanent | A partition's `DETACH PARTITION ... CONCURRENTLY` did not finish, and the server leaves that partition out when it attaches an index or drops a parent's column. Surface to an operator to finish the detach with `ALTER TABLE ... DETACH PARTITION ... FINALIZE`, then rerun |
| `table-lock-unconfirmed` | nothing | no, permanent | The pool's role cannot see when the session holding the table lock started, so the build cannot confirm the lock is its own. Connect the pool as the role the `dbconn.Config` uses, or grant the pool's role `pg_read_all_stats`, then rerun |
| `partitioned-index-name-taken` | whatever was finished | no, permanent | A relation that is not the requested index holds a name the build needs. The build never renames or drops it; an operator removes or renames the occupant, then the same statement resumes |
| `partitioned-index-unsettled` | everything finished | yes, after a wait | Partitions kept appearing faster than the build covered them. Retry the same statement; if it repeats, involve an operator to stop whatever is creating partitions |
| `invariant-violation` | whatever was finished; nothing on a table that was swapped in | no | The build refused to act on what it could not prove, and an adapter treats every case the same way: stop and surface the error to an operator, who reads its text (the text is for the operator, not for routing). The cases are: a table, partition, or index changed under the build while it ran (ST-7); a concurrent build landed on a replacement table, and its index is left in place and named, because the build cannot prove the index is its own; a partition was detached, renamed, or replaced after its index was built, and the error gives the last-known names of the index and the partition; the `dbconn.Config` and the pool reach different databases or servers, caught before any step (LK-1); the lock session dropped mid-run (LK-1), which also matches `errors.Is(err, dbconn.ErrTableLockLost)`, and the interrupted step can leave an invalid index or a commit whose outcome is unknown, which the rerun removes or reads under its own proof; or the catalog holds what the build does not recognize, such as an invalid attached partition index. The operator inspects the catalog (the parent's index and its partitions), repairs or fixes the wiring, re-plans, and reruns |
| `invalid-index-*` | the invalid index | per the [invalid-index table](#invalid-index-outcomes-of-a-concurrent-index-build) | A partition's concurrent build left an invalid index. A rerun removes a leftover it proves abandoned and builds again, so there is no separate recovery call; the codes that say no (`invalid-index-build-in-flight`, `invalid-index-other-table`, `invalid-index-not-droppable`, `invalid-index-unproven`) route as in that table |
| `commit-outcome-unknown` | unknown for the last step | no, inspect first | A step's `COMMIT` did not return success. Read the catalog (the parent's index and its partitions) before rerunning |
| `budget-lock-exceeded`, `budget-statement-exceeded` | whatever was finished | yes | A step's lock or statement bound ran out. Retry the same statement later; the finished partitions are not redone |
| `execution-failed` | unknown for the last step | no, inspect first | A failure outside the typed set: the statement text holds more than one statement (`statement.ErrNotOneStatement`), a step was cancelled as one side of a deadlock (SQLSTATE 40P01, for example against a concurrent `DETACH PARTITION`), or the server returned another error. Surface to an operator; a deadlock commits nothing of its step, so a rerun resumes |
| `*dbconn.TableLockHeldError` (no code) | nothing | yes, after a wait | Another session holds the table's lock; the error names the backend. Wait and retry |

### Wiring the build through `migrate.Run` and `migrate.RunDesired`

A `CREATE INDEX` on a partitioned table that reaches `migrate.Run` or `migrate.RunDesired` runs
as this build; the plan statement carries `execution: partitioned-index-build`. The build takes
the parent's table lock on a dedicated session, which the pool cannot supply, so the adapter
sets `migrate.Options.Connection` to the `dbconn.Config` the pool was dialed with. It must
reach the pool's database: the lock is an advisory lock, and an advisory lock excludes only
sessions of the database it was taken in. The CLI sets it; a library caller that builds
`Options` by hand does it itself. A statement that needs no dedicated session never reads it.

Two wiring errors stop the run with no verdict and nothing executed. Both are mistakes no retry
corrects, so fix the wiring rather than routing either as an outcome:

| Error | How to recognize it | Cause and fix |
| --- | --- | --- |
| `migrate.ErrConnectionRequired` | `errors.Is(err, migrate.ErrConnectionRequired)` | `Options.Connection` is unset. Set it to the config the pool was dialed with |
| `*migrate.PartitionedIndexConnectionMismatchError` | `errors.AsType[*migrate.PartitionedIndexConnectionMismatchError](err)` | `Options.Connection` reaches another database than the pool, whether another database of the cluster or one of the same name on another cluster. Point it at the pool's database |

The mismatch error names `Options.Connection` and carries the database name and the cluster's
system identifier on both sides: `PoolDatabase` and `PoolCluster` for the pool,
`ConnectionDatabase` and `ConnectionCluster` for the connection. A connection that could not be
opened or identified is an ordinary error, not either of these.

`migrate.RunDesired` checks the wiring for the whole plan before its first statement, so a
native statement planned ahead of the index, such as an `ADD COLUMN`, never commits first. The stop is a failed result that carries the plan and no verdicts, with the
error returned alongside. `migrate.Run` checks it before the statement's first step. The build
checks again when it runs.

An adapter that enumerates `executor.Codes()` exhaustively must map every row above; the set
is the contract, and [execution-model](execution-model.md#outcome-codes) is its canonical
list. The design is in [partitioned-index-build-design](partitioned-index-build-design.md).

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
  rewrite-required refusal (`guidance`), the create path's typed shape refusal
  (`statements[].cause`, `format_version` 3 and later), the invalid-index advisories the diff
  declines to act on (`advisories[]`, `format_version` 15 and later; not collected for a desired
  file that declares row security, so absence there proves nothing), the plan `fingerprint`, and
  `table_exists` all live on the [plan report](plan-report.md) — the dry run's output. The run verdict carries
  the refusal reason as a typed field but its `detail` is display-only prose: an orchestrator
  that wants the typed guidance or cause must obtain the plan report first (dry-run for the
  imperative front door, `diff --json` for the declarative one), then apply, and treat the
  verdict's `detail` as presentation until the verdict carries typed guidance too. The
  imperative dry run can itself end in a refusal verdict instead of a report (an unsupported
  statement kind, or routed steps the executor refuses from their shape alone, such as an
  unnamed index build or a `DETACH PARTITION`, or a statement routed to copy-and-swap on a
  table in a partition or inheritance tree): it exits 2 and carries no `format_version`
  or `fingerprint`, so branch on the exit code and on `format_version` before reading a
  report. For that partition or inheritance tree case `migrate --desired --dry-run` ends,
  exit 2, in the run's whole-plan refusal result, with the report under `plan`, while
  `diff --json` does not read the route's preflight and prints the report with the
  statement's `execute` disposition; an orchestrator that plans through `diff --json` meets
  the refusal when it applies the plan.
- **A rename refusal is an operator decision, not a retry.** `RENAME COLUMN` and `RENAME TO`
  refuse as `not-native-safe-rewrite-required` with class `by-design` and `guidance`
  `expand-contract-column` (a column) or `coordinated-table-rename` (a table). Retrying, or
  waiting for a pg-sprite release, changes nothing: route it to the operator, who either rolls
  the change out as the guidance names or, after coordinating the application deploy, runs
  the rename with `--force SCHEMA.TABLE`. A rewrite-required refusal of class
  `capability-boundary` is the other case: a blocking form the planner cannot rewrite yet.
- **Data loss needs the operator's acknowledgement, passed through.** `migrate.Run` refuses a
  statement that drops a column, a constraint, or a `NOT NULL` with
  `destructive-change`, class `by-design`, unless `Options.AllowDestructive` names the
  resolved schema-qualified table (RF-10) — the same acknowledgement the CLI takes as
  `--allow-destructive SCHEMA.TABLE`. SchemaBot already asks for this decision: map its
  unsafe acknowledgement (`--allow-unsafe` on a plan whose change `IsUnsafe` for data loss)
  to `Options.AllowDestructive` for that statement's table, and never set it for a change
  the operator did not acknowledge. The refusal maps to `engine.ExecutionModeBlocked` until
  then; on `migrate --alter` the reason always means the acknowledgement would lift it, and
  a statement that drops a column and adds it back carries `guidance`
  `alter-column-type-in-place`, the in-place retype that keeps the values. A mismatched
  acknowledgement is `migrate.ErrAllowDestructiveMismatch` with a zero verdict and nothing
  executed. An acknowledged run's verdict carries `allowed_destructive: true`, the
  machine-readable audit record to store beside the operator's consent. The acknowledgement
  covers only the loss: a statement that also needs `Force` still needs it, and a `DROP INDEX`
  run with `Options.AcceptBlocking` needs both, each naming the index's owning table; its
  refusal carries `index_usage` (the writer's `idx_scan`, summed over the leaf indexes of a
  partitioned index with `partitions` counting them, `stats_reset`, `last_idx_scan`,
  uniqueness, `track_counts`, `track_counts_off_by_setting`; times in UTC) to show the
  operator before asking for that consent. If that read fails, `Run` returns the error with a
  zero verdict and nothing executed rather than the refusal: route it as a retryable
  failure, like a connection fault, not as a refusal.
  `RunDesired` rejects it ([below](#rundesired-takes-no-acknowledgement)); a declarative
  drop the operator approves runs through `DesiredRequest.AllowDestructive`, which needs the
  fingerprint of the plan the operator reviewed ([below](#reviewed-desired-drop)).
- **A wrong-table acknowledgement is never retried as is.** Each acknowledgement `Run`
  takes must name a schema-qualified table — the resolved target, or for `AcceptBlocking` the
  table the statement locks — and one that names any other table is
  a usage error with a zero verdict and nothing executed, carrying its own sentinel:
  `ErrForceMismatch`, `ErrAcceptBlockingMismatch`, `ErrAllowDestructiveMismatch`,
  `ErrRestartMismatch`, `ErrBlockWritesMismatch`. Branch on them with `errors.Is`, not on the
  message; a retry with the same input fails the same way. Each is a
  `*migrate.AcknowledgementMismatchError`, so one `errors.As` answers whether the error is
  any wrong-table acknowledgement, and its fields carry what to show: `Acknowledgement`
  (the name as `migrate.Acknowledgements()` spells it, such as `force`), `Table` (the table
  the acknowledgement must name), `Given` (the value it carried), and for `AcceptBlocking`
  `LockingStatement` (the kind of the statement whose lock was acknowledged). `Table` is the
  engine's flat `SCHEMA.TABLE` spelling: an unquoted name folded to lower case, a quoted one
  kept as written, no quoting added. It is exactly what the operator must type, but it cannot be
  split back into schema and table, and it is empty for a statement that names no table. The
  error unwraps to its own sentinel and no other. Read it as the engine returns it; a value an
  adapter builds in its own tests behaves the same only with `Acknowledgement` spelled as
  `migrate.Acknowledgements()` returns it. Where the table came from decides
  the action. An acknowledgement the operator typed goes back to the operator, with the
  message, which carries the resolved table and the one given. One the adapter filled in from
  its own plan means the plan and the live schema disagree — a renamed table, a different
  `search_path`, a stale plan — so plan again against the live schema, and treat a mismatch
  that survives the new plan as an adapter defect, not as operator input. `Run` reports the
  first mismatch only. It checks `Force`, `CopySwap.Restart`, `CopySwap.BlockWrites`, and
  `AllowDestructive`, in that order, before anything runs; `AcceptBlocking` is checked later,
  once the statement has been refused and the table it locks has been read, and
  `AllowDestructive` again against that table.
- <a id="rundesired-takes-no-acknowledgement"></a>**`RunDesired` takes no acknowledgement in `Options`.** Each of `Force`, `AcceptBlocking`,
  `AllowDestructive`, `CopySwap.Restart`, and `CopySwap.BlockWrites` accepts a loss for one
  statement the operator wrote, and a desired-state run executes statements the plan derived.
  Setting one returns its own sentinel before any database work, and setting several returns
  the first one's, in this order: `ErrForceNotSupported`, `ErrAcceptBlockingNotSupported`,
  `ErrAllowDestructiveNotSupported`, `ErrRestartNotSupported`, `ErrBlockWritesNotSupported`.
  A `BlockWrites` value would let the derived plan, not the operator, decide how many fenced
  copies take the table offline. An adapter that reuses one
  `Options` value for both front doors passes `RunDesired` `opts.WithoutAcknowledgements()`:
  a copy with every acknowledgement cleared and every other field kept, drawn from the same
  list `RunDesired` rejects from, so an acknowledgement added in a later release is cleared
  without an adapter change. The view clears acknowledgements without reporting that one was
  set, and does not carry an operator's consent across doors: an adapter calls
  `opts.Acknowledgements()` before taking the view, which names, in the order above, every
  acknowledgement `opts` carries and so every one the view sets aside, and runs a statement
  the operator approved through `Run`. `migrate.Acknowledgements()` names every
  acknowledgement the engine takes — `force`, `accept-blocking`, `allow-destructive`,
  `restart`, `block-writes`, each also the `migrate` command's flag for it — so an adapter
  that maps its own approval switches onto them can assert in its tests that every one is
  reachable, and learns of one added in a later release. The names are the CLI's flag names,
  so renaming a flag changes them; before v1 there is no compatibility promise. Both return a
  fresh slice, and a policy carrying no acknowledgement returns nil, which `encoding/json`
  writes as `null`. The
  desired-state door's own refusals still apply to the view, so a destructive plan is still
  refused whole.
- <a id="reviewed-desired-drop"></a>**A reviewed desired drop is a pinned plan and a named
  table.** A desired plan that drops a column, a constraint, an index, or a `NOT NULL` is
  refused whole with `destructive-change`, class `by-design`, and nothing runs, unless the
  request carries both `DesiredRequest.ExpectedFingerprint`, the `plan.Report` fingerprint of
  the plan the operator reviewed, and `DesiredRequest.AllowDestructive`, the desired file's
  resolved schema-qualified table. A preview that has been through `plan.RefuseDestructive`,
  as the CLI's dry-run JSON has, carries a `Fingerprint` that covers the refused plan and
  cannot be pinned: pin its `PinnableFingerprint` (`pinnable_fingerprint`) instead. The acknowledgement is not the `Options` field, which
  `RunDesired` still rejects. `AllowDestructive` without `ExpectedFingerprint` is
  `migrate.ErrAllowDestructiveNeedsFingerprint` before any database work, and one naming
  another table is `migrate.ErrAllowDestructiveMismatch` before any statement runs, even
  when the plan drops nothing. A plan whose fingerprint differs from the pin is refused as
  `plan-fingerprint-mismatch` with nothing executed. A pinned plan that drops something and
  also routes a statement to copy-and-swap is refused whole with `destructive-change`, since
  the drop would commit before copy-and-swap is checked against the changed table; make the
  drop and the other change in two plans. Each destructive statement then runs
  under the acknowledgement as it does through `Run` (RF-10), so its verdict carries
  `allowed_destructive: true`, and a desired index drop runs as `DROP INDEX CONCURRENTLY`
  and never holds the table's lock, refusing, dropping nothing, if the index no longer has
  the definition the plan carried. A column, constraint, or `NOT NULL` drop is held to the
  definition its plan carried the same way (`dropped-object-drift`), and a column drop is also
  refused when the column has an index, constraint, or sequence the plan did not list as
  dropped with it; the fingerprint covers each of those definitions and dependents. On a
  partitioned table a column or constraint drop runs and removes it from every partition; a
  column drop fails with `dropped-column-partition-key` when the column is in a partition key
  at any level, a partitioned table with no partitions included, with
  `dropped-column-partition-dependent` when a partition carries an object of its own on the
  column that the plan, which lists only the parent's dependents, did not review, and with
  `partition-detach-pending` when a partition's `DETACH PARTITION ... CONCURRENTLY` did not
  finish, since PostgreSQL leaves that partition out of the drop. A `NOT NULL` drop on a
  partitioned table, and any column, constraint, or `NOT NULL` drop on a table with
  inheritance children, fails with `dropped-object-has-descendants`. All of these are
  permanent: nothing was dropped, and the operator changes the table before re-planning.
  Route on the code: the message names at most five objects and counts the rest. A column
  drop on a partitioned table takes `ACCESS EXCLUSIVE` on the parent and every partition
  under it for the drop's transaction, under the same lock budget as any drop, so one busy
  partition fails it with `budget-lock-exceeded`, nothing dropped, and the retry is the usual
  lock-budget retry in a quieter window. A foreign-table partition loses the column from its
  local definition only; the remote table is not changed. The plan runs in its order, which drops before adds, and statements that
  already committed stay committed if a later one fails. Map SchemaBot's unsafe
  acknowledgement to `AllowDestructive` only together with the fingerprint of the plan it
  showed. `RunDesiredWithRowSecurity` takes the same two fields for a change to the table
  definition; a policy a row security file no longer lists is dropped by the atomic row
  security change without them.
- **Plan-time verdicts are advisory; the executor re-resolves.** The stored verdict exists so
  operators learn about engine limitations at plan time, but it is estimate-based and stale by
  apply time. SchemaBot's own engines re-evaluate the refusal/policy predicate per statement
  at apply time, and the adoption contract requires the same of any adopting engine:
  pg-sprite's `Apply` must re-resolve the routing decision at execution time and act on that
  fresh decision — never treat the stored plan-time verdict as execution authority. This is
  [OC-4](invariants.md#oc-4--toctou-discipline-on-all-async-state) applied to the verdict.
- **A statement that names only an index or a constraint needs the entry point that re-checks
  it.** An adapter that runs planned statements through the executor directly, rather than
  through `migrate.RunDesired`, picks the entry point the way `migrate` does: a plan statement
  (`plan.Statement`, carried over from `schemadiff.Change`) with `Adopts` set runs through
  `executor.ExecuteNativeAdoptingWithProgress` with that `IndexAdoption`, one with `Validates`
  set through `executor.ExecuteNativeValidatingWithProgress` with that `ConstraintValidation`,
  a statement whose `statement.Kind` is `KindRenameIndex` through
  `executor.ExecuteIndexRenameWithProgress`, preflighted against the table that owns the index,
  and a statement with `Drops` set and a column, constraint, or NOT NULL kind through
  `executor.ExecuteNativeDroppingWithProgress` with that `DroppedObject`; a `Drops` of kind
  `index` is a `DROP INDEX` that runs through `executor.ExecuteDropIndexConcurrently` with the
  definition. Each re-reads the index, constraint, column, or NOT NULL under the
  lock it runs with (the dropping session, for an index) and refuses with `adopted-index-drift`,
  `validated-constraint-drift`, `renamed-index-drift`, `dropped-object-drift`, or
  `dropped-index-drift` when it changed after planning. An index drop whose name was taken by a
  different index between its check and the drop reports `dropped-index-replaced` (permanent):
  unlike the drift refusals, an unreviewed index was dropped, and the error names it so the
  operator can check and recreate it. `ExecuteNativeWithProgress` carries no
  planned definition and runs an adoption, validation, or drop unchecked, and refuses an index
  rename under ST-7, since the statement names no table. `Adopts` and `Validates` stay out of
  the plan's JSON, so an adapter that stores plans between plan and apply persists them itself
  or derives the plan again at apply time. `Drops` is in the JSON, covered by the plan's
  fingerprint, so a stored plan carries it.
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

## Stopping, resuming, and starting over

A copy-and-swap run is one process holding one table lock session, and its whole lifecycle
is driven by running the same statement again. The same four operator verbs apply at the
CLI and through `migrate.Run`, plus one the Runner performs on its own; an orchestrator's
`Stop`/`Cancel` map onto them without any cross-process control channel.

| Operator need | What to do | What pg-sprite does |
| --- | --- | --- |
| **Start** a change | `pg-sprite migrate --url … --alter 'ALTER TABLE s.t …'` (with `--copy-and-swap` to open the preview route and `--free-disk`, and `--block-writes SCHEMA.TABLE` to take the table offline for writes instead of streaming them) | Routes the statement; on the copy-and-swap backend it takes the table's lock session, writes the checkpoint row, builds and fills the shadow, verifies, swaps, and prints one verdict |
| **Stop** a running change | Send `SIGINT` (Ctrl-C) or `SIGTERM` once | The run's context is cancelled and the run unwinds through its own stop path: a copy in flight saves the watermark its committed chunks reached, the table lock session is released, the row stays at its last save with the run's hold on the source — the write fence of a run that blocked writes, or the replication slot of a live run, which retains WAL until the run is finished or discarded — and the verdict is `failed` with `code: cancelled-by-caller` (exit 1). A second `SIGINT`/`SIGTERM` ends the process at once with exit 130, before the stop path completes; the row then records whatever the last save landed, and the lock session is released by the server when its connection drops |
| **Resume** a stopped change | Run the same statement again — with `--copy-and-swap` (`CopySwap.Enabled`), since the resume is a copy-and-swap run too — and the same `--block-writes SCHEMA.TABLE` if the stopped run blocked writes | The checkpoint row says where to continue: a stopped copy continues from its watermark, a run past its copy verifies again, a `cutover` row is answered by the catalog (a committed swap finishes from the old-table drop; a rolled-back swap goes back through verify and gate). Unless the slot was lost, nothing is copied twice. A live run whose slot is gone or the server reports lost, or whose row is still in `reconciling` from a reconcile that stopped partway, is reconciled first, with no flag: a new slot, a comparison of every chunk below the watermark against the source, and a recopy of those that differ ([ST-4](invariants.md#st-4--slot-loss-is-a-modeled-state-transition-not-a-crash)); the run then continues as above, and `Result.Reconciled` reports it ([What `Runner.Run` returns](#what-runnerrun-returns)). The same statement resumes through copy-and-swap whatever it would route to against the table's shape by then — a run stopped after its swap committed has already put the statement's shape live, where a drop of the column it dropped would otherwise run natively and fail — and it still needs `--allow-destructive` if it drops something. A different statement on the same table is refused before it runs, whatever it routes to (`copy-and-swap-refused`, cause `copy-and-swap-run-stopped`; the `Runner` called directly refuses it as `resume-row-mismatch`), unless `--restart` names the table ([below](#a-stopped-run-holds-its-table)). A stopped run that blocked writes resumes only with the acknowledgement, and a stopped live run only without it; a run of the other kind is refused (`resume-row-decoding`) and leaves the stopped run's hold as it is |
| **Start over** — discard a stopped change and run it from the beginning | Run the same statement, with `--copy-and-swap` (`CopySwap.Enabled`) and `--restart SCHEMA.TABLE`, where the value names the resolved target exactly | Under the run's own table lock session, discards what the stopped run left in the order a later restart could meet idempotently — the shadow, then the hold on the source (`DropWriteFence` for a blocked-writes row, `decode.DropSlot` for a live row), then the checkpoint row — and runs the change fresh. A leftover shadow that no row records is discarded the same way. The decision is written to the audit log before anything is discarded |
| **Abandon** a change the data refuses | Nothing — the Runner does it | A run whose fill or swap stops on a data fault (a `*pgconn.PgError` in SQLSTATE class `22` or `23`: a value the new type will not hold, a constraint the copied rows break) would stop the same way on every resume, so the Runner discards it the way `--restart` would — shadow, hold on the source, checkpoint row — under the lock session it still holds, and returns `*schemachange.AbandonedError` wrapping the server's error. The verdict is `failed` with `code: data-fault` and `abandoned: true`, and nothing is left on the source. Correct the data or the statement, then run it again; the run starts from the beginning |

**When to start over rather than resume.** Most stops resume: a cancellation, a lost lock
session, a rolled-back swap, a slot past its lag ceiling, an unsettled hot chunk. A data
fault is abandoned by the Runner itself, so there is nothing to resume or to start over: the
next run of the corrected statement is a fresh run. Two stops hold the source and do not
resume, and the `failed` verdict's `detail` says so: a divergence
(`*checksum.DivergenceError`) — the shadow differs from the source and every resume reads the
same shadow and stops the same way — and a data fault whose abandon itself failed (the detail
says the run "could not be abandoned" and names the discard error). For both, correct the
data or the statement, then run it with `--restart`; the corrected run builds its shadow from
the source as it now is. An adapter tells the two data-fault outcomes apart by the verdict's
`abandoned` field, not by the detail's wording: `code: data-fault` with `abandoned: true` is
rerun as written; `code: data-fault` without it is rerun with `--restart`. A `data-fault`
verdict that carries `failed_step` is not a copy-and-swap run at all but a native concurrent
index build that failed on the rows (duplicate values under a unique index): there is no run
to restart, and the build's own invalid index is the leftover to drop once the rows are
corrected ([runbook](invalid-index-recovery.md#a-build-that-failed-on-its-rows-data-fault)). A
`data-fault` verdict from a partitioned table's index build carries neither `abandoned` nor
`failed_step`, and it is not a copy-and-swap run either: there is no run to restart. When
the run proved the invalid index the build left its own, the detail names it and the
partition whose rows broke the index, and once the rows are corrected the same statement
run again drops that index itself and resumes; otherwise the detail names the index but no
partition, and says to inspect it before any removal
([runbook](invalid-index-recovery.md#a-build-that-failed-on-its-rows-data-fault)). On `RunDesired` the
field is on the failed entry of `DesiredResult.Verdicts`, not on the aggregate. An engine from
before the field existed abandons the same way but never writes it, so an adapter that reads
CLI output from binaries of mixed age keeps reading the detail for those.

**What a restart will not discard.** A `cutover` row whose swap the catalog shows committed
is refused with `restart-swap-committed` and left as found: the shadow already bears the
source's name, so there is nothing to start over. Run the statement without `--restart` to
finish — the run drops the slot of a live run, drops or keeps the retained source, and writes
the row `done`.

<a id="a-stopped-run-holds-its-table"></a>**A stopped run holds its table.** While a
copy-and-swap run's checkpoint row stands short of `done`, `migrate.Run` reads it before
routing any statement on the table, without the table's lock. Its own statement, compared as
parsed, goes back to copy-and-swap and the `Runner` decides the resume under the lock, as
above; `--force` does not send it native and is not reported in the verdict, since the
change already has a shadow and a native attempt would leave it behind. Any other statement
on the table, an index rename and a blocking index statement under `--accept-blocking`
included, is refused with
`copy-and-swap-refused`, class `environmental`, cause `copy-and-swap-run-stopped`, and
nothing executes; the `detail` names the stopped statement and its phase. A host finishes the
stopped run by running its statement again, then runs the new one. Before the swap, a new
statement that itself routes to copy-and-swap can start over instead under `--restart
SCHEMA.TABLE`, which discards the stopped run; a statement that routes anywhere else cannot
take `--restart`, so the stopped run has to be finished, or discarded by hand as for
`resume-shadow-gone`. `migrate.RunDesired` refuses the same way, with the same cause, when the
plan for the table is empty while such a row stands — the table matches the desired schema
only because the stopped run swapped it in, and the retained source and the row would
otherwise stay with nothing to finish them; a non-empty plan meets the row statement by
statement through `Run`. A read of the row that fails stops `Run` before a verdict, with
nothing executed. A row that is `done` holds nothing. A row this engine cannot judge — in
another format or an unknown phase (a rolling upgrade), in a checkpoint store an older engine
created that no copy-and-swap run has evolved yet, or in a store the connecting role has no
privilege to read — holds the table exactly while the catalog shows that run's shadow or
retained source (the `pgsprite_…_new` or `pgsprite_…_old` relation the engine derives from the
table's name): the same refusal, whose `detail` names the relation and why the row cannot be
judged; `--restart` lifts it only for a statement that routes to copy-and-swap, whose route
then judges the row itself under the table's lock. With neither relation standing, statements route
as they would with no row. `--dry-run` does not read the row, so a preview can show a route the
apply then refuses or resumes.

**What `--restart` excludes.** It is a copy-and-swap acknowledgement, so it is an error —
before any verdict, with nothing executed or discarded — on a statement the router sends
anywhere else, with `--dry-run`, with `--desired`, or alongside the other two
acknowledgements (`--force`, `--accept-blocking`): each names a different decision about a
different path, and a command carrying two of them is a contradiction, not a combination. An
acknowledgement that names another table than the statement's resolved target is the same
error. `--allow-destructive` is not one of them: it acknowledges the statement's data loss,
and a restart runs the same statement again, so a destructive copy-and-swap restarts with
both.

Through the library, the verbs are `migrate.Run` with `CopySwapOptions.Restart` set to the
acknowledgement, and `schemachange.RunOptions.Restart` on the `Runner` directly; the
`Runner` refuses the committed-swap case as `CauseRestartSwapCommitted`. `migrate.RunDesired`
rejects the field with `ErrRestartNotSupported` before planning anything: a desired plan
resumes a stopped copy-and-swap on its own, and a run that must start over is the stopped
statement run through `Run` — `migrate --alter … --restart SCHEMA.TABLE` on the CLI — before
the plan is run again.

**Discarding without running the change.** An orchestrator's `Cancel` — end the change and
leave nothing behind, without running it — is `schemachange.Runner.Discard(ctx, pool, shape)`.
It takes a `preflight.CopySwapShape` (`preflight.CheckPrivileges` with
`TierCopyAndSwap`, and `LogicalDecoding` set when the stopped run may be a live one, then
`preflight.CheckCopySwapShape`), not a target: a discard grows nothing, so it is not asked for
the disk or the slot headroom the stopped run itself holds. It has no CLI flag or verb.

The shape check reads the table as it stands now, so a table that has changed shape since the
run stopped — a user trigger, a foreign key, or a dependent view added — has no
`CopySwapShape` to hand to `Discard`. The operator then removes the run by hand, with no run
in flight: delete the table's row from `pgsprite.pgsprite_checkpoint`, drop the shadow
(`_pgsprite_…_new` in the table's schema), and for a blocked-writes run drop the
`pgsprite_write_fence` trigger from the table; `pg-sprite reap` (`Store.ReapOrphanSlots`) then
drops a live run's slot and publication, which no row names any more. A `cutover` row is not
removed this way: read the catalog first, since a swap that committed is finished by running
the statement again.

A live row's slot is dropped with `decode.DropSlot`, which waits for a walsender holding it,
bounded only by the context `Discard` is given, so a host's `Cancel` passes a deadline. Every
error comes back beside a `Discarded` that names what the call dropped before it.

| Outcome | What it means | What the adapter does |
| --- | --- | --- |
| `Discarded` with `Nothing()` false | The call held the table lock, and dropped what the stopped run left in the restart's order — the shadow (`Shadow`), the hold on the source (`Slot` and `Publication` for a live row, `WriteFence` for a blocked-writes row), then the row (`RowPhase` is the phase it was in, any phase but `done`, a `failed` row included). With no row, a shadow or fence under the table's names and a slot and publication of its derived name are dropped the same way, the slot as the orphan reaper drops one | Done: the table is as if no run had started, and the next `Run` of any statement starts from the beginning |
| `Discarded` with `Nothing()` true, nil error | Nothing of a run stands on the table | Done. This is the answer to a discard called again after one that completed, so a `Cancel` retried after a crash is safe to repeat |
| `*dbconn.TableLockHeldError` | A run, or another discard, holds the table lock; the call waited for nothing and touched nothing | Stop the run first (cancel its context and wait for `Run` to return), then call `Discard` again |
| `restart-swap-committed` | The row is in `cutover` and the catalog shows its swap committed: the new table is live under the source's name. Nothing was dropped | The change cannot be discarded. Run the same statement again, without a restart, to finish it; undoing it is a new schema change |
| `resume-row-terminal` | The row is `done`: the change completed. Nothing was dropped | Treat the change as applied; a `KeepOld` rollback window is the caller's own to end |
| `resume-row-mismatch` | The row names a slot other than the table's derived name. Nothing was dropped | Operator: the row is not this table's run's; inspect it before deleting anything |
| `resume-slot-foreign` | No row records the table's run, and a slot of its derived name is named by a live row of another table: two tables can derive the same name, and the slot is that table's run's. The shadow and fence before it are gone, and `Discarded` names them | Leave the slot; it is the other run's. Once that run ends, `Discard` again drops the slot if it is still there |
| error wrapping `decode.ErrSlotActive` | No row records the table's derived slot and a process holds it, so the discard, which drops such a slot only as the reaper would, left it; the shadow and fence before it are gone, and `Discarded` names them | Find the holder, end it, and call `Discard` again |
| error wrapping `preflight.ErrCopySwapProofMismatch` | A slot stands to be dropped and the shape was proven without replication access. Nothing was dropped | Prove the shape with `LogicalDecoding` and call again |
| any other error | A drop failed partway, or the context's deadline ended the wait for a walsender holding a live row's slot. `Discarded` names each leftover the call dropped before it; a drop the error interrupted is not named, though part of it may be gone (a slot dropped before its publication). The row still records whatever stands | Call `Discard` again, after ending a holder if one kept the slot; each step is idempotent, and its `Discarded` names only what that call dropped |

## Design constraints the integration imposes

These shape the engine's state and API surface from day one, long before the adapter exists —
they are registered as the `OC-*` invariants in
[invariants § orchestration / control-plane](invariants.md#orchestration--control-plane-oc):

- **Keep PostgreSQL-only machinery inside the adapter.** When the adapter exists, the
  copy-and-swap backend's logical-decoding slot create/cleanup, `REPLICA IDENTITY`, and
  logical-replication preflight, and any later trigger-capture implementation, will all live
  behind `Apply`/`Stop`/`Cancel` so the
  engine-neutral orchestration layer stays untouched
  (OC-6).
- **Shared types stay engine-agnostic** — engine-specific data rides in generic
  `Metadata map[string]string` fields (OC-6).
- **ID namespaces never conflate** — the engine's schema-change identifier is an opaque
  `external_id` to the orchestrator; the orchestrator's user-facing identifier is never routed
  to the engine (OC-5).
- **The orchestrator is an untrusted request source** like the CLI: control requests are
  re-validated against current engine state, fail-closed (OC-1..OC-4; see
  [tcb-model.md](tcb-model.md)).
