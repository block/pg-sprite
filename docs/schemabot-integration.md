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
- [The concrete contract](#the-concrete-contract)
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
| `Plan` | run parse → declarative diff when applicable → classify → route — exported as `diffplan.Plan` in [`pkg/diffplan`](../pkg/diffplan/diffplan.go) (parse via `statement.ParseDesired`, connect via `dbconn.NewPool`); return a `PlanResult` whose `SchemaChange.TableChanges` are `engine.TableChange{Table, Operation (statement.StatementType), DDL, IsUnsafe, UnsafeReason}` |
| `Apply` | start the native executor asynchronously, or map a **not native-safe** refusal to `engine.ExecutionModeBlocked`; return immediately |
| `Progress` | per-table rows-copied / total / percent / ETA / checksum state |
| `Stop` / `Start` | checkpoint and resume (slot + copy + applier watermark) |
| `Cutover` | the deferred, operator-gated atomic swap |
| `Cancel` | abort and **guarantee logical-slot + shadow-table cleanup** |
| `Volume` | map 1–11 onto chunk-time target / parallelism / throttle |
| `Revert` / `SkipRevert` | decline for the copy-and-swap path (like Spirit); only the expand/contract backend could honour them |

A refusal is a first-class planning verdict, not a delegation fallback: it includes the reason,
notes that copy-and-swap support arrives in later phases, and names a safer native alternative
where one exists. The adapter maps that verdict to `engine.ExecutionModeBlocked`; it never invokes
pg-osc or another external copy tool.

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
