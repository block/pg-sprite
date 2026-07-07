# Trusted Computing Base

pg-sprite rewrites production tables — a bug in the wrong place is silent data corruption or an
app-wide outage. The codebase is therefore partitioned into a small **trusted computing base
(TCB)** that enforces the engine's invariants, and an **untrusted periphery** where a bug can
only produce a wrong message, a wasted copy, or a missed optimization.

**Membership test:** can a bug here corrupt data, lose writes, swap in a wrong table, strand a
replication slot, or take the application down? If yes → TCB. If no → periphery.

The invariant registry (invariant IDs referenced below) and the full TCB design currently live
in the research corpus (`research/migrations-related/aurora-postgresql/online-schema-change-engine/`,
docs `17-invariants.md` and `18-tcb-model.md`) and migrate here with the open-sourcing work.

## The partition

| Package | TCB? | Status | Invariants enforced |
| --- | --- | --- | --- |
| `pkg/dbconn` — pool defaults, advisory lock, terminate-blockers, retries | ✅ TCB | exists (Phase 0) | LK-1, LK-2 primitives |
| `pkg/preflight` — precondition verifier, refusals | ✅ TCB | planned (Phase 1–2) | ST-6, RF-1..RF-5 |
| `pkg/checksum` — chunk verifier, continuous checker, repair | ✅ TCB | planned (Phase 5) | CO-1, CO-2, CO-3 |
| `pkg/copier` — shadow-table chunked copy | ✅ TCB | planned (Phase 4) | CO-4, LK-3 |
| `pkg/applier` — change apply, buffer, flush scheduling | ✅ TCB | planned (Phase 6) | CO-4, CO-5, CO-6, LK-3 |
| `pkg/decode` — logical decoding, LSN/position accounting | ✅ TCB | planned (Phase 6) | ST-4, CO-4 |
| `pkg/checkpoint` — durable resume state | ✅ TCB | planned (Phase 8) | ST-1, ST-2 |
| slot lifecycle (in `pkg/decode`) — create, reap, lag ceiling | ✅ TCB | planned (Phase 8) | ST-3 |
| `pkg/migration` — orchestrator, **cutover swap + fidelity gate** | ✅ TCB | planned (Phase 7) | LK-2, LK-4, ST-5 |
| `pkg/statement`, `pkg/planner`, `pkg/schemadiff`, `pkg/lint` — classify/diff/route | ❌ periphery¹ | planned (Phase 1–2) | (CO-7 holds at the parse boundary) |
| `internal/cli` — CLI, flags, help, prompts | ❌ periphery | exists (stubs) | — |
| status / progress / advisory rendering, metrics | ❌ periphery | planned | — |
| SchemaBot adapter | ❌ periphery | planned (Phase 11) | OC-* hold *at* the boundary |
| `internal/testutil` | ❌ test-only | exists | — |

¹ **The planner is deliberately outside.** Its verdicts are *requests*, not permissions: a wrong
"native-safe" verdict is capped by the executor's own `lock_timeout` bound; a wrong "copy"
verdict produces a wasteful but *correct* migration (the checksum still gates). The TCB
executors re-verify their own preconditions and never trust that the planner checked.

## Rules inside the TCB

The short version — the full rules live in the research doc 18:

- **Never trust callers.** Every dangerous operation re-verifies its preconditions, whoever the
  requester is (CLI, planner, SchemaBot). Untrusted code may request; the TCB enforces.
- **Domain types make illegal states unrepresentable.** Validating passages return proof types
  with package-private constructors (`statement.Classified`, `PreflightedTable`,
  `VerifiedShadow`, `CleanWatermark`, `TableLock`); dangerous APIs accept only proof types —
  e.g. the cutover swap accepts only a `VerifiedShadow`.
- **Put a limit on everything.** Every loop bounded, every queue bounded, every retry counted,
  every wait deadlined. An unbounded anything in a TCB package is a review-blocking defect.
- **Assert the positive and the negative space; pair assertions across boundaries.** Invariant
  violations use a distinct error class (`ErrInvariantViolation`) naming the invariant ID, and
  always abort fail-closed — never a warning, never retried.
- **Locality of behavior.** The enforcement point of an invariant carries a `// INV: <id>`
  comment so a reviewer or agent can grep the ID and see the whole enforcement in one screen.
- **Dependencies inside the TCB become part of the TCB.** Current TCB dependency list: `pgx/v5`,
  `pglogrepl`, stdlib. Adding one requires a recorded decision (see the rubric in doc 18 —
  copy small things, take pinned dependencies only for load-bearing expertise). pg-sprite
  **never imports `block/spirit` as a module**: we port ideas with citations, not code.
- **Priorities when trade-offs are hard:** Correctness → Readability → Ease of use →
  Performance.

## Working here with AI assistance

- **Inside the TCB: less AI, more steering.** Spec first (the design docs + invariant IDs),
  test-first with the invariant's named test obligation, small diffs, careful review.
- **Outside the TCB: more AI, less steering.** Iterate at inference speed; the boundary means a
  bug in the periphery cannot corrupt data.
