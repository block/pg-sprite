# Architecture

The one-screen map of the codebase: what the layers are, where each responsibility lives, and
what exists today versus what each build phase adds. For the design rationale behind this shape
start with [high-level-design.md](high-level-design.md); for interfaces and lifecycle internals
see [low-level-design.md](low-level-design.md).

## The three layers

pg-sprite is a decoupled **planner → router → executor** engine. The planner decides *what*
changes, the router decides *which strategy*, interchangeable executors decide *how*:

```
        user: --alter "..."  OR  --desired schema.sql
                         │
              ╭──────────▼───────────╮
              │ CLI: migrate · diff ·│
              │ fmt · lint · status  │
              ╰──────────┬───────────╯
                         ▼
                 ╭───────────────╮      shared front-end:
                 │   PLANNER     │      parse · introspect ·
                 │  (classify)   │      diff · classify · lint
                 ╰───────┬───────╯
                         ▼
                 ╭───────────────╮
                 │    ROUTER     │      policy + cluster facts
                 ╰───────┬───────╯
        ╭────────────────┼────────────────┬───────────────╮
        ▼                ▼                ▼               ▼
   native DDL      copy-and-swap    expand/contract     refuse with
   CONCURRENTLY    (later phase)    (reversible,        not-native-safe
   NOT VALID …                       later)              verdict
        ╰────────────────┴────────────────╯
                         │ cross-cutting: connection mgmt,
                         │ lock bounding, Aurora-aware throttling
                         ▼
              ╭─────────────────────╮
              │  Aurora PostgreSQL  │
              ╰─────────────────────╯
```

The planner's verdicts are **requests, not permissions** — executors re-verify their own
preconditions. Which components are safety-critical (and the stricter rules inside that
boundary) is defined in [../SAFETY.md](../SAFETY.md).

## Package map

| Package | Role | Status |
| --- | --- | --- |
| `cmd/pg-sprite` | CLI entry point (Kong): `migrate` · `diff` · `fmt` · `lint` · `status` | exists (stubs) |
| `internal/cli` | Command tree and flag handling | exists (stubs) |
| `internal/testutil` | Test harness: containerized PostgreSQL, throwaway schemas | exists |
| `pkg/dbconn` | Pool with bounded session timeouts, retries, RDS/Aurora auto-TLS (embedded CA bundle), terminate-blockers; advisory-lock mutual exclusion lands here | exists |
| `pkg/statement` | `pg_query_go` parsing + classification (never hand-parse SQL) | Phase 1–2 |
| `pkg/preflight` | Precondition verification and refusals before any write | Phase 1–2 |
| `pkg/planner` / `pkg/schemadiff` / `pkg/lint` | Shared front-end: introspect, declarative diff (may wrap [stripe/pg-schema-diff](https://github.com/stripe/pg-schema-diff) — see the low-level design's open decisions), classify, lint | Phase 2 |
| `pkg/executor` | The `Executor` contract (`Plan`/`Execute`/`Status`/`Abort`) + native executor | Phase 2–3 |
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
  matrix, open decisions.
- [design-principles.md](design-principles.md) — the principles everything traces back to.
- [invariants.md](invariants.md) — the testable MUST-statements, with per-phase test
  obligations.
- [tcb-model.md](tcb-model.md) — the safety-critical-core partition in depth.
- [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md) — per-operation lock /
  rewrite behaviour, the classifier's ground truth.
- [../AGENTS.md](../AGENTS.md) and [../SAFETY.md](../SAFETY.md) — how to work in this repo.
