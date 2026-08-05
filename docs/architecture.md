# Architecture

The map of the codebase: what the layers are, where each responsibility lives, and
what exists today versus what each build phase adds. For the design rationale behind this shape
start with [high-level-design.md](high-level-design.md); for interfaces and lifecycle internals
see [low-level-design.md](low-level-design.md).

## The three layers

pg-sprite is a decoupled **planner → router → executor** engine. The planner decides *what*
changes, the router decides *which strategy*, interchangeable executors decide *how*.

The planner is itself a pipeline of five distinct stages. The two front-ends enter it at
different points — an imperative `--alter` already *is* DDL, so it goes straight to parse;
a declarative `--desired` schema must first be compared against the live database to
*produce* DDL — but both converge on the same classify → lint tail, so every operation is
judged by the same rules regardless of how it arrived:

```
   user: --alter "ALTER TABLE …"           user: --desired schema.sql
     (imperative: statements)               (declarative: whole schema)
                  │                                      │
        ╭─────────▼──────────────────────────────────────▼─────────╮
        │        CLI: migrate · diff · fmt · lint · status         │
        ╰─────────┬──────────────────────────────────────┬─────────╯
                  │                                      │
   ┌──────────────▼───── PLANNER (shared front-end) ─────▼──────────────┐
   │                                                                    │
   │  ╭─────────────────────╮            ╭─────────────────────────╮    │
   │  │ PARSE               │            │ INTROSPECT              │    │
   │  │ pkg/statement       │            │ pkg/schemadiff          │    │
   │  │ real PG grammar     │            │ read the live catalog;  │    │
   │  │ (go-pgquery) →      │            │ apply desired DDL to a  │    │
   │  │ typed per-operation │            │ scratch schema and      │    │
   │  │ descriptors         │            │ introspect that too     │    │
   │  ╰──────────┬──────────╯            ╰────────────┬────────────╯    │
   │             │                                    │ two schema      │
   │             │                                    ▼ models          │
   │             │                       ╭─────────────────────────╮    │
   │             │                       │ DIFF                    │    │
   │             │                       │ pkg/schemadiff          │    │
   │             │                       │ desired vs live →       │    │
   │             │                       │ ordered DDL operations  │    │
   │             │                       ╰────────────┬────────────╯    │
   │             │ operations                         │ operations      │
   │             ╰──────────────────┬─────────────────╯                 │
   │                                ▼                                   │
   │                   ╭─────────────────────────╮                      │
   │                   │ CLASSIFY                │                      │
   │                   │ pkg/planner             │                      │
   │                   │ per op: native-safe ·   │                      │
   │                   │ needs-rewrite · refuse; │                      │
   │                   │ emit the safer native   │                      │
   │                   │ sequence where one      │                      │
   │                   │ exists                  │                      │
   │                   ╰────────────┬────────────╯                      │
   │                                ▼                                   │
   │                   ╭─────────────────────────╮                      │
   │                   │ LINT                    │                      │
   │                   │ pkg/lint                │                      │
   │                   │ reject unsafe or        │                      │
   │                   │ unsupported operations  │                      │
   │                   │ before any write        │                      │
   │                   ╰────────────┬────────────╯                      │
   │                                │ Plan (ordered, classified steps)  │
   └────────────────────────────────┼───────────────────────────────────┘
                                    ▼
                            ╭───────────────╮
                            │    ROUTER     │      policy + cluster facts
                            ╰───────┬───────╯
           ╭────────────────────────┼───────────────┬────────────────╮
           ▼                        ▼               ▼                ▼
      native DDL             copy-and-swap    expand/contract    refuse with
      CONCURRENTLY           (later phase)    (reversible,       not-native-safe
      NOT VALID …                              later)            verdict
           ╰────────────────────────┴───────────────╯
                                    │ cross-cutting: connection mgmt,
                                    │ lock bounding, Aurora-aware throttling
                                    ▼
                         ╭─────────────────────╮
                         │  Aurora PostgreSQL  │
                         ╰─────────────────────╯
```

### The five front-end stages

| Stage | Package | Input → output | Why it is a separate stage |
| --- | --- | --- | --- |
| **Parse** | `pkg/statement` | SQL text → typed per-operation descriptors | One parse boundary using the real PostgreSQL grammar — no hand-parsing anywhere else; a parse failure is an error surfaced to the caller, never a guess |
| **Introspect** | `pkg/schemadiff` | live catalog (and desired DDL applied to a scratch schema) → schema models | The classifier and diff need *facts*, not text: column types, defaults, and constraint state come from PostgreSQL's own catalog, not a reimplementation of its semantics |
| **Diff** | `pkg/schemadiff` | desired model vs live model → ordered DDL operations | Declarative mode is a front-end that *produces statements*; its output enters the same pipeline as hand-written DDL, so both modes get identical safety treatment |
| **Classify** | `pkg/planner` | each operation + introspected facts → native-safe · needs-rewrite · refuse, with the safer native sequence where one exists | The safety decision lives in one pure, testable place — PostgreSQL's missing `ALGORITHM=`/`LOCK=` declaration ([design-principles.md](design-principles.md)) |
| **Lint** | `pkg/lint` | classified operations → pass or structured refusal | Policy-level rejection of unsafe or unsupported changes *before* any write — separate from the mechanical can-this-run-online judgment |

Parse, introspect, diff, and classify exist today (Phases 1–2); lint is the one stage not
yet built (see the [package map](#package-map) for per-package status).

The planner's verdicts are **requests, not permissions** — executors re-verify their own
preconditions. Which components are safety-critical (and the stricter rules inside that
boundary) is defined in [../SAFETY.md](../SAFETY.md).

## Package map

| Package | Role | Status |
| --- | --- | --- |
| `cmd/pg-sprite` | CLI entry point (Kong): `migrate` · `diff` · `fmt` · `lint` · `status` | `migrate` · `diff` · `fmt` · `status` exist; `lint` is a stub |
| `internal/cli` | Command tree and flag handling | `migrate` · `diff` · `fmt` · `status` exist; `lint` is a stub |
| `internal/testutil` | Test harness: containerized PostgreSQL, throwaway schemas | exists |
| `pkg/dbconn` | Pool with bounded session timeouts, retries, RDS/Aurora auto-TLS (embedded CA bundle), terminate-blockers; advisory-lock mutual exclusion lands here | exists |
| `pkg/statement` | `go-pgquery` (Wasm `libpg_query`) parse boundary, typed per-operation descriptors, and advisory rewrites (never hand-parse SQL); migration-time shadow DDL + fingerprints are derived by `pkg/schemadiff` via scratch-DB execute-and-introspect | exists |
| `pkg/preflight` | Precondition verification and refusals before any write | exists (Phase 1: table-size guard); grows through Phase 2 |
| `pkg/verdict` | Structured outcome contract (executed / refused + reason + safer idiom), rendering, exit codes | exists (Phase 1) |
| `pkg/schemadiff` | Execute-and-introspect desired state, introspect the live catalog, and produce an ordered declarative diff | exists |
| `pkg/planner` | Classify typed operations and emit safer native SQL | exists |
| `pkg/lint` | Policy-level rejection of unsafe or unsupported operations | planned |
| `pkg/plan` | Versioned machine-readable dry-run plan report — the one JSON contract both front doors emit and an orchestrator consumes | exists (Phase 2.5) |
| `pkg/router` | Route classified statements to native / copy-and-swap / refuse dispositions; copy-and-swap reports unavailable until that backend lands | exists (Phase 2.4) |
| `pkg/executor` | Bounded optimistic native attempt; the `Executor` contract (`Plan`/`Execute`/`Status`/`Abort`) lands in Phase 3 | bounded optimistic attempt exists |
| `pkg/table` | PK-range chunkers (single-column fast path, composite), dynamic time-based sizing | Phase 4 |
| `pkg/copier` | Parallel chunked copy into the shadow table (never overwrites) | Phase 4 |
| `pkg/checksum` | The mandatory correctness gate; continuous checker; repair primitive | Phase 5 |
| `pkg/decode` | Logical-decoding change capture, LSN accounting, slot lifecycle | Phase 6, 8 |
| `pkg/applier` | Change apply onto the shadow (always wins), buffer/dedup, flush scheduling | Phase 6 |
| `pkg/migration` | Orchestrator: lifecycle, cutover swap + fidelity gate, checkpoint/resume | Phase 7–8 |
| `pkg/checkpoint` | Durable single-row resume state | Phase 8 |
| `pkg/throttler` | Aurora reader-lag / slot-lag / WAL throttling | Phase 8 |

## The copy-and-swap lifecycle

In a later phase, when the in-house heavy path is available and the router picks it:

```
 create shadow table  ─▶  start change capture   ─▶  bulk-copy existing rows
 with the new schema      (logical decoding,         in parallel chunks
                           off the WAL)                     │
                                                            ▼
        cut over  ◀──  CHECKSUM GATE  ◀──  drain the captured-change backlog
   (brief ACCESS           (must prove          onto the shadow
    EXCLUSIVE swap,         shadow == source
    bounded + retried)      before cutover)
```

The interleaving rules that make the copier and applier converge — and the invariant IDs every
component must uphold — are registered in [invariants.md](invariants.md); the trust boundary
and domain-type design are in [tcb-model.md](tcb-model.md).

## Where to read more

- [high-level-design.md](high-level-design.md) — the conceptual design: why one planner and
  many executors, when each pattern is chosen, advisory mode.
- [low-level-design.md](low-level-design.md) — interfaces, lifecycle internals, coverage
  matrix, decisions remaining for later phases.
- [design-principles.md](design-principles.md) — the principles everything traces back to.
- [invariants.md](invariants.md) — the testable MUST-statements, with per-phase test
  obligations.
- [tcb-model.md](tcb-model.md) — the safety-critical-core partition in depth.
- [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md) — per-operation lock /
  rewrite behaviour, the classifier's ground truth.
- [../AGENTS.md](../AGENTS.md) and [../SAFETY.md](../SAFETY.md) — how to work in this repo.
