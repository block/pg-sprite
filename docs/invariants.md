# Engine invariants

The canonical registry of **runtime invariants** — testable MUST-statements the engine enforces
in code. This is the enforceable-rule companion to
[design-principles.md](design-principles.md): the principles say *what we value* (safety over
speed, decisions not options); this doc says *what must never be false at runtime*, where each
rule is enforced, and where it came from. Every invariant cites its source — this doc set,
[Spirit](https://github.com/block/spirit)'s codebase (which states several of these as explicit
`Safety invariant:` comments), or [SchemaBot](https://github.com/block/schemabot)'s AGENTS.md and
control-plane docs — so the lineage survives the port.

**How to use this doc during the build:** each invariant carries an ID (`CO-*` correctness,
`LK-*` locking/concurrency, `ST-*` state/resume, `RF-*` refusals, `OC-*` orchestration). The
build-plan phase that implements an invariant must land a test named for it;
the [phase mapping](#build-phase-mapping) is at the end. The code that enforces these invariants
is the engine's **trusted computing base** — the boundary, the domain types that make violating
several of these unrepresentable, and the in-TCB engineering rules live in
[tcb-model.md](tcb-model.md).

## Table of contents

- [Correctness (CO)](#correctness-co)
- [Locking and concurrency (LK)](#locking-and-concurrency-lk)
- [State, checkpoint, and resume (ST)](#state-checkpoint-and-resume-st)
- [Refusals and preflight (RF)](#refusals-and-preflight-rf)
- [Orchestration / control-plane (OC)](#orchestration--control-plane-oc)
- [Engineering invariants live in AGENTS.md](#engineering-invariants-live-in-agentsmd)
- [Build-phase mapping](#build-phase-mapping)

## Correctness (CO)

### CO-1 — The checksum gate is non-skippable

A migration that cannot prove shadow == source **must refuse to cut over**. No flag, mode, or
capture mechanism removes the gate; it is also the repair primitive for
[slot-loss reconciliation](low-level-design.md#failover-during-migration-what-survives-and-what-doesnt).
*Enforced:* cutover entry condition. *Source:* [design-principles](design-principles.md#correctness-and-safety),
risks-and-mitigations; Spirit's "never skip it".

### CO-2 — A persisted checksum watermark describes only chunks verified clean on a fresh read

Spirit states this as an explicit safety invariant (`pkg/migration/runner.go`): a chunk that
needed a **repair** has not been *verified* — only the recopy succeeded — yet the chunker's
low-watermark advances past every chunk it sees feedback for, including repaired ones. So in any
checksum pass where **any** chunk was repaired, the watermark is not a valid resume point until a
later pass re-checks those chunks clean. The engine must persist an **empty** checksum watermark
whenever the current pass has had repairs, forcing a resumed run to re-verify from the start of
the checksum phase. The same rule applies to the continuous (deferred-cutover) checker: resuming
from a stale watermark after a continuous-checker repair would let a re-run "pass" by verifying
only trailing chunks — silently neutralizing a deliberate divergence abort.
*Enforced:* checkpoint writer (watermark dropped unless **all** active checkers are clean).
*Source:* Spirit `pkg/migration/runner.go` + `pkg/move/runner.go` ("Safety invariant").

### CO-3 — Divergence policy is an explicit setting, never inferred

Whether a confirmed, stable source/shadow divergence **aborts** or **self-heals by recopy** is an
explicit per-mode policy (Spirit's `ContinuousCheckerConfig.DivergenceIsFatal`), not something
inferred from whether a recopier happens to be wired up:

- **Steady-state migration** (CDC keeping the shadow in sync): divergence is a real bug —
  `DivergenceIsFatal = true`, abort the cutover.
- **Reconciliation after slot loss** (the checksum-repair pass): divergence is *expected* —
  `DivergenceIsFatal = false`, and a recopier is **mandatory** (self-heal without one is treated
  as fatal).
- The two knobs stay decoupled: fatal-divergence aborts even if a recopier is supplied.

*Enforced:* checker configuration per lifecycle mode. *Source:* Spirit AGENTS.md
(block/spirit#994 policy) — maps directly onto our failover-reconcile design.

### CO-4 — The copy/apply ordering invariants

The copier **never overwrites** (`ON CONFLICT (pk) DO NOTHING`); the applier **always
overwrites** (`ON CONFLICT (pk) DO UPDATE` + explicit deletes); captured changes above the
copier's watermark may be **discarded only for a monotonic integer PK**, and must be queued for
composite/non-comparable PKs; a delete for a key inside an in-flight chunk must be re-applied
after that chunk lands. Full statement and the races these resolve:
[low-level-design § copy and apply ordering](low-level-design.md#copy-and-apply-ordering-the-core-correctness-subtlety).
*Enforced:* copier/applier SQL shapes + flush scheduling. *Source:* this doc set (Spirit's
model translated).

### CO-5 — The change buffer is disjoint and current at flush time

At every flush, each PK appears **at most once** in the change buffer, holding the **latest** row
image (or a delete marker) — dedup is what makes catch-up convergent rather than linear. After a
mode transition (map ↔ FIFO-queue for non-memory-comparable PKs), **only the active store may
hold entries**: the outgoing store is drained inline at the toggle, so no flush ever has to merge
a stale store. *Enforced:* buffer data structure + the mode-toggle transition. *Source:* Spirit
`pkg/change/subscription_buffered.go` (stated invariant).

### CO-6 — Unique-secondary-key moves must converge (PostgreSQL-specific gap)

Spirit applies via `REPLACE INTO`, which **deletes** rows that collide on *any* unique key; a
transiently-deleted row converges because its own event re-inserts it (the buffer-disjointness
guarantee, CO-5). PostgreSQL has no REPLACE: `INSERT … ON CONFLICT (pk) DO UPDATE` targets
**one** conflict arbiter, so a batch that legally moves a unique value between rows (set
`slot_id` NULL on row 1, then `'S'` on row 2, in one source transaction) can **error** on the
secondary unique index instead of converging. The applier must define semantics for this —
order-preserving apply within the batch, per-row retry on unique violation, or delete-then-insert
pairs — and prove convergence under test. The checksum (CO-1) backstops, but the applier must
converge without it. *Enforced:* applier batch semantics (design work, Phase 6). *Source:* Spirit
`pkg/change/README.md` (the REPLACE rationale) — the PG translation in
mysql-vs-postgresql
is incomplete without this.

### CO-7 — Every statement parses, or it is an error

All SQL the engine processes must parse with `pg_query_go`. No `strings.Split(";")` fallback, no
silently skipping unparseable statements — a parse failure is surfaced to the caller as an error.
*Enforced:* `pkg/statement` boundary. *Source:* SchemaBot AGENTS.md (TiDB-parser hard
requirement, rewritten for our parser); carried in the repo's [AGENTS.md](../AGENTS.md).

## Locking and concurrency (LK)

### LK-1 — At most one migration runs per table

Migrations serialize per table via a **session-scoped advisory lock** (`pg_advisory_lock` on a
key derived from database + table — the analog of Spirit's `GET_LOCK` `MetadataLock`), with
Spirit's hard-won connection rules carried over:

- The lock is held on a **dedicated pool of exactly one connection**, exempt from client-side
  connection recycling (a recycled connection silently releases a session lock — a window in
  which a second instance could start a concurrent migration on the same table).
- A **keepalive** re-acquires on an interval strictly shorter than any server/idle timeout that
  could kill the session; if the keepalive fails, the connection is torn down and re-established.
- **Losing the lock is fail-closed:** if the lock cannot be confirmed held, the migration aborts
  rather than continuing unprotected.

*Enforced:* `pkg/dbconn` lock type, verified before any write and monitored throughout.
*Source:* Spirit `pkg/dbconn/metadatalock.go` (stated pool invariants). This resolves the
mutual-exclusion gap called out in the validation review.

### LK-2 — Exactly one `ACCESS EXCLUSIVE` window, and every strong lock is bounded

The cutover swap is the only `ACCESS EXCLUSIVE` acquisition in the happy path, and **every**
strong-lock acquisition (swap, catalog flips, trigger install in fallback mode) runs under
`lock_timeout` + bounded retry/backoff so the engine never sits at the head of the lock queue
(mysql-vs-postgresql § the lock queue).
**Exception policy required:** `CREATE INDEX CONCURRENTLY` (and `REINDEX CONCURRENTLY`,
`VALIDATE CONSTRAINT`) wait on other transactions via lock waits that a naive `lock_timeout`
cancels — leaving an `INVALID` index. These statements get their own wait policy rather than the
blanket timeout. *Enforced:* every DDL execution path in the native and copy-and-swap executors.
*Source:* [design-principles](design-principles.md#correctness-and-safety), mysql-vs-postgresql;
CIC exception from the validation review.

### LK-3 — Pending work is claimed exactly once, and Wait means finished

For the parallel copier/applier: a pending-work entry is **claimed** by removing it from the
pending set **and** incrementing an in-flight counter **in the same critical section** — exactly
one path (success, error, or cancellation cleanup) can claim an entry, so its completion callback
runs exactly once. The claimer invokes the callback **without** holding the lock (callbacks may
be slow or re-enter the applier). `Wait()` returns only when the pending set is empty **and** the
in-flight counter is zero — it can never return while a callback is still running. *Enforced:*
applier/copier concurrency structure. *Source:* Spirit `pkg/applier/single_target.go` +
`sharded.go` ("Completion invariant", block/spirit#765).

### LK-4 — An ambiguous cutover outcome is resolved by inspection, never assumed

If the connection drops mid-swap (around `COMMIT`), the engine must determine from the catalog
**which table now bears the source name** before retrying or reporting — never assume the rename
did or didn't commit. PostgreSQL's transactional DDL makes the swap itself atomic, but the
*client's knowledge* of the outcome is not. Retries of the cutover must be written against this
ambiguity. *Enforced:* cutover retry loop. *Source:* Spirit's cutover
(`information_schema` inspection on dropped connection,
spirit-architecture-notes).

## State, checkpoint, and resume (ST)

### ST-1 — The checkpoint is a single row, written atomically

The checkpoint table keeps **one row** (upsert on a fixed key) so a crash can never leave *zero*
checkpoints or a partial pair — there is always exactly one, and it is either the old or the new
one. Unbounded append-style checkpoint history is not used. *Enforced:* `pkg/checkpoint` write
path (`INSERT … ON CONFLICT (id) DO UPDATE`, the REPLACE analog). *Source:* Spirit
`pkg/checkpoint` (single-row REPLACE on `id=1`).

### ST-2 — An incompatible checkpoint is distinguishable from a transient read error

Resume must tell apart: (a) a readable, matching checkpoint → resume; (b) a checkpoint written by
an incompatible engine version or for a **different statement** → refuse to resume, start fresh
(never mix state across versions/statements); (c) a *transient* read failure → retry, and never
trigger fresh-start recovery on a blip. *Enforced:* checkpoint read/validation path (version +
statement fingerprint stored with the watermark). *Source:* Spirit `checkpoint.IsIncompatible` +
"resume requires the identical ALTER".

### ST-3 — Slot cleanup is guaranteed on success, failure, and crash

Replication slots are created with a recognizable name prefix; a reaper drops orphaned
engine-prefixed slots (including one stranded on a demoted writer after failover); a hard
slot-lag ceiling aborts the migration before an abandoned slot can fill the volume. No exit path
leaves a slot behind silently. *Enforced:* slot lifecycle manager + reaper + throttler ceiling.
*Source:* risks-and-mitigations § logical-decoding risks.

### ST-4 — Slot loss is a modeled state transition, not a crash

Losing the slot (Aurora failover) enters **reconcile mode** — keep the shadow and copy watermark,
new slot, checksum-repair pass under the CO-3 self-heal policy — and is handled distinctly from a
process crash (slot survives, clean resume). The engine detects writer-identity changes and slot
disappearance rather than blindly continuing.
*Enforced:* checkpoint/resume state machine (Phase 8). *Source:*
[low-level-design § failover](low-level-design.md#failover-during-migration-what-survives-and-what-doesnt).

### ST-5 — The swap is gated on a fidelity checklist, not just the checksum

Before cutover the engine verifies the shadow carries the source's **owner, grants/ACLs, RLS
policies, comments, storage parameters**, that **sequences are re-owned and advanced past the
source's current values** (`setval`), and that indexes are valid (`pg_index.indisvalid`). Data
equality (CO-1) plus metadata fidelity, or no swap. *Enforced:* cutover preconditions. *Source:*
[low-level-design § operational caveats](low-level-design.md#operational-caveats),
risks-and-mitigations.

### ST-6 — Preflight before the first write

Every knowable prerequisite is validated before the engine writes anything: logical-replication
enablement and role, PK usability, `REPLICA IDENTITY`, slot/WAL-sender headroom, disk headroom
(~2× the table), lock LK-1 acquired, and the RF-* refusals below. Failing hours into a copy on
something knowable up front is a bug. *Enforced:* preflight stage. *Source:*
[design-principles](design-principles.md#correctness-and-safety).

## Refusals and preflight (RF)

Each refusal is a preflight **error with a stated reason** — never a warning, never attempted.

- **RF-1** — The table must have a usable PK (or `NOT NULL UNIQUE` key), and the migration must
  not alter or drop it. The PK is simultaneously chunk key, conflict target, and resume
  watermark. *Source:* [low-level-design](low-level-design.md#table-shape-requirements-preconditions-to-even-start), Spirit.
- **RF-2** — No FKs referencing the table, no triggers on it, no dependent **views**, no
  **publication membership** (v1) — the OID-bound dependents a rename-swap strands.
  *Source:* [low-level-design coverage](low-level-design.md#schema-shapes), risks-and-mitigations.
- **RF-3** — Lossy **or failable** conversions are refused up front (shortening below max data
  length, `NOT NULL` without default on null data, `text→jsonb` with unvalidatable rows) rather
  than discovered mid-copy. *Source:* Spirit blocklist + validation review.
- **RF-4** — Renames are never guessed: a missing-plus-new column pair is drop+add unless rename
  intent is explicit; dangerous rename-overlap patterns are refused. *Source:*
  [low-level-design § declarative safety rules](low-level-design.md#safety-rules-inherited-philosophy-surprise-free-decisions-not-options), Spirit.
- **RF-5** — The dangerous literal never runs silently: risky statements with a safer native
  idiom get the idiom (reported) or a recommendation; running as-submitted requires the loud,
  typed, audited `--force`. *Source:* [high-level-design § advisory mode](high-level-design.md#advisory-mode-suggest-the-safe-rewrite-dont-silently-run-the-risky-one).

## Orchestration / control-plane (OC)

From the orchestrator's operational discipline — the integration itself lives in
[schemabot-integration.md](schemabot-integration.md). These bind fully at the integration
phase, but they shape the engine's state and API surface from day one.

### OC-1 — Fail closed on uncertainty

Storage uncertainty, engine-state uncertainty, ownership ambiguity, or in-flight ambiguity must
**never** be converted into a passing/ready/succeeded status. Concretely: if the engine cannot
confirm the checksum state or the slot position, `status` reports the uncertainty and `cutover`
refuses — it never rounds up to "ready". *Source:* SchemaBot AGENTS.md ("safety gates first").

### OC-2 — Started migrations remain authoritative

Once a migration has **started** (shadow/slot/triggers exist), a later change of intent — the PR
updated, the desired-state file reverted, a new plan — must not silently mark it succeeded or
clean it up. The started operation blocks until an operator verb (`cancel`, `cutover`) resolves
it and the target is reconciled. Cleanup alone never declares success. *Source:* SchemaBot
AGENTS.md ("started applies remain authoritative").

### OC-3 — Control requests are durable operator intent

`stop` / `start` / `cutover` / `cancel` are stored durably and reconciled to completion or
explicit failure; they are never dropped on a crash, and never retried unboundedly without fresh
operator intent. *Source:* SchemaBot `docs/grpc-control-edge-cases.md`.

### OC-4 — TOCTOU discipline on all async state

Wherever two actors can race (scheduler vs engine, two engine instances, operator vs
reconciler), state updates are conditional (compare-and-set / ownership token) and decisions are
made on a **final state reload**, so a stale actor cannot overwrite newer state. LK-1 is the
engine-side anchor; the orchestration layer needs the same at its own store. *Source:* SchemaBot
AGENTS.md ("TOCTOU review").

### OC-5 — ID namespaces are never conflated

The engine's migration identifier is an **opaque external ID** to any orchestrator; the
orchestrator's user-facing identifier is never routed to the engine. Every engine API takes
exactly one of them, by name. *Source:* SchemaBot `docs/grpc-control-edge-cases.md`
("Remote apply ID invariant").

### OC-6 — Shared interfaces stay engine-agnostic

No PostgreSQL-specific fields (slot names, LSNs, `REPLICA IDENTITY` details) in shared
engine/API types — engine-specific data rides in generic `Metadata map[string]string`, and
PG-only machinery stays behind `Apply`/`Stop`/`Cancel`. *Source:* SchemaBot AGENTS.md;
[schemabot-integration.md](schemabot-integration.md).

## Engineering invariants live in AGENTS.md

The process-level rules mined from both repos — never silently fail; error early, never swallow;
no silent branch cases; wrap errors with context and identifiers; logs answer the triage
question; one owner closes a handle; tests prove documented behavior; integration tests against
a real database, no mocked-DB core tests; no `nolint`; no `--no-verify` — belong in the repo's [`AGENTS.md`](../AGENTS.md), not in this runtime registry.
This doc holds only invariants about the **database state machine**; that one holds invariants
about **how we write and review the code**.

## Build-phase mapping

| Invariant | Landed by phase | Test obligation |
| --- | --- | --- |
| CO-7, RF-1..RF-5 | 1–2 (gate/linter), full at 2 | golden refusal/parse tests |
| LK-1 | 0–1 (before any executing mode ships) | two-instance mutual-exclusion + keepalive-loss test |
| LK-2 | 3 (native), 7 (cutover) | lock-bounding + CIC-exception tests |
| CO-1, CO-2, CO-3 | 5 (gate), 8 (watermark/divergence policy) | inject-divergence, repair-invalidates-watermark |
| CO-4, CO-5, CO-6 | 6 | one convergence test per race, incl. unique-value move |
| LK-3 | 4–6 | cancellation/claim race test |
| LK-4, ST-5 | 7 | dropped-connection cutover, fidelity checklist |
| ST-1, ST-2, ST-3, ST-4 | 8 | kill/resume, cross-version refuse, orphan-slot reap, failover reconcile |
| ST-6 | 1 onward, complete by 8 | preflight matrix |
| OC-1..OC-6 | shape APIs from 2; bind at 11 | engine-contract tests |
