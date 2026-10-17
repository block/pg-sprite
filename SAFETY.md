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
| `pkg/dbconn` — pool defaults, terminate-blockers, retries, RDS TLS, advisory table lock, replication connection | ✅ core | table-lock primitive exists; `ConnectReplication` dials one replication-mode connection with the pool's TLS, timeouts, and `BeforeConnect` hook; wiring into executing modes planned | LK-1 primitive; LK-2 primitives; CO-9 (session hook and `LocalSearchPath`) |
| `pkg/preflight` — precondition verifier, refusals | ✅ core | exists; copy-and-swap target proof (shape and dependents) and cluster/volume environment check gate the copy-and-swap route before any shadow object is created | ST-6, RF-1..RF-5, CO-9 (every proof read is `pg_catalog`-qualified) |
| `pkg/executor` — bounded optimistic attempt; native concurrent index build with invalid-index recovery; native sequence executor for the safer idioms | ✅ core | exists (Phase 1: attempt-under-budget; Phase 3.1: concurrent index build; Phase 3.2: sequence executor) | LK-2 (attempt bound + the CONCURRENTLY wait-policy exception), CO-9 (qualified proof reads), ST-9 (create owner verified, never repaired) |
| `pkg/checksum` — chunk verifier, divergence policy, repair, continuous checker | ✅ core | the chunk `Verifier` exists (one read-only `REPEATABLE READ` transaction per chunk in which the chunk is cut and both digests read, so all three share a snapshot and the cut is bounded, every column cast to the shadow's type on both sides, the same guard as the copier — owner role, the copy statement's `search_path` (the catalog alone, or the shadow build's path when the proof carries conversions), `ACCESS SHARE` on both relations, lock confirmation, relation-OID check — and a `Report` of mismatched chunks up to the landed watermark); `Check` runs the pass under an explicit `DivergencePolicy` (zero value refused; `ParseDivergencePolicy` for configuration, which names `abort` or `repair` and refuses `recopy`), aborts with the shadow untouched or recopies every differing chunk with the copier's chunk statement in one guarded transaction — every chunk's rows deleted before any are put back, so a unique value the source moved between two chunks lands — then rereads each chunk under `repair`, or commits without the reread under `recopy` (the reconcile mode's policy, for a source written while the pass runs), returning the committed repairs with any refusal, and mints `CleanWatermark` and `VerifiedShadow` (private constructors) only from a pass that found nothing and repaired nothing; `CheckLive` is the pass for a source written while the catch-up applies the stream — a differing chunk is reread once the catch-up has confirmed past the insert position it was read at and judged against the earlier read (settled, hot and reread again up to a bound, or a confirmed divergence), an unsettled chunk is an `UnresolvedError` that mints nothing, a live repair holds the catch-up's flushes off the shadow in Go for as long as it recopies (`LiveOptions.Flushes`, a `FlushGate` the applier's `Catchup` satisfies, required under `repair`) with `SHARE MODE` on the shadow against every other writer, resumes them once the recopy has committed, and settles each repaired chunk the same way, and `recopy` is refused; the pass is the progress tracker's `WorkSource` while it runs (chunks compared, rows hashed, chunks mismatched, chunks repaired, chunks reread, all from memory); the continuous checker that runs `CheckLive` on a loop is planned | CO-1, CO-2, CO-3, CO-9, LK-1, LK-2 |
| `pkg/copier` — shadow-table chunked copy | ✅ core | contract types and the keyset `Chunker` exist (row-count chunks over the proven key, first chunk open below and last open above, a cut frontier for the applier's discard rule, time-targeted sizing) and the parallel `Copier` (one bounded never-overwriting insert per chunk under the table lock session — each column the proof's conversions name selected as its `USING` expression, every other bare — each in a guarded transaction — owner role, the copy statement's `search_path` (the catalog alone, or the shadow build's path when the copy carries conversions), `ACCESS SHARE` on both relations, lock confirmation, relation-OID check — frontier-ordered in-flight registry, a resume that first clears the shadow above the watermark, `Position.Classify` for the applier, and the copy step's `progress.WorkSource` — rows from committed chunks, the source's catalog row count, both tables' measured sizes; every cut and measurement in a bounded read-only transaction with the catalog alone on its `search_path`) exist; every refusal is a `*RefusalError` carrying a typed `RefusalCause` from the closed set in [docs/refusal-classes.md](docs/refusal-classes.md#copier-refusals-keyed-on-copierrefusalcause) | CO-4 (chunk coverage, copy SQL shape, in-flight registry), CO-9, LK-1, LK-2 (every statement on the caller's pool bounds itself), LK-3 |
| `pkg/internal/chunksql` — the chunk insert statement the copier and the verifier's repair both run, and the `search_path` it runs under | ✅ core | exists; unexported from the module so no caller can run the statement outside the guard both packages wrap around it | CO-4 (copy SQL shape) |
| `pkg/refusal` — the routing classes the engine's typed refusal causes take (`Class`: `capability-boundary`, `by-design`, `environmental`, `invariant-violation`; `no-online-safety-problem` is the verdict's own, for a statement that never reached the engine), so `copier.RefusalCause.Class()`, `schemachange.RefusalCause.Class()`, `preflight.PartitionRefusalCause.Class()`, `preflight.CopySwapRefusalCause.Class()`, and `executor.CreateShapeCause.Class()` answer in one vocabulary `verdict.Class` declares its tokens from, and the core never imports the verdict renderer | ✅ core | exists; stdlib only; the classes are decided in [docs/refusal-classes.md](docs/refusal-classes.md#the-decided-vocabulary) and the doc guards in `pkg/copier`, `pkg/schemachange`, `pkg/preflight`, `pkg/executor`, `pkg/refusal`, and `pkg/verdict` hold the code to it, reading the cause tables through the shared reader in `internal/testutil`; `pkg/plan`'s guard holds the registry's class for each create-shape and partition cause to that cause's `Class()` | — |
| `pkg/applier` — change apply, buffer, flush scheduling | ✅ core | `Buffer` (merge, drain against `copier.Position`, `Requeue` of a refused batch) and `Flusher` (one guarded read-write transaction per batch under the table lock session — owner role, catalog-only `search_path`, `ACCESS SHARE` on both relations, lock confirmation, relation-OID check — completing moved marker-bearing images first and holding, until the stream passes the check, an unmoved marker-bearing image whose shadow row a presence check finds absent (refused on a second sight), then column-wise delete / upsert / present-columns UPDATE, with the delete-all-then-insert-all fallback on a unique violation and `ErrBatchDeferred` when the fallback is refused too) and `Catchup` (the stream consumer loop — gather, release, drain against the copier's position read after the last add, flush, requeue or hold, confirm the lesser of delivered and oldest pending, measure lag against `pg_current_wal_lsn()` on the pool, end on a refusal no chunk can resolve or on a lost lock, hold its flushes under `Pause` — returned once no flush is in flight, with every cycle until the resume still reading, buffering, and confirming the stream but neither draining nor flushing — and end each cycle by inspecting the stream's slot on the pool and judging it against `CatchupOptions.SlotLagCeiling` — a vanished or `lost` slot is `decode.SlotLostError`, over the ceiling is `decode.SlotLagExceededError`, neither switchable off, the ceiling never below one byte — and the catch-up step's `progress.WorkSource`) built | CO-4, CO-5, CO-6, CO-8, LK-1, LK-2, LK-3, ST-3, ST-4 |
| `pkg/decode` — logical decoding, LSN/position accounting, per-column presence | ✅ core | exists (`OpenStream` decodes the slot with pgoutput on a dedicated replication connection proven to be a session of the pool's cluster on the target's database, into one `ChangeEvent` per committed row change: a text value or NULL is a present column, the unchanged-TOAST marker is an absent one, and an UPDATE that moved the key carries `OldKey`; a relation that is not the target, a tuple that does not line up with it, or a change outside a transaction fails closed, a changed table shape is `ErrSourceShapeChanged`, and TRUNCATE is `ErrUnsupportedChange`; a warning from the server in copy-both mode — the walsender saying it will withhold changes — stops the stream fail-closed, and the replication connection asks for warnings itself, with `SET client_min_messages = warning` on the open session, so no role, database, or server setting, login event trigger, URL parameter, or connection pooler can keep them from it; `Delivered` moves on a commit or on a keepalive between transactions and never names a position a transaction not yet yielded in full committed at or below — positions order transactions by commit, so a change's own LSN can lie below one, and each `ChangeEvent` carries the `Delivered` it arrived with as the position confirmable while it is unapplied; `Confirm` is the only standby-status report that carries a position, refuses a regression or a position beyond `Delivered`, and records nothing the server was not told; keepalive replies carry only what the caller confirmed, and a wait that runs out in `Next` sends the same report, so the stream is never silent for longer than the caller's wait; `ServerWALEnd` is the walsender's send position from its last keepalive, never below `Delivered` — a lower bound on lag, not the lag itself; the server ending replication with the slot intact is `ErrStreamEnded`, not a violation) | ST-4, CO-4, CO-8 |
| `pkg/checkpoint` — durable resume state | ✅ core | contract and persistence exist: `Store` over a `pkg/dbconn` pool creates `pgsprite.pgsprite_checkpoint` on first use under the engine's advisory key, creating only what is absent, refusing, typed, a schema or table another role owns, adding the columns a table an older engine created lacks, and refusing, with no DDL, a table a newer engine shaped (`Ensure`; a read or write that reaches an older table before `Ensure` ran is the typed `ErrTableBehind`), writes one row per target — the shadow's JSON proof included, write-once: a save without a proof keeps the row's, a save with a different proof is refused and changes nothing — in one guarded upsert — under the target's table lock session, confirmed from the write's own transaction — that refuses, typed, to overwrite a row carrying another statement's fingerprints or another row format (`Save`), reads it back telling a matching row, no row (`ErrNotFound`), a missing table (`ErrTableMissing`), an `IncompatibleError`, and a retried transient read error apart (`Load`), reads it guarded on the row format alone before the run knows its statement (`Inspect`), and removes it as the one explicit fresh start, matching the row identity the caller was shown (`Delete`), and reaps the engine's orphan slots of its own database (`ReapOrphanSlots`: lists the logical, engine-prefixed slots of `current_database()`, then reads the non-terminal rows that name them, and re-reads each orphan's row right before its drop; a slot no live row names is dropped through `decode.DropIdleSlot`, publication and all, which refuses a held slot rather than wait for its holder; a slot a live row names, a slot a process holds, and a name wearing the prefix without the engine's shape are reported and left; a database without the checkpoint table is refused with `ErrTableMissing`); `PhaseReconciling`, between `catching_up` and `verifying`, is the marker the ST-4 reconcile writes before it touches the slot, so a reconcile that dies restarts as one; `WithSlotLock` is the engine's advisory lock on a slot's name, transaction-scoped on a connection dialled outside the store's pool and let go however the callback ends, held by a run while it makes the slot and commits the row that names it and by the reaper from its last read of the row to the drop, so the two never interleave; the resume state machine that drives the phases end to end is `pkg/schemachange.Runner` | ST-1, ST-2, ST-3, ST-4, ST-10, LK-1 |
| slot lifecycle (in `pkg/decode`) — create, drop, inspect, reap, lag ceiling | ✅ core | create, drop, and inspect exist (`CreateSlot` makes the single-table publication first and then the logical slot, named as preflight derived, on a dedicated replication connection from `pkg/dbconn` proven to be on the target's database, with an exported snapshot and the consistent point; `DropSlot` waits out a walsender holding the slot under the caller's context alone, never reports a cut-off wait as a drop, refuses a slot of the name another database owns, and is idempotent; `DropIdleSlot` is the same drop without the wait, refusing a held slot with `ErrSlotActive` and leaving slot and publication as found; `InspectSlot` reads `wal_status`, retained WAL — unknown, never zero, on a server in recovery — the holder, and the server's account of a lost slot (`invalidation_reason`, `conflicting`, `inactive_since`) where the server version carries it, so `SlotStatus.LostState` renders a `SlotLostError` that names the cause or says none was reported; `SlotStatus.WithinLagCeiling` judges a slot against a ceiling in bytes, lost before any measure, unknown never read as nothing retained, and refuses a ceiling below one byte; `DefaultSlotLagCeiling` is one GiB); the reaper is `pkg/checkpoint` `Store.ReapOrphanSlots`, the ceiling is enforced by `pkg/applier` `Catchup` every cycle, and the ST-4 reconcile that replaces a lost slot is `pkg/schemachange` `Reconciler` | ST-3, ST-4 |
| `pkg/schemachange` — shadow builder, orchestrator, **cutover swap + fidelity gate** | ✅ core | shadow lifecycle exists (`BuildShadow` → `BuiltShadow`, `DropShadow`, `InspectShadow`, `SourceOfDerivedName`), the cutover fidelity gate exists (`GateCutover` → `CutoverReady`), and the swap exists (`Cutover` → `SwappedTable`, `DropOldTable`), and the ST-4 resume read and reconcile mode exist (`InspectResume` → `Resume` answering clean or reconcile, refusing a terminal, cutover, or mismatched row before any slot is touched and a slot of the derived name that is foreign or held; `Reconciler.Reconcile` → `Reconciled`, whole under the lock session's `Bind` context: the same row checks, relations confirmed against the build, row to `reconciling` naming the slot it is about to make, drop whatever bears the derived slot name, fresh slot, one `checksum` pass under `recopy` at or below the watermark when anything landed, row back to `copying` or `catching_up` with the new slot's consistent point as the applied position — the shadow kept and the watermark never moved), every operation taking the `*dbconn.TableLockSession` it runs under; the orchestrator exists for both kinds of target (`Runner.Run`: acquires the table lock itself, reads the row through `Store.Inspect` and dispatches on it — no row builds the shadow, a non-terminal row before its cutover is held against the shadow the catalog re-derives, proof included, a `cutover` row is answered by `InspectSwapped`, a terminal row or the other kind of run's row is refused — then copies, saving the landed watermark on an interval, verifies through the complete watermark under `DivergenceAbort`, gates, writes `cutover`, swaps, drops the old table unless `KeepOld`, and writes `done`; a quiesced target is fenced for the run; a decoding target gets its slot with the row that names it under `WithSlotLock`, its stream applied to the shadow by a capture beside the copy, a catch-up to a marker past every change committed so far, `CheckLive` as the pass, the capture ended and its confirmed position saved before `cutover`, a drain under the swap's lock, on a connection of its own outside the pool, that reopens the stream from that position and flushes it through the marker it writes, and the slot dropped once the swap has committed; a lost slot is reconciled through `Reconciler.Reconcile` right before the stream opens and reported on `Result.Reconciled`; `PhaseFailed` is never written, so a stopped run resumes from its last save) | LK-1, LK-2, ST-5, ST-6, ST-7 (shadow build, drop, inspect); CO-1, ST-5, ST-6 (cutover gate); LK-1, LK-2, LK-4, RF-2, ST-5, ST-6 (swap and old-table drop); ST-4, CO-3 (resume read and reconcile); LK-4, ST-2, ST-3, ST-4, CO-1, CO-4, CO-6, CO-8 (runner); CO-9 (every catalog read is `pg_catalog`-qualified, as is the write fence function's body, which runs in the writer's session) |
| `pkg/statement`, `pkg/planner`, `pkg/schemadiff`, `pkg/router`, `pkg/plan`, `pkg/lint`, `pkg/suggest` — classify/diff/route/report | ❌ periphery¹ | `pkg/statement` (parse boundary), `pkg/schemadiff` (introspect/diff via scratch execute-and-introspect), `pkg/planner` (classifier), `pkg/router` (backend assignment + availability policy), `pkg/plan` (versioned dry-run plan report), `pkg/lint` (offline typed findings), and `pkg/suggest` (advisory rewrites with typed caveats) exist (Phases 2.1–2.5) | (CO-7 holds at the parse boundary) |
| `pkg/verdict` — structured outcome contract, rendering, exit codes | ❌ periphery | exists (Phase 1) | — |
| `pkg/capabilities` — embedded, validated support matrix and Markdown rendering | ❌ periphery | exists | — |
| `pkg/diffplan` — desired schema → routed convergence plan, the declarative front door as a library (the CLI `diff` and embedding orchestrators share it) | ❌ periphery | exists | — |
| `pkg/migrate` — one gated statement → resolve, classify, route, execute → one verdict; the imperative front door as a library (the CLI `migrate` and embedding orchestrators share it), plus the desired-state execution loop (`RunDesired`: derive the convergence plan, admit it as a whole, run each planned statement back through the same pipeline) | ❌ periphery² | exists | CO-9 (the target resolve and the accept-blocking table lookup are `pg_catalog`-qualified) |
| `internal/cli` — CLI, flags, help, prompts | ❌ periphery | `migrate`, `pull`, `diff`, `fmt`, `lint`, `suggest`, `capabilities`, and `status` exist | — |
| `pkg/progress` — strategy-wide progress snapshots; the executors' observation seam (core imports it, so its locking discipline is core-critical); the `WorkSource` seam for engine-measured steps such as the copy and the checksum pass | ✅ core | native progress and the work-source seam exist; the copier and the checksum verifier fill it | — |
| orchestrator adapter | ❌ periphery | planned (Phase 11) | OC-* hold *at* the boundary |
| `internal/testutil` | ❌ test-only | exists | — |

¹ **The planner is deliberately outside the core.** Its verdicts are *requests*, not
permissions: a wrong "native-safe" verdict is capped by the executor's own `lock_timeout` bound.
A wrong "copy" verdict produces a wasteful but *correct* schema change because the checksum
still gates the cutover; a "copy" route for an operation the engine does not carry reports
unavailable and nothing runs.
The core executors re-verify their own preconditions and never trust that the planner checked.

² **The imperative front door is periphery for the same reason.** `pkg/migrate` sequences the
pipeline — gate, resolve, preflight, execute — but every dangerous step it requests is enforced
by the core packages it calls: the executors re-verify admission and run under their own bounded
budgets, and preflight's proof types gate what may execute. A wrong sequencing decision in
`pkg/migrate` yields a refusal or a bounded failed attempt, never an unbounded lock. The
desired-state loop inherits that argument for every *execution-time* property: it executes
nothing itself — every planned statement goes back through `Run`, so each one is
re-introspected, re-classified, re-routed, and re-preflighted at execution time, and a plan
the loop wrongly admits still cannot make the core exceed a lock budget or skip a preflight.
**One admission check has no core backstop: the destructive guard.** The core has no concept
of destructiveness — `pkg/executor` and `pkg/preflight` never check it — so refusing a
destructive desired-state plan rests entirely on `RunDesired`'s admission gate and on the
classifier's `Destructive` derivation in `pkg/planner`, and its failure mode is data loss (a
falsely-admitted `DROP COLUMN` commits), not a refusal or a bounded failed attempt. Those two
sites are the exception to the periphery posture: treat `destructiveOp` and the desired-state
admission gate with the core's review bar — spec-first, test-first, small diffs — even though
their packages stay periphery for everything else they do.

## Rules inside the core

The short version — the full rules live in [docs/tcb-model.md](docs/tcb-model.md):

- **Never trust callers.** Every dangerous operation re-verifies its preconditions, whoever the
  requester is (CLI, planner, orchestrator). The periphery may request; the core enforces.
- **Domain types make illegal states unrepresentable.** Validating passages return proof types
  with package-private constructors (`statement.Statement`, `statement.DesiredSchema`,
  `preflight.PreflightedTable`, `preflight.AbsentTarget`, `preflight.CreationRole`,
  `preflight.PrivilegedRole`, `preflight.CopySwapShape`, `preflight.CopySwapTarget`,
  `dbconn.TableLock`, `checksum.VerifiedShadow`, and `checksum.CleanWatermark`); dangerous
  APIs accept only proof types —
  e.g. the planned cutover swap will accept only a `VerifiedShadow`.
- `statement.DesiredWithRowSecurity` proves declaration syntax, not execution safety. It stays distinct from
  `DesiredSchema`. `executor.PreviewRowSecurity`, `executor.ExecuteRowSecurity`, and
  `executor.ExecuteReviewedRowSecurity` consume it. All take a bounded exclusive table lock,
  check table equality, and derive SQL through scratch introspection. Preview rolls back
  before target DDL; execution verifies convergence before commit. Reviewed execution also
  checks the exact ordered SQL under the lock (RS-5).
- **Put a limit on everything.** Every loop bounded, every queue bounded, every retry counted,
  every wait deadlined. An unbounded anything in a core package is a review-blocking defect.
- **Assert the positive and the negative space; pair assertions across boundaries.** Invariant
  violations will use a distinct error class (`ErrInvariantViolation`) naming the invariant ID
  once the executor phases land, and always abort fail-closed — never a warning, never retried.
- **Locality of behavior.** The enforcement point of an invariant carries a `// INV: <id>`
  comment so a reviewer or agent can grep the ID and see the whole enforcement in one screen.
- **Dependencies inside the core become part of the core.** Current core dependency list:
  `pgx/v5`, the parse boundary (`pkg/statement` → `wasilibs/go-pgquery`, the real PostgreSQL
  grammar — the native executor re-verifies statement shape itself rather than trusting the
  caller's classification; the grammar is load-bearing expertise, not copyable mechanics),
  `pkg/progress` (the executors' progress-observation seam: they write state into a
  caller-owned tracker whose mutators take only a memory lock, and its polling reads ride
  the reserved verdict session behind a separate one-slot poll gate, which `Progress` and
  `CancelBuild` wait on only as long as their caller's context lasts — the executor's own state
  updates never wait for a database read, but the handoffs that end a poll target's
  ownership *are* observer-gated: `StopConcurrentBuild` and `SetWorkSource` drain an
  in-flight poll before the executor reclaims the build's session, a wait bounded by the
  read's own deadline (the build's read runs detached from its observer's context, so an
  observer giving up never closes the session) and the session's `statement_timeout`, and `StopWorkSource` and
  `SetConcurrentBuild` drain one before an engine step releases the state its `WorkSource`
  reads, a wait the `WorkSource` contract requires `Work` to bound itself — memory reads or
  catalog reads under a session `statement_timeout`, never the observer's context alone),
  stdlib. Adding one requires a recorded decision (see the rubric in
  [docs/tcb-model.md](docs/tcb-model.md) — copy small things, take pinned dependencies only
  for load-bearing expertise).
  Recorded decision: `google.golang.org/protobuf/reflect/protoreflect` is used
  by the parse boundary to traverse the parser's generated AST and reject relation
  dependencies in policy expressions. It is the parser's existing pinned runtime,
  now a direct dependency; it does not interpret SQL or authorize execution.
  Recorded decision: `jackc/pglogrepl` (pinned) is admitted to the core for `pkg/decode` because
  the streaming-replication protocol and `pgoutput` message decoding are load-bearing
  wire-protocol expertise, under the same rubric as the parser; it is confined to `pkg/decode`
  (the depguard `decode` rule admits it there and nowhere else in the core) and is pinned to a
  commit in `go.mod`, as the module publishes no tags.
  Recorded decision: the AWS SDK (`aws-sdk-go-v2`) is a test-harness-only dependency, confined
  behind the `ministack` build tag in `internal/testutil` — it never appears in the core, in
  `cmd/pg-sprite`, or in any ordinary build; a plain `go build ./...` / `go test ./...` never
  compiles it.
  Recorded decision: `pkg/schemadiff`'s catalog introspection (`Introspect`, `IntrospectTx`,
  `Model`) is admitted to the core for `pkg/schemachange` because the checkpoint fingerprint
  ST-2 keys on is the digest of the execute-and-introspect model, and the design fixes that
  model as `pkg/schemadiff`'s ([copy-and-swap D1](docs/copy-and-swap-design.md#d1--no-durable-scratch-database));
  a second in-core introspection would have to stay identical to it to be worth anything.
  `pkg/schemadiff` imports only `pgx/v5`, `pkg/dbconn`, and `pkg/statement`, so no new
  third-party code enters the core, but its introspection queries now carry the core's review
  bar even though the package's diff and render duties stay periphery.
  pg-sprite **never imports `block/spirit` as a module**: we port ideas with citations, not
  code.
- **Priorities when trade-offs are hard:** Correctness → Readability → Ease of use →
  Performance.

## Working here with AI assistance

- **Inside the core: less AI, more steering.** Spec first (the design docs + invariant IDs),
  test-first with the invariant's named test obligation, small diffs, careful review.
- **Outside the core: more AI, less steering.** Iterate at inference speed; the boundary means
  a bug in the periphery cannot corrupt data.

The atomic RLS executor also admits `pkg/schemadiff` scratch introspection, table
comparison, render admission, and catalog-derived RLS rendering into the core. Those calls refuse mixed or
unsupported table shapes; final catalog comparison gates commit (RS-1..RS-4). Reviewed execution binds the locked SQL to the
caller’s review before target DDL (RS-5).
