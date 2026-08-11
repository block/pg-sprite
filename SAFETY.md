# The safety-critical core

pg-sprite rewrites production tables — a bug in the wrong place is silent data corruption or an
app-wide outage. The codebase is therefore partitioned into a small **safety-critical core**
that enforces the engine's invariants, and a **periphery** where a bug can only produce a wrong
message, a wasted copy, or a missed optimization. (The design docs call this partition the
engine's *trusted computing base*; this file is the repo-level map of it.)

**Membership test:** can a bug here corrupt data, lose writes, swap in a wrong table, strand a
replication slot, or take the application down? If yes → core. If no → periphery.

The invariant registry (invariant IDs referenced below) lives in
[docs/invariants.md](docs/invariants.md); the full partition design lives in
[docs/tcb-model.md](docs/tcb-model.md).

## The partition

| Package | Core? | Status | Invariants enforced |
| --- | --- | --- | --- |
| `pkg/dbconn` — pool defaults, terminate-blockers, retries, RDS TLS; advisory table lock planned | ✅ core | exists; advisory table lock planned | LK-2 primitives; LK-1 planned |
| `pkg/preflight` — precondition verifier, refusals | ✅ core | exists (Phase 1: table-size guard); grows through Phase 2 | ST-6, RF-1..RF-5 |
| `pkg/executor` — bounded optimistic attempt; native executor later | ✅ core | exists (Phase 1: attempt-under-budget); Executor contract at Phase 2–3 | LK-2 (attempt bound) |
| `pkg/checksum` — chunk verifier, continuous checker, repair | ✅ core | planned (Phase 5) | CO-1, CO-2, CO-3 |
| `pkg/copier` — shadow-table chunked copy | ✅ core | planned (Phase 4) | CO-4, LK-3 |
| `pkg/applier` — change apply, buffer, flush scheduling | ✅ core | planned (Phase 6) | CO-4, CO-5, CO-6, LK-3 |
| `pkg/decode` — logical decoding, LSN/position accounting | ✅ core | planned (Phase 6) | ST-4, CO-4 |
| `pkg/checkpoint` — durable resume state | ✅ core | planned (Phase 8) | ST-1, ST-2 |
| slot lifecycle (in `pkg/decode`) — create, reap, lag ceiling | ✅ core | planned (Phase 8) | ST-3 |
| `pkg/schemachange` — orchestrator, **cutover swap + fidelity gate** | ✅ core | planned (Phase 7) | LK-2, LK-4, ST-5 |
| `pkg/statement`, `pkg/planner`, `pkg/schemadiff`, `pkg/router`, `pkg/plan`, `pkg/lint`, `pkg/suggest` — classify/diff/route/report | ❌ periphery¹ | `pkg/statement` (parse boundary), `pkg/schemadiff` (introspect/diff via scratch execute-and-introspect), `pkg/planner` (classifier), `pkg/router` (backend assignment + availability policy), `pkg/plan` (versioned dry-run plan report), `pkg/lint` (offline typed findings), and `pkg/suggest` (advisory rewrites with typed caveats) exist (Phases 2.1–2.5) | (CO-7 holds at the parse boundary) |
| `pkg/verdict` — structured outcome contract, rendering, exit codes | ❌ periphery | exists (Phase 1) | — |
| `pkg/diffplan` — desired schema → routed convergence plan, the declarative front door as a library (the CLI `diff` and embedding orchestrators share it) | ❌ periphery | exists | — |
| `internal/cli` — CLI, flags, help, prompts | ❌ periphery | `migrate`, `status`, `diff`, `fmt`, `lint`, and `suggest` exist | — |
| status / progress / advisory rendering, metrics | ❌ periphery | planned | — |
| orchestrator adapter | ❌ periphery | planned (Phase 11) | OC-* hold *at* the boundary |
| `internal/testutil` | ❌ test-only | exists | — |

¹ **The planner is deliberately outside the core.** Its verdicts are *requests*, not
permissions: a wrong "native-safe" verdict is capped by the executor's own `lock_timeout` bound.
Today a "copy" route reports unavailable; once copy-and-swap exists, a wrong "copy" verdict will
produce a wasteful but *correct* schema change because the checksum will still gate it.
The core executors re-verify their own preconditions and never trust that the planner checked.

## Rules inside the core

The short version — the full rules live in [docs/tcb-model.md](docs/tcb-model.md):

- **Never trust callers.** Every dangerous operation re-verifies its preconditions, whoever the
  requester is (CLI, planner, orchestrator). The periphery may request; the core enforces.
- **Domain types make illegal states unrepresentable.** Validating passages return proof types
  with package-private constructors (today `preflight.PreflightedTable`; later phases add
  `VerifiedShadow`, `CleanWatermark`, and `TableLock`); dangerous APIs accept only proof types —
  e.g. the planned cutover swap will accept only a `VerifiedShadow`.
- **Put a limit on everything.** Every loop bounded, every queue bounded, every retry counted,
  every wait deadlined. An unbounded anything in a core package is a review-blocking defect.
- **Assert the positive and the negative space; pair assertions across boundaries.** Invariant
  violations will use a distinct error class (`ErrInvariantViolation`) naming the invariant ID
  once the executor phases land, and always abort fail-closed — never a warning, never retried.
- **Locality of behavior.** The enforcement point of an invariant carries a `// INV: <id>`
  comment so a reviewer or agent can grep the ID and see the whole enforcement in one screen.
- **Dependencies inside the core become part of the core.** Current core dependency list:
  `pgx/v5`, stdlib. The future decode path will add `pglogrepl`. Adding one requires a recorded decision (see the rubric in
  [docs/tcb-model.md](docs/tcb-model.md) — copy small things, take pinned dependencies only
  for load-bearing expertise).
  Recorded decision: the AWS SDK (`aws-sdk-go-v2`) is a test-harness-only dependency, confined
  behind the `ministack` build tag in `internal/testutil` — it never appears in the core, in
  `cmd/pg-sprite`, or in any ordinary build; a plain `go build ./...` / `go test ./...` never
  compiles it.
  pg-sprite **never imports `block/spirit` as a module**: we port ideas with citations, not
  code.
- **Priorities when trade-offs are hard:** Correctness → Readability → Ease of use →
  Performance.

## Working here with AI assistance

- **Inside the core: less AI, more steering.** Spec first (the design docs + invariant IDs),
  test-first with the invariant's named test obligation, small diffs, careful review.
- **Outside the core: more AI, less steering.** Iterate at inference speed; the boundary means
  a bug in the periphery cannot corrupt data.
