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
| apply | **Router + Executor**: run native changes **asynchronously**; return a refusal verdict for non-native-safe changes until later-phase executors land |
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
| `Apply` | start the native executor asynchronously and return immediately — the synchronous core is exported as `migrate.Run` in [`pkg/migrate`](../pkg/migrate/migrate.go) (parse via `statement.ParseOne`, gate via `migrate.Gate` before dialing, connect via `dbconn.NewPool`, policy via `migrate.DefaultOptions` tuned per table): one statement in, one `verdict.Verdict` out, with a three-shape contract — refusal (verdict, nil error), execution failure (failed verdict carrying the stable code and the committed prefix, plus the operational error), or an error with a zero verdict (stopped before executing). `Run` re-resolves the routing decision at execution time, so the adapter never trusts the stored plan-time verdict (see [execution-mode verdicts](#execution-mode-verdicts-and-direct-execution)). For the declarative flow the adapter does not iterate the plan itself: `migrate.RunDesired` takes the parsed desired schema, re-derives the convergence plan, and runs every planned statement back through `Run` — returning per-statement verdicts with committed-prefix semantics ([execution-model.md](execution-model.md)) — so plan-vs-execute drift and per-statement re-gating stay inside the engine, and the adapter can pin the reviewed plan with `ExpectedFingerprint` |
| `Progress` | per-table rows-copied / total / percent / ETA / checksum state |
| `Stop` / `Start` | checkpoint and resume (slot + copy + applier watermark). `Start` reads the row back through `checkpoint.Store.Load`, hands a non-terminal row to `schemachange.InspectResume`, and on `ResumeReconcile` runs `schemachange.Reconciler.Reconcile` before any stream reopens; the slot a reconcile returns in `Reconciled.Slot` is open and the adapter closes it. A reconcile that fails leaves the row in `reconciling`, which `InspectResume` answers as reconcile again, so repeating it is safe. How to classify each refusal is in [the resume path's refusals](#routing-the-resume-paths-refusals) |
| `Cutover` | the deferred, operator-gated atomic swap |
| `Cancel` | abort and **guarantee logical-slot + shadow-table cleanup**; for a running concurrent index build the stop path is `progress.Tracker.CancelBuild`, whose outcomes the adapter maps with the [cancel outcome table](progress-report.md#cancelling-a-concurrent-index-build) — test for `ErrCancelNotDispatched` before any context error, and record the operator's cancel as possibly delivered only when that sentinel is absent |
| `Volume` | map 1–11 onto chunk-time target / parallelism / throttle |
| `Revert` / `SkipRevert` | decline for the copy-and-swap path (like Spirit); only the expand/contract backend could honour them |

Connect through `dbconn.NewPool`, as `Plan` and `Apply` do: a pool from `dbconn.NewPool` drops a shadowing `pg_catalog` search_path entry on every session, while any other pool relies on each read being `pg_catalog`-qualified ([CO-9](invariants.md#co-9--decision-reads-resolve-to-the-real-catalog)).

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
  with the exact missing `GRANT`. `preflight.CheckCopySwap` is the one call the adapter
  makes for the copy-and-swap route: it folds the privilege, shape, and environment checks
  and returns the `CopySwapTarget` every writing step requires; how to classify each of its
  refusals is in
  [refusal-classes.md](refusal-classes.md#copy-and-swap-refused-keyed-on-copyswaprefusalcause).
- **Fan-out is per table.** A `DesiredSchema` is one `CREATE TABLE` plus its indexes, while
  SchemaBot's declarative roots are directories of many tables: the adapter loops `Plan` per
  table and merges into one `PlanResult`. One pool serves the whole fan-out (`Plan` does not
  close it), but each table costs one scratch transaction per plan, and cross-table ordering
  is the adapter's responsibility.
- **So is the set of tables.** A live table with no desired file is invisible to a per-table
  diff, and under a declarative model its only convergence is `DROP TABLE` — which pg-sprite
  refuses at both front doors and never executes. There is no `verdict.Verdict` to map: pg-sprite
  never saw the table. The adapter enumerates the namespace's live tables itself —
  `schemadiff.ListManagedTables` is the exported catalog query `pull` uses, and its
  exclusions (partitions and extension-owned tables) are explained under
  [Deliberately operator-owned](capabilities.md#deliberately-operator-owned) — and
  *synthesizes* an `engine.TableChange` per undeclared table: `ExecutionMode =
  ExecutionModeBlocked`, `IsUnsafe` with a data-loss `UnsafeReason`, and a `ModeReason` that
  names the table and the two remedies (restore or write its file; drop it through a reviewed
  process). The row's `DDL` may carry the canonical `DROP TABLE <ns>.<table>` so the operator
  sees what convergence would mean — the row is blocked, and pg-sprite neither emitted nor
  will run that text. `verdict.ReasonDestructiveChange` is not reused for it: that reason is
  scoped to a plan pg-sprite produced. The point of the synthesized row is the fail-closed
  direction — the divergence shows on the plan and the apply is refused rather than the table
  lingering unreported. This is a deliberate divergence from SchemaBot's MySQL path, which
  plans the drop and, behind its unsafe acknowledgment, quarantines the table through
  pending drops instead of blocking: PostgreSQL gets the block because pg-sprite offers no
  whole-schema convergence and the drop is outside its statement set, so the orchestrator
  holds the only view of the table set. A PostgreSQL quarantine (`ALTER TABLE … SET SCHEMA`)
  would be the orchestrator's mechanism to add, not an engine gap. Onboarding follows from
  the rule: an existing database is entirely undeclared tables, so the adapter declares them
  first (`pull` — one desired file per table) or scopes which namespaces it owns.
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
them when admitting execution. A greenfield run resolves to one of six things:

| Outcome | Routing class |
| --- | --- |
| Executed | The table and its indexes exist; a rerun converges to an empty plan |
| `create-collision` refusal | **Re-plan, then fix the occupant**: the table name or a claimed index, constraint-index, or sequence name is occupied. Re-diff the live catalog to see what holds it; re-planning alone reproduces the refusal — drop or rename the occupant, name a constraint's index explicitly, or for a sequence use an explicitly named sequence or a non-serial column. Never blindly retry |
| `insufficient-privileges` refusal (`*preflight.PrivilegeError`, `Tier == TierCreateTable`) | **Operator provisioning action**: the role needs the exact `GRANT` the error carries — not a desired-file fix, and not retryable until granted |
| Plan/admission refusal (`unsupported-statement`) | **Author action**: the desired file states a shape the create path refuses; retrying unchanged cannot succeed. The [plan report](plan-report.md) (`format_version` 3 and later) names the shape as the refused statement's typed `cause` — read that field rather than matching the executor's sentinels or the verdict's prose |
| `declarative-foreign-key` refusal (`cause` `foreign-key`) | **Not yet supported**: the new table declares a foreign key, and desired-state execution does not run foreign-key changes. The plan still shows the `CREATE TABLE`; create the table without the foreign key, then add it with `migrate --alter` (`ADD FOREIGN KEY` runs as `NOT VALID` + `VALIDATE`) |
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
`checkpoint.Store.Load` for the row, `schemachange.InspectResume` for the mode, and
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
| `restart-swap-committed` (from `Runner`, with `RunOptions.Restart`) | A run asked to start over met a `cutover` row whose swap the catalog shows committed: the shadow already bears the source's name, so there is nothing to discard and the change is past the point of starting over. Nothing is written | Run the same statement without the restart; `InspectSwapped` answers that the swap committed and the run finishes from `DropOldTable` — drops the slot of a live run, drops or keeps the retained source, writes the row `done` |
| `resume-row-mismatch` | The row is for another table, names another slot, or (in `Reconcile` and `Runner`) carries another statement's shadow name, fingerprints, or shadow proof; from `Runner` also a row whose recorded statement is not the one `Run` was given, compared as parsed, or a row that records no statement | Configuration or operator action: the target and the row disagree; retrying cannot succeed. A caller that means a second, different change on the same table deletes the row (`Store.Delete`) — and drops the shadow and the fence — first |
| `shadow-statement-target` (from `Runner`, on resume) | The statement `Run` was given names another table than the proven target, caught before the row's statement is compared with it | Configuration: the statement and the target disagree; retrying cannot succeed |
| `resume-row-decoding` (from `Runner`) | The row was written by the other kind of run: a target proven without logical decoding was handed a row that names a slot or publication, or is in `catching_up` or `reconciling`, and has no slot to continue it with; a target proven for decoding was handed a row that names no slot, whose source was fenced, not captured | Run the row under the kind of `CopySwapTarget` proof that made it; the proof, not the caller, chooses the route |
| `shadow-unrecorded` (from `Runner`) | A relation bears the target's derived shadow name and no row records a run for the target | `DropShadow` the leftover and run again, or run again with `RunOptions.Restart`, which drops it under the run's lock first; a relation the engine did not make is an operator's to rename |
| `resume-shadow-gone` (from `Runner`) | The row's swap has not committed (any phase from `copying` to a `cutover` whose swap rolled back) and it records a shadow under which no relation stands; `ErrShadowNotFound` is reachable through the refusal, and nothing was written | Run the statement again with a restart (`--restart`, `RunOptions.Restart`), which discards the row, its slot or fence, and starts over. By hand, quiesced row: `Store.Delete` the row and run again; the next run builds a fresh shadow, and the source stays fenced until `DropWriteFence` or that run's swap. Live row (it names a slot): `decode.DropSlot` the row's slot, then `Store.Delete` the row, and run again; the slot retains WAL until it is dropped, and a run started while it stands fails creating its own. A live source has no fence |
| `resume-slot-foreign` | The slot of the derived name is physical or another database's; the engine may neither decode from it nor drop it | Operator action on the slot; retrying cannot succeed while it is there |
| `resume-slot-held` | The engine's own slot is held by another backend — a previous run still draining, or a tool reading it | Wait and inspect again; a holder that never lets go is an operator action |
| `cutover-relation-replaced` (from `Reconcile`) | The source or the shadow is no longer the relation the build recorded, read before the reconcile wrote anything | Stop; the table under the names is not the one this run proved — operator action |

`Runner` also ends with errors that are not refusals but still route differently; each leaves
the row at its last save and the shadow in place, with the source fenced (quiesced target) or
the slot under the row (decoding target):

| `Run` outcome | What it means | Orchestrator action |
| --- | --- | --- |
| `*decode.SlotExistsError` (decoding target, first start) | A logical slot of the engine's derived name already exists and no row names it: a run that ended before its row landed, or a slot made by hand. The run made nothing and dropped nothing — the slot, its publication, and the absence of a row are as the run found them | Run `Store.ReapOrphanSlots`, which drops the slot once nothing holds it, and then `Run` again; a slot that stays held is an operator's to inspect |
| `*checksum.DivergenceError` | The verification pass found the shadow differing from the source — under a fence nothing should have written either, and under a stream a difference the catch-up did not settle is a confirmed divergence — so the run stops rather than repair (CO-3 `DivergenceAbort`) and the row stays in `verifying` | Do not retry blindly: a retry runs the same pass over the same shadow and stops the same way. Start the run over — `Run` again with `RunOptions.Restart`, which discards the shadow, the fence or the slot, and the row under the table lock before building afresh; or by hand, `DropShadow`, `DropWriteFence` or `decode.DropSlot`, `Store.Delete`, then `Run` — a difference that recurs under a fence is a writer bypassing it (a role that owns the table and disables it) to find. A writer in `session_replication_role = replica` — a logical-replication subscription's apply worker, a bulk restore with triggers disabled — does not bypass the fence, which is enabled `ALWAYS`: it is refused by it, and a subscription that errors and retries for the run's length is that refusal, not a divergence |
| `cutover-unfenced` (quiesced target, from `Cutover`, through `Run`) | The swap, under its lock, found the engine's write fence on the source disabled, dropped, replaced by a trigger of another shape, or not enabled `ALWAYS`; nothing is swapped and the row is in `cutover` | Retry `Run`: it installs the fence again, verifies again, and swaps; a write that landed while the fence was down makes the verification pass stop on a `*checksum.DivergenceError`. Find what changed the source's triggers |
| `*checksum.UnresolvedError` (decoding target) | The live pass could not settle a chunk the source kept writing within its reread bound; nothing was found wrong and nothing was minted, and the row stays in `verifying`. The bound is `LiveOptions.Attempts` fresh rereads of one chunk (default `checksum.DefaultSettleAttempts`), each waited on for the catch-up up to `LiveOptions.SettleWait`: a chunk written again between every two of its rereads never settles, so a table whose writes land on one chunk faster than the pass can read it twice is the case, whatever the table's total write rate | Retry `Run`: the resumed row catches up again and runs a fresh pass; a chunk that never settles is a hot spot to wait out, write less often, or give more `Attempts` |
| `cutover-drain-deferred` (decoding target) | The swap's drain met a unique-key move it could not order into the shadow; the swap rolled back and the row is in `cutover` with the position the drain reopened from | Retry `Run`: `InspectSwapped` answers `ErrNotSwapped`, the row goes back to `catching_up`, and the same changes are drained again once the rows the move touched have settled |
| `*decode.SlotLagExceededError` (decoding target) | The slot retained more WAL than `CatchupOptions.SlotLagCeiling` while the capture ran; the run stopped with the slot in place under the row | Retry `Run` once the source's write rate allows: the stream reopens at the row's position and drains the backlog. Abandoning the run (`DropShadow`, `decode.DropSlot`, `Store.Delete`) releases the WAL |
| `ErrCutoverRolledBack` / `ErrLockRetriesExhausted` | The swap did not commit; the source is still live and the row is in `cutover` | Retry `Run`: the row is answered by `InspectSwapped`, which sends the run back through verify and gate to a fresh swap attempt |
| `ErrTableLockLost` (`shadow-lock-lost`) | The lock session dropped mid-run; whatever was in flight was cancelled with it | Retry `Run` once the database is reachable; the row says where to continue |
| An error wrapped `after the checkpoint in phase …` | The row saved; `AfterCheckpoint` returned an error after it | The row and the run are consistent; retry `Run` when the hook's cause is cleared. A hook that fails at the first save leaves the shadow, since the row records it |
| `context.Canceled` / `DeadlineExceeded` | The caller stopped the run; a cancelled copy saves what its committed chunks landed before returning | Retry `Run`; the copy continues from the saved watermark |

A refusal from either read leaves the row and the slot as they were. A `Reconcile` that fails
after its marker leaves the row in `reconciling`, and `InspectResume` answers that row as
reconcile whatever the catalog shows under the slot name, so the adapter repeats `Start`
rather than deciding anything itself.

#### What a live `Run` holds open

A `Run` on a decoding target keeps server sessions outside the pool it is handed, and draws on
the pool besides; an importer running many tables from one process sizes for both:

- **Outside the pool, dialled from the `dbconn.Config`:** the table-lock session for the whole
  run; one replication connection for the capture's stream while the row is in `copying`,
  `catching_up`, or `verifying`, and one more for the drain's stream during each swap attempt
  (preflight already requires two free `max_wal_senders` for this); one plain connection that
  holds the slot's advisory lock only while the run makes its slot and commits its first row
  (`Store.WithSlotLock`); and one plain connection for the drain's marker and slot reads during
  each swap attempt. None of these waits on the pool, so a claim or a swap never holds a pool
  connection while waiting for another — as many live runs at once as the pool has connections
  all finish (`TestRunnersOnOnePoolDoNotStarveEachOther` runs four on a pool of four;
  `TestSlotLockLeavesThePoolToTheCallback` runs a claim on a pool of one).
- **From the pool:** `Copier.Workers` connections (default `copier.DefaultWorkers`) while the
  copy runs, one at a time for the capture's flushes and the checkpoint saves, one for the
  verification pass, and one for the swap transaction. A pool smaller than the copy's workers
  slows the copy; it does not stall the run.

### What `Runner.Run` returns

`Run` returns a `Result` only with a nil error; with an error it returns the zero `Result`, so
read no field of it then. A returned `Result` means the swap committed and the row is `done`
(`Result.Checkpoint`). What the adapter can record about the swap depends on how the run met it:

| How the run met the swap | `Swapped` | `SwapProven` | `OldTableDropped` | `Verification` |
| --- | --- | --- | --- | --- |
| This run's cutover made it | The proof of the renames | `true` | `true`, or `false` under `KeepOld` | This run's pass |
| A resume found it committed with the retained source still there | The proof re-derived from the catalog | `true` | `true`, or `false` under `KeepOld` | Zero: no pass was made |
| A resume found it committed with the retained source already dropped | Zero `SwapProof` | `false` | `true` | Zero: no pass was made |

`SwapProven == false` does not mean the swap is in doubt: it committed, but the renames it
performed can no longer be read back once one side of them is gone, so there is no proof to
persist; `Shadow` still names the table that is now live. Record the swap's renames only when
`SwapProven` is true. `Reconciled` and `Reconciliation` say whether a live run replaced a lost
slot on its way there.

### Routing the checkpoint store's own refusals

Before `Load` can be guarded on the statement's fingerprints, the resume reads the row with
`checkpoint.Store.Inspect`, guarded on the row format alone, because the row's phase and
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

Releases through v0.3.3 write format 2, whose model fingerprints leave out the table's
storage parameters and its columns' sequence declarations. A copy-and-swap run in flight across the upgrade to format 3 surfaces as
`MismatchFormat` with `Stored.FormatVersion == 2`: finish it with the release that started
it, or, once no run is in flight, `Store.Delete(Stored)` and start over.

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
| `outcome: executed-via-copy-and-swap` | The shadow was built, filled, verified, and swapped in under the source's name; `detail` says how many rows and chunks the verification pass covered, whether the retained source was dropped or kept under its `_old` name, whether the run first reconciled a lost slot, and — for a quiesced run — that the source's writes were fenced for the run's length, since exit 0 otherwise reads as online-safe. A run whose done row is saved is this outcome even when the release of its lock session failed afterwards; the detail then names the release error. The error is nil | Done. Exit 0 at the CLI; the outcome token is distinct from `executed-natively` so an adapter can tell the two paths apart in its record |
| `outcome: refused`, `reason: copy-and-swap-refused` | The route's own gates declined: the preflight found a table shape or an environment it will not run against, or a shadow operation refused mid-run to keep an invariant. `cause` is the route's token unchanged — a `copy-and-swap-…` cause from the preflight, a `shadow-…`, `cutover-…`, `resume-…`, or `copy-…` cause from the shadow operations — and `class` is its routing class. The error is nil | Route on `cause` with the two tables above and [refusal-classes.md](refusal-classes.md#copy-and-swap-refused-keyed-on-copyswaprefusalcause). A preflight refusal touched nothing. A refusal raised after the row was saved — a `cutover-…` or `resume-…` cause — leaves the row, the shadow, and the run's hold on the source in place: the write fence of a quiesced run, or the replication slot of a live run, which pins WAL until it is dropped; the cause says whether the next run clears it or the run is abandoned first. A missing privilege is not this reason: it is the same `insufficient-privileges` refusal the native route reports, with the exact `GRANT` in `detail` |
| `outcome: failed`, with `code` | The run stopped on an error that is not a refusal; the error is returned alongside the verdict. `code` is `cancelled-by-caller` when the caller's context ended, `invariant-violation` when the engine caught a breach of its own contract, and `execution-failed` for everything else — a divergence, an unsettled chunk, a rolled-back swap, a lost lock session, exhausted lock retries, a slot past its lag ceiling | The checkpoint row remains at its last save with the run's hold on the source — the fence or the slot — and `detail` says what to do with it: a divergence (`*checksum.DivergenceError`) is wedged in `verifying`, and a data fault — a `*pgconn.PgError` in SQLSTATE class `22` or `23`, a value the new type will not hold or a constraint the copied rows break — would stop the same way on every resume, so for both the detail says to correct the data or the statement and then start the run over with the restart acknowledgement (`--restart SCHEMA.TABLE`; `CopySwapOptions.Restart`), which drops the shadow, takes the fence off the source or drops the slot, deletes the row, and runs the change from the start; every other stop says that running the same statement again resumes from the row. The `Run` outcome table above is the finer map. Match the wrapped error with `errors.Is`/`errors.As` for the finer routing — the verdict's `code` is the coarse class |

The adapter supplies the route's policy on `migrate.Options.CopySwap`: the connection the
pool was dialed with (the route opens dedicated sessions — the table lock, the copy workers,
the replication stream — so the pool alone does not suffice; a statement that routes to
copy-and-swap with the connection unset is a caller wiring error: `Run` returns
`migrate.ErrCopySwapConnectionRequired` with no verdict, before the preflight, so match it with
`errors.Is` and fix the wiring rather than routing it as an outcome; a connection that is set
but reaches another database than the pool — another database of the cluster, or a database
of the same name on another cluster — is the other wiring error, `*migrate.CopySwapConnectionMismatchError`,
returned the same way after the route has compared both sides' `pg_control_system()` identity
and `current_database()`, so a mistake that would otherwise surface mid-run as the environmental
`shadow-lock-unconfirmed` refusal — which an adapter routing on class would retry — stops before
the run takes its lock or writes its checkpoint row; match it with `errors.As`), the free space on the table's
volume for the preflight's disk-headroom check, whether to keep the retained source under
its `_old` name, and whether the table is quiesced for the run — the fenced path that needs
no logical decoding — or live. The native route's budget, retry policy, and size ceiling do not govern
this route: the Runner's sessions carry `pkg/dbconn`'s bounded `lock_timeout` and
`statement_timeout`, the cutover runs its own bounded lock retry, and the size ceiling guards
the native route's blind bounded attempt, which this route never makes. A statement routed to copy-and-swap and run with `--force`
does not take this route at all: `--force` keeps its one meaning, the submitted form run
natively as a bounded blind attempt, and the audit record says so.

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
only the `ConfirmedSource`, refusing a caller-supplied `Live.Confirmed` or `Live.Flushes`. A pass returns one of five outcomes, each a typed error or a proof, and
each asks for a different action. Whether the pass wrote the shadow is part of the outcome:

| Outcome | What it means | Orchestrator action | Shadow written? |
| --- | --- | --- | --- |
| `Outcome` with `VerifiedShadow` minted (`Live()` true) | Every chunk was equal or settled against the catch-up's confirmed position; the shadow equals the source as of those positions and every change since is in the stream | Hand the proof to `GateCutover`; `Cutover` requires a `DrainFunc` that applies the stream through the final position under the swap lock, and refuses the proof without one (`cutover-undrained`) | No |
| `*UnresolvedError`, no repairs | A chunk could not be settled: the catch-up did not confirm past the read within the wait (`lagging`), or the chunk changed on every reread (`hot`). Not a finding about the data | Run the pass again — after the catch-up has confirmed further, or when the source is quieter; `Outcome.Report.Unsettled` counts the chunks that did not settle | No |
| `*DivergenceError` (policy `abort`) | At least one chunk held still on both sides across a confirmed position and still differed; the stream delivered everything and the shadow does not match. `Report.Unsettled` may also be non-zero | Stop and report; a divergence under the stream is a defect, or the known synchronous-commit window recorded in [CO-4's *Open* note](invariants.md#co-4--the-copyapply-ordering-invariants), and either way not a retry: a second pass reads the same shadow and stops the same way, so start the run over (the `*checksum.DivergenceError` row above). A chunk that settled is not compared again, so a loss that lands in that window after the pass has settled its chunk is not caught | No |
| `*UnresolvedError` with `Outcome.Repairs` listed (policy `repair`) | Every confirmed divergence was recopied and read back clean, but another chunk did not settle | The repairs are committed; run the pass again as above. No proof was minted (CO-2) | Yes — the listed repairs |
| `*RepairError` (policy `repair`) | A repaired chunk was a confirmed divergence again after its recopy | Stop and report; the recopy committed and is listed in `Outcome.Repairs`, and something other than the stream is writing the shadow | Yes — the listed repairs |

A pass that returns a context error, `ErrTableLockLost`, or `ErrPassRunning` decided nothing
about the data and wrote nothing beyond any repairs it lists. Progress during a pass is the
tracker's: `chunks_mismatched` counts the chunks the sweep found differing, which under the
stream are chunks being re-checked rather than confirmed divergences, and `chunks_reread`
every settling reread; the pass's final figures are the `Report` it returns.

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
  (`statements[].cause`, `format_version` 3 and later), the plan `fingerprint`, and `table_exists` all
  live on the [plan report](plan-report.md) — the dry run's output. The run verdict carries
  the refusal reason as a typed field but its `detail` is display-only prose: an orchestrator
  that wants the typed guidance or cause must obtain the plan report first (dry-run for the
  imperative front door, `diff --json` for the declarative one), then apply, and treat the
  verdict's `detail` as presentation until the verdict carries typed guidance too.
- **A rename refusal is an operator decision, not a retry.** `RENAME COLUMN` and `RENAME TO`
  refuse as `not-native-safe-rewrite-required` with class `by-design` and `guidance`
  `expand-contract-column` (a column) or `coordinated-table-rename` (a table). Retrying, or
  waiting for a pg-sprite release, changes nothing: route it to the operator, who either rolls
  the change out as the guidance names or, after coordinating the application deploy, runs
  the rename with `--force SCHEMA.TABLE`. A rewrite-required refusal of class
  `capability-boundary` is the other case: a blocking form the planner cannot rewrite yet.
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

## Stopping, resuming, and starting over

A copy-and-swap run is one process holding one table lock session, and its whole lifecycle
is driven by running the same statement again. The same four verbs apply at the CLI and
through `migrate.Run`; an orchestrator's `Stop`/`Cancel` map onto them without any
cross-process control channel.

| Operator need | What to do | What pg-sprite does |
| --- | --- | --- |
| **Start** a change | `pg-sprite migrate --url … 'ALTER TABLE s.t …'` (with `--free-disk`, and `--quiesced` when the table takes no writes) | Routes the statement; on the copy-and-swap backend it takes the table's lock session, writes the checkpoint row, builds and fills the shadow, verifies, swaps, and prints one verdict |
| **Stop** a running change | Send `SIGINT` (Ctrl-C) or `SIGTERM` once | The run's context is cancelled and the run unwinds through its own stop path: a copy in flight saves the watermark its committed chunks reached, the table lock session is released, the row stays at its last save with the run's hold on the source — the write fence of a quiesced run, or the replication slot of a live run, which retains WAL until the run is finished or discarded — and the verdict is `failed` with `code: cancelled-by-caller` (exit 1). A second `SIGINT`/`SIGTERM` ends the process at once with exit 130, before the stop path completes; the row then records whatever the last save landed, and the lock session is released by the server when its connection drops |
| **Resume** a stopped change | Run the same statement again | The checkpoint row says where to continue: a stopped copy continues from its watermark, a run past its copy verifies again, a `cutover` row is answered by the catalog (a committed swap finishes from the old-table drop; a rolled-back swap goes back through verify and gate). Nothing is copied twice. A different statement on the same table is refused (`resume-row-mismatch`) |
| **Start over** — discard a stopped change and run it from the beginning | Run the same statement with `--restart SCHEMA.TABLE`, where the value names the resolved target exactly | Under the run's own table lock session, discards what the stopped run left in the order a later restart could meet idempotently — the shadow, then the hold on the source (`DropWriteFence` for a quiesced row, `decode.DropSlot` for a live row), then the checkpoint row — and runs the change fresh. A leftover shadow that no row records is discarded the same way. The decision is written to the audit log before anything is discarded |

**When to start over rather than resume.** Most stops resume: a cancellation, a lost lock
session, a rolled-back swap, a slot past its lag ceiling, an unsettled hot chunk. Two do not,
and the `failed` verdict's `detail` says so: a divergence (`*checksum.DivergenceError`) —
the shadow differs from the source and every resume reads the same shadow and stops the same
way — and a data fault (a `*pgconn.PgError` in SQLSTATE class `22` or `23`: a value the new
type will not hold, a constraint the copied rows break). For both, correct the data or the
statement, then run it with `--restart`; the corrected run builds its shadow from the source
as it now is.

**What a restart will not discard.** A `cutover` row whose swap the catalog shows committed
is refused with `restart-swap-committed` and left as found: the shadow already bears the
source's name, so there is nothing to start over. Run the statement without `--restart` to
finish — the run drops the slot of a live run, drops or keeps the retained source, and writes
the row `done`.

**What `--restart` excludes.** It is a copy-and-swap acknowledgement, so it is an error —
before any verdict, with nothing executed or discarded — on a statement the router sends
anywhere else, with `--dry-run`, with `--desired`, or alongside the other two
acknowledgements (`--force`, `--accept-blocking`): each names a different decision about a
different path, and a command carrying two of them is a contradiction, not a combination. An
acknowledgement that names another table than the statement's resolved target is the same
error.

Through the library, the verbs are `migrate.Run` with `CopySwapOptions.Restart` set to the
acknowledgement, and `schemachange.RunOptions.Restart` on the `Runner` directly; the
`Runner` refuses the committed-swap case as `CauseRestartSwapCommitted`.

## Design constraints the integration imposes

These shape the engine's state and API surface from day one, long before the adapter exists —
they are registered as the `OC-*` invariants in
[invariants § orchestration / control-plane](invariants.md#orchestration--control-plane-oc):

- **Keep PostgreSQL-only machinery inside the adapter.** When the copy-and-swap backend and
  adapter exist, logical-decoding slot create/cleanup, `REPLICA IDENTITY`, logical-replication
  preflight, and any later trigger-capture implementation will all live behind
  `Apply`/`Stop`/`Cancel` so the
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
