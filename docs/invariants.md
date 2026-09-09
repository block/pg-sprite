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
the checksum phase. The same rule applies to the continuous checker (the pre-cutover re-check
loop; a deferred-cutover mode, if ever built, would run the same checker longer): resuming
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
copier's watermark are **discarded**, which is sound only because v1 restricts the chunk key to
one integer-family PK with a monotonic watermark
([copy-and-swap D4](copy-and-swap-design.md#d4--restrict-the-chunk-key-to-one-integer-family-primary-key));
composite and non-comparable PKs are refused in v1 rather than queued; a delete for a key inside
an in-flight chunk must be re-applied after that chunk lands. Full statement and the races these
resolve:
[low-level-design § copy and apply ordering](low-level-design.md#copy-and-apply-ordering-the-core-correctness-subtlety).
*Enforced:* copier/applier SQL shapes + flush scheduling. *Source:* this doc set (Spirit's
model translated).

### CO-5 — The change buffer is disjoint and current at flush time

At every flush, each PK appears **at most once** in the change buffer, holding the **latest** row
image (or a delete marker) — dedup is what makes catch-up convergent rather than linear. Dedup
**merges, never replaces**: a newer UPDATE image overlays only the columns it carries onto the
buffered image, so an unchanged-TOAST marker (CO-8) survives dedup only when no buffered image
for that key ever held the column's value; a delete marker replaces the image outright, and an
INSERT after a delete replaces the marker. The buffer is a single keyed map: v1 has one
integer-family PK
([copy-and-swap D4](copy-and-swap-design.md#d4--restrict-the-chunk-key-to-one-integer-family-primary-key)),
so Spirit's map ↔ FIFO-queue mode toggle for non-memory-comparable keys has no v1 counterpart
and returns only if queue mode is ever built. *Enforced:* buffer data structure (merge on
overlay). *Source:* Spirit `pkg/change/subscription_buffered.go` (stated invariant), narrowed to
the v1 key shape; the merge rule is this doc set's addition for pgoutput's partial images.

### CO-6 — Unique-secondary-key moves must converge (PostgreSQL-specific gap)

Spirit applies via `REPLACE INTO`, which **deletes** rows that collide on *any* unique key; a
transiently-deleted row converges because its own event re-inserts it (the buffer-disjointness
guarantee, CO-5). PostgreSQL has no REPLACE: `INSERT … ON CONFLICT (pk) DO UPDATE` targets
**one** conflict arbiter, so a batch that legally moves a unique value between rows (set
`slot_id` NULL on row 1, then `'S'` on row 2, in one source transaction) can **error** on the
secondary unique index instead of converging. The decided semantics
([copy-and-swap D13](copy-and-swap-design.md#d13--recover-unique-secondary-key-moves-batch-wide))
are key-targeted upserts, and on `23505` a savepoint rollback and batch-wide
delete-all-then-insert-all; per-key delete-then-insert retry is rejected because a cyclic
exchange collides in both orders. The fallback inserts whole rows, so it must not invent a value
for an unchanged-TOAST column (CO-8): before deleting, it completes any surviving image that
still carries a marker from the current shadow row (`SELECT … FOR UPDATE`, same transaction), and
treats an absent shadow row for such an image as an invariant violation (fail closed) — CO-5's
merge rule guarantees the marker only survives for rows that pre-existed on the source.
Convergence must still be proved under test. *Test obligation:*
`seats(id int PRIMARY KEY, slot text UNIQUE)` holding `(1,'A'),(2,'B')`, flushed with the batch
`{1→'B', 2→'A'}`, converges in one flush; a second vector adds a ≥8 KiB TOASTed column left
untouched by both updates and asserts it survives the fallback byte-for-byte. The checksum (CO-1)
backstops, but the applier must converge without it. *Enforced:* applier batch semantics
(Phase 6). *Source:* Spirit
`pkg/change/README.md` (the REPLACE rationale) — the PG translation in
[mysql-vs-postgresql](mysql-vs-postgresql.md#copy-and-swap-executor-spirit-mysql--postgresql-primitive-mapping)
is incomplete without this.

### CO-7 — Every statement parses, or it is an error

All SQL the engine processes must parse with the real PostgreSQL grammar — `wasilibs/go-pgquery`,
the Wasm build of `libpg_query` (the cgo `pg_query_go` is the API-compatible escape hatch). No
`strings.Split(";")` fallback, no silently skipping unparseable statements — a parse failure is
surfaced to the caller as an error. The invariant pins the *capability* (classified or refused);
the parser choice is an implementation decision of the understanding layer (see
[low-level-design](low-level-design.md#how-the-planner-understands-ddl-decided)).
*Enforced:* `pkg/statement` boundary. *Source:* SchemaBot AGENTS.md (TiDB-parser hard
requirement, rewritten for our parser); carried in the repo's [AGENTS.md](../AGENTS.md).

### CO-8 — A TOAST-omitted column is never overwritten

An UPDATE decoded from pgoutput must change only columns whose new-tuple field carries a value.
Under either PK-based replica identity (`DEFAULT` or `FULL`), pgoutput sends an unchanged TOASTed
value in the new tuple as the unchanged-TOAST marker (type byte `u`) — the column is present, the
column count is the full count, and there is no value; the marker means "leave the stored value
unchanged", not NULL or an empty value. A full-row upsert that invents a value for that column
would overwrite live shadow data and silently break convergence. *Enforced:* `pkg/decode`
per-column presence on `ChangeEvent`, `pkg/applier` column-wise UPDATE construction from it.
*Source:* [copy-and-swap D6](copy-and-swap-design.md#d6--preserve-omitted-toast-values).
*Test obligation:* a convergence test updates other columns while leaving a ≥8 KiB column
untouched, using the load generator's TOAST-unchanged update profile, run under both
`REPLICA IDENTITY DEFAULT` and `FULL`.

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

*Planned enforcement:* `pkg/dbconn` lock type, verified before any write and monitored throughout.
*Source:* Spirit `pkg/dbconn/metadatalock.go` (stated pool invariants). This resolves the
mutual-exclusion gap called out in the validation review.

### LK-2 — Exactly one `ACCESS EXCLUSIVE` window, and every strong lock is bounded

The cutover swap is the only `ACCESS EXCLUSIVE` acquisition in the happy path, and **every**
strong-lock acquisition (swap, catalog flips, trigger install if trigger capture is ever built) runs under
`lock_timeout` + bounded retry/backoff so the engine never sits at the head of the lock queue
([mysql-vs-postgresql § the lock queue](mysql-vs-postgresql.md#why-ddl-is-dangerous-the-lock-queue)).
**Exception policy required:** `CREATE INDEX CONCURRENTLY` and `REINDEX CONCURRENTLY` wait on
other transactions via lock waits that a naive `lock_timeout` cancels — leaving an `INVALID`
index — so they get their own wait policy rather than the blanket timeout: no per-lock timeout,
with either one overall server statement deadline or a caller-owned cancellable context as the
statement's only bound. In caller-owned mode the executor refuses a non-cancellable context, so
the client call is bounded by construction; the server statement is not — it runs with
`statement_timeout` off and stops only on a cancel request, so a client that dies without
cancelling leaves it running until `Tracker.CancelBuild` or an operator's `pg_cancel_backend`
stops it. `VALIDATE CONSTRAINT` is different in kind: its cancellation is
transactionally clean (the constraint simply stays `NOT VALID`; no debris), so the sequence
executor's validate class deliberately keeps a bounded per-lock timeout — queueing behind a
conflicting lock holder must not stall a sequence for the whole scan budget — while the scan
itself runs under its own generous overall budget. *Enforced:* every DDL execution path in the
native and copy-and-swap executors.
*Source:* [design-principles](design-principles.md#correctness-and-safety), [mysql-vs-postgresql](mysql-vs-postgresql.md#why-ddl-is-dangerous-the-lock-queue);
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
[Spirit README](https://github.com/block/spirit#cut-over-and-cleanup)).

### LK-5 — An index is dropped only by proven identity, under the lock that excludes its builder

pg-sprite removes an invalid index only when it has **proved** the entry is abandoned debris,
and a proof is a statement about an identity (`pg_class.oid`), never about a name: the same
name can be occupied by a different entry between any two observations. The proof rests on
`SHARE UPDATE EXCLUSIVE` on the index's table — the lock every concurrent index command
(`CREATE`, `DROP`, `REINDEX … CONCURRENTLY`) holds for its whole life, so holding it proves no
build of *any* index on the table is in flight, including one this role cannot observe.
Under that lock, in the same transaction, the executor re-verifies the candidate by OID
(still under the observed name, still invalid, still on the target table by OID, schema and
the table name the statement gave — a rename keeps the OID but the statement no longer names
the table — still an index the server will drop concurrently, no visible builder) and renames it to a
**quarantine name derived from its OID** — the only mutation the lock licenses. Any
disagreement between the unlocked observation and the locked re-check fails closed
(`ErrTargetIdentityChanged` when the table's identity moved, `ErrAbandonmentUnproven`
otherwise) and mutates nothing. The subsequent `DROP INDEX CONCURRENTLY` — which cannot run
inside the locking transaction — targets only quarantine names, re-verifies the same facts
by OID immediately before it, and trusts the server's success only once the OID is gone.
A valid index is never a candidate; an invalid index on a partitioned table, an index
partition, or a constraint's index is never a candidate (the server will not drop it
concurrently, so the rename would strand it). Every lock the recovery takes is bounded, and
the proof's lock, every drop, and the requested build share **one** overall budget, so a
recovery over k quarantined entries costs at most the budget, not (k+1) budgets.
The declarative diff leans on this proof without performing it: when desired names an index
whose live entry is an unfinished build, `diff` plans the `create-index` alone and never a
drop, knowing the create cannot run as-is against the occupied name. The plan completes only
through this invariant's proven removal — `RebuildAbandonedIndex` — or through an operator
following the runbook; a plain `CREATE INDEX` on the occupied name fails as a duplicate
relation, and the concurrent build path refuses it by proof rather than drop by name.
*Enforced:* `pkg/executor` recovery (`RebuildAbandonedIndex`): locked re-verification and
rename, pre-/post-drop OID checks, droppability predicate, shared-budget accounting, with
stale-observation tests that alter the catalog between observation and lock on a real
database. *Source:* PostgreSQL's session-level `ShareUpdateExclusiveLock` on the heap for
every `CONCURRENTLY` index command; [invalid-index-recovery](invalid-index-recovery.md).

## State, checkpoint, and resume (ST)

### ST-1 — The checkpoint is one row per target, written atomically

The checkpoint table keeps **one row per `(schema, table)`** (upsert on that key) so a crash can
never leave a partial pair for one target — its record is either the old or the new one.
Unbounded append-style checkpoint history is not used. *Planned enforcement (Phase 8):*
`pkg/checkpoint` write path (`INSERT … ON CONFLICT (schema_name, table_name) DO UPDATE`, the REPLACE
analog). *Source:* Spirit `pkg/checkpoint` (single-row REPLACE on `id=1`), scoped per target by
[copy-and-swap D3](copy-and-swap-design.md#d3--store-checkpoints-in-the-target-database).

### ST-2 — An incompatible checkpoint is distinguishable from a transient read error

Resume must tell apart: (a) a readable, matching checkpoint → resume; (b) a checkpoint written by
an incompatible engine version or for a **different statement** → refuse to resume, start fresh
(never mix state across versions/statements); (c) a *transient* read failure → retry, and never
trigger fresh-start recovery on a blip. *Enforced:* checkpoint read/validation path (version +
statement fingerprint stored with the watermark; the fingerprint will hash the
execute-and-introspect after-schema model, not SQL text, so textually-different-but-identical statements match and
cosmetic edits don't force a fresh start). *Source:* Spirit `checkpoint.IsIncompatible` +
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
(~2× the table), execute-and-introspect workspace, lock LK-1 acquired, and the RF-* refusals
below. Failing hours into a copy on something knowable up front is a bug. Copy-and-swap needs no
durable scratch database or `CREATEDB`: its gated DDL executes against the empty shadow and its
checkpoint fingerprint uses the rolled-back, transaction-scoped `pkg/schemadiff` scratch schema.
The server is the semantic authority and client-side parsing is advisory. *Enforced today:*
declarative diff. *Planned enforcement:* all execution paths in preflight. *Source:*
[design-principles](design-principles.md#correctness-and-safety).

### ST-7 — The executor runs exactly the statement that was gated

The executor accepts only a parsed `statement.Statement` — constructible solely by `ParseOne`,
which enforces exactly one statement through the real grammar — and refuses, before anything
executes, any statement whose target table does not match the preflight proof it was handed.
A proof for one table can never smuggle SQL against another, and a multi-statement string can
never reach the database through the executor (pgx's simple protocol would happily run all of
it). *Enforced:* `pkg/executor` (`ExecuteNative`; `RunSequence` admission re-proves every step's
target against the preflight proof before the first step executes; `ExecuteCreate` re-proves
every desired statement's target against the absence proof the same way), `pkg/statement`
(proof construction). *Planned enforcement:* the `pkg/schemachange` shadow builder re-proves the
retargeted statement against the gated one with the shadow as the sole permitted target
([copy-and-swap D1](copy-and-swap-design.md#d1--no-durable-scratch-database)).
*Source:* adversarial review of the optimistic front door.

### ST-8 — A desired schema's statements carry execution order in the proof

A `statement.DesiredSchema` orders its statements for execution at construction — the
`CREATE TABLE` first, the indexes keeping their input order after it — so every replay of
the file states the same order and the position mapping between a greenfield plan's
statements and the create path's step verdicts holds by construction, not by each replay
site re-deriving the rule. A set that does not lead with a `CREATE TABLE` means the proof
was forged or mutated, and every consumer refuses it fail-closed rather than reordering.
*Enforced:* `pkg/statement` (`ParseDesired` establishes the order), `pkg/diffplan`
(`qualifiedDesired` asserts it when rendering the greenfield plan), `pkg/executor`
(`checkCreateSteps` asserts it before anything is planned or run); `pkg/schemadiff`'s scratch
materialization relies on it to run the `CREATE TABLE` before its indexes.
*Source:* adversarial review of the declarative front door.

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
- **RF-6** — A partitioned-parent sequence is refused before its first step when it would build
  an index, or on PostgreSQL before 18 when it would add a foreign key `NOT VALID`. pg-sprite
  does not substitute a blocking parent build for the missing partition-aware online flow.
  *Enforced:* preflight and sequence-executor admission. *Source:* PostgreSQL relation-kind and
  version capabilities.

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

The declarative diff makes one deliberate trade against this invariant. An invalid index on
a plain table is an unfinished concurrent build — abandoned, or still running — and the diff
never emits a `DROP INDEX` for it, whatever the desired file says about its name: a drop by
name cannot tell the two apart, and dropping under a running build is the cleanup this
invariant forbids. That keeps the cleanup half. It gives up the other half when the desired
file no longer names the index at all: the observation is discarded and the plan is empty,
so a desired state that removes an index the build never delivered reads as already
converged. The trade is preferred to the alternatives — a name-based drop, or a fabricated
change — because the plan stays executable and never destroys a build it cannot see; the
leftover surfaces at the next `pull` (see [pull.md](pull.md)) and clears per the
[invalid-index runbook](invalid-index-recovery.md). OC-1 reaches the same conclusion from
the other side: the diff cannot confirm the entry is abandoned, so it must not round the
uncertainty into a destructive change. *Enforced:* `pkg/schemadiff` never-drops-invalid
tests against real debris on a real database.

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
| CO-7, RF-1..RF-6 | 1–3 (gate/linter/executor) | golden refusal/parse and executor-admission tests |
| LK-1 | 0–1 (before any executing mode ships) | two-instance mutual-exclusion + keepalive-loss test |
| LK-2 | 3 (native), 7 (cutover) | lock-bounding + CIC-exception tests |
| CO-1, CO-2, CO-3 | 5 (gate), 8 (watermark/divergence policy) | inject-divergence, repair-invalidates-watermark |
| CO-4, CO-5, CO-6, CO-8 | 6 | one convergence test per race, incl. unique-value move and TOAST-unchanged update |
| LK-3 | 4–6 | cancellation/claim race test |
| LK-5 | 3 (native recovery) | stale-observation fail-closed tests, never-drops-valid, not-droppable skip, shared-budget test |
| LK-4, ST-5 | 7 | dropped-connection cutover, fidelity checklist |
| ST-1, ST-2, ST-3, ST-4 | 8 | kill/resume, cross-version refuse, orphan-slot reap, failover reconcile |
| ST-6 | 1 onward, complete by 8 | preflight matrix |
| ST-7 | 1 | target-mismatch refusal + single-statement-by-construction tests |
| ST-8 | 2 (declarative model) | parse-time ordering + forged-proof refusal tests at every replay site |
| OC-1..OC-6 | shape APIs from 2; bind at 11 | engine-contract tests |
