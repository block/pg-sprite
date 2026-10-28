# Spirit vs pg-sprite: the copy-and-swap process, phase by phase

[Spirit](https://github.com/block/spirit) is the MySQL online schema change tool pg-sprite
borrows its lifecycle from; [mysql-vs-postgresql.md](mysql-vs-postgresql.md) maps the
*primitives* (binlog → logical decoding, `REPLACE INTO` → `ON CONFLICT`, `RENAME TABLE` →
transactional renames). This document maps the *process*: what each tool does in each phase,
which transactions and isolation levels it uses, how it verifies and repairs, and where
pg-sprite deliberately departs. It records ideas with citations into Spirit's source; no code
is ported (see [SAFETY.md](../SAFETY.md)).

Spirit facts are from `main` at the time of writing (October 2026) and link to the lines they
describe; Spirit is a moving target, so a claim here is a snapshot, not a contract. pg-sprite
facts link to the source or the invariant that owns them. Where pg-sprite has deliberately not
built something, the table says *deferred* and points at
[copy-and-swap-design.md](copy-and-swap-design.md#deferred-alternatives).

pg-sprite runs copy-and-swap in one of two modes, chosen from the target's proof
([runner.go](../pkg/schemachange/runner.go)). A **live** run streams the source's writes
through a logical replication slot. An **offline** run (`--block-writes SCHEMA.TABLE`,
[cli.go](../internal/cli/cli.go)) decodes no WAL and instead fences the source with a statement
trigger that refuses every write until the swap ([fence.go](../pkg/schemachange/fence.go)).
Spirit always streams the binlog; the offline mode has no Spirit counterpart.

## Table of contents

- [The two flows side by side](#the-two-flows-side-by-side)
- [Phase by phase](#phase-by-phase)
- [Transactions and isolation](#transactions-and-isolation)
- [Verification: the snapshot checker, the lockless checker, and pg-sprite's gate](#verification-the-snapshot-checker-the-lockless-checker-and-pg-sprites-gate)
- [Repair and re-verification](#repair-and-re-verification)
- [What pg-sprite takes from Spirit's lockless checksum](#what-pg-sprite-takes-from-spirits-lockless-checksum)
- [Summary of the departures](#summary-of-the-departures)

## The two flows side by side

```diagram
Spirit (MySQL)                            pg-sprite (PostgreSQL)
+-------------------------------------+   +-------------------------------------------+
| try native INSTANT / INPLACE DDL    |   | plan: classify from the grammar ->        |
| on failure fall through to copy     |   | online idiom, safer sequence, bounded     |
+-------------------------------------+   | attempt, copy-and-swap, or refusal;       |
                   |                      | a failed attempt is refused, not copied   |
                   |                      +-------------------------------------------+
                   v                                            v
+-------------------------------------+   +-------------------------------------------+
| resume from _<t>_chkpnt if any      |   | preflight proof (ST-6); per-table lock    |
| CREATE TABLE _<t>_new LIKE <t>      |   | session (LK-1); resume from the row if    |
| ALTER the new table                 |   | any; shadow = LIKE ... INCLUDING ALL      |
| create checkpoint table             |   | + the retargeted ALTER (D1, D2)           |
| start binlog change source          |   | offline:  write-fence trigger on source   |
+-------------------------------------+   | live: slot + publication; the row         |
                   |                      | records the consistent point (D11)        |
                   |                      +-------------------------------------------+
                   v                                            v
+-------------------------------------+   +-------------------------------------------+
| COPY: readers SELECT chunk rows     |   | COPY: N workers, one bounded tx per       |
| into memory; applier writes         |   | chunk, server-side INSERT ... SELECT      |
| INSERT IGNORE ... VALUES            |   | ... ON CONFLICT DO NOTHING (CO-4)         |
| chunklets; feed flushed every 30 s; |   | live: stream applied beside the copy;     |
| checkpoint = low watermark +        |   | uncut keys discarded, in-flight keys      |
| binlog position                     |   | deferred (CO-4, CO-5); then catch up      |
+-------------------------------------+   | to a WAL marker                           |
                   |                      | checkpoint = watermark + applied LSN      |
                   |                      +-------------------------------------------+
                   v                                            v
+-------------------------------------+   +-------------------------------------------+
| drain backlog; ANALYZE new table    |   | CHECKSUM (non-skippable, CO-1):           |
| CHECKSUM (mandatory):               |   |  per chunk, one REPEATABLE READ           |
|  default: LOCK TABLES, open N       |   |  read-only tx cuts the chunk and reads    |
|  REPEATABLE READ snapshots,         |   |  both sha256 digests; no table lock       |
|  unlock, compare; mismatch ->       |   |  offline:  Check; live: CheckLive         |
|  recopy chunk -> whole-table pass   |   |  settles differing chunks against the     |
|  again, <= 3 attempts               |   |  stream; the run's policy is abort        |
|  experimental: lockless, retry      |   |  (CO-3): a divergence stops the run       |
|  lineage, hot-range split           |   +-------------------------------------------+
+-------------------------------------+                         |
                   v                                            v
+-------------------------------------+   +-------------------------------------------+
| optional: wait on _spirit_sentinel  |   | gate: fidelity + verified shadow (ST-5)   |
| CUTOVER: flush; LOCK TABLES         |   | CUTOVER: one tx; ACCESS EXCLUSIVE under   |
| source, new WRITE; flush under      |   | lock_timeout, retry with backoff (LK-2);  |
| lock; carry AUTO_INCREMENT;         |   | live: drain the stream; offline:          |
| atomic RENAME TABLE; force-kill     |   | confirm the fence; rename table,          |
| blockers from performance_schema    |   | indexes, sequences (D5, D8); recheck      |
+-------------------------------------+   | catalog; COMMIT (LK-4)                    |
                   |                      +-------------------------------------------+
                   v                                            v
+-------------------------------------+   +-------------------------------------------+
| drop _<t>_old (or keep, stamped)    |   | live: drop slot + publication (ST-3)      |
| drop checkpoint table               |   | drop old table unless --keep-old (D9)     |
+-------------------------------------+   | row -> done                               |
                                          +-------------------------------------------+
```

## Phase by phase

| Phase | Spirit | pg-sprite | Why they differ |
| --- | --- | --- | --- |
| Deciding to copy | Tries the native `ALTER` first (`INSTANT`, then known-safe `INPLACE`) and falls through to the copy algorithm on failure ([runner.go](https://github.com/block/spirit/blob/main/pkg/migration/runner.go#L516-L547)). | The planner classifies the statement from the real grammar first. Proven online idioms and planner-built safer sequences run as such; proven rewrites the copy engine carries route to copy-and-swap; the unproven residue gets one bounded attempt under `lock_timeout` and `statement_timeout`. An attempt that runs out of its statement budget is rolled back and refused — it does not fall through to the copy ([optimistic-attempt.md](optimistic-attempt.md), [verdicts.go](../pkg/migrate/verdicts.go)). | MySQL asserts the algorithm it used; PostgreSQL asserts nothing, so a guess is only safe under budgets, and a rewrite has to be predicted to reach the copy engine. |
| Shadow table | `CREATE TABLE _<t>_new LIKE <t>`, then the requested `ALTER` on the new table ([change.go](https://github.com/block/spirit/blob/main/pkg/migration/change.go#L36-L92)). | `CREATE TABLE <shadow> (LIKE <source> INCLUDING ALL EXCLUDING IDENTITY)` as the owner, then the gated `ALTER` retargeted to the empty shadow; indexes and constraints exist before the copy starts ([D1](copy-and-swap-design.md#d1--no-durable-scratch-database), [D2](copy-and-swap-design.md#d2--build-indexes-and-constraints-up-front)). | Same shape. PostgreSQL's `LIKE … INCLUDING ALL` does not carry the owner, ACLs, row-level security, storage parameters, table comment, or replica identity, so the builder replicates them and the fidelity gate refuses the swap until they match (ST-5); dependent names are rewritten deterministically (D8). |
| Change capture | Binlog client subscribed per source/shadow pair, full row images recorded from the stream ([subscription_buffered.go](https://github.com/block/spirit/blob/main/pkg/change/subscription_buffered.go#L20-L29)). | Live: logical decoding over `pgoutput` from a slot whose consistent point is saved in the checkpoint row before anything is copied; the stream starts there ([runner_live_dispatch.go](../pkg/schemachange/runner_live_dispatch.go), [D15](copy-and-swap-design.md#d15--capture-changes-with-pgoutput)). The slot is created with an exported snapshot, but the copy does not read through it: every chunk read happens after the consistent point, so the position rule (CO-4) reconciles copy and stream. Offline: no capture; the write fence keeps the source still. | Both reconcile an unsnapshotted copy with the stream. pgoutput under `REPLICA IDENTITY DEFAULT` omits unchanged TOAST columns, which the applier must preserve (D6, CO-8); Spirit never has to handle that. |
| Row copy | Readers `SELECT` a chunk's rows through the client; an applier writes them back as `INSERT IGNORE … VALUES` chunklets, each in its own transaction ([buffered.go](https://github.com/block/spirit/blob/main/pkg/copier/buffered.go#L138-L186), [mysql_applier.go](https://github.com/block/spirit/blob/main/pkg/applier/mysql_applier.go#L709-L729)). Chunk size by byte budget. | Server-side: one bounded transaction per chunk runs `INSERT INTO shadow (…) SELECT … FROM source WHERE pk BETWEEN $1 AND $2 ON CONFLICT (pk) DO NOTHING`; rows never cross the wire ([chunksql.go](../pkg/internal/chunksql/chunksql.go), [copy_chunk.go](../pkg/copier/copy_chunk.go)). Chunk size in rows, scaled toward a 500 ms chunk-time target ([D12](copy-and-swap-design.md#d12--throttle-by-chunk-time-and-slot-lag)). | Spirit replaced its server-side `INSERT IGNORE … SELECT` copier because its `SELECT` side took shared next-key locks on every source row it read, contending with production writes ([pkg/copier/README.md](https://github.com/block/spirit/blob/main/pkg/copier/README.md#L23-L29), [issue #908](https://github.com/block/spirit/issues/908)). A PostgreSQL `INSERT … SELECT` reads under MVCC with no row locks, so the server-side statement is lock-free and the rows never leave the server. |
| Copy guard rails | Chunk reads on the ordinary connection; the checksum is the correctness net. | Each chunk transaction sets its own `lock_timeout` / `statement_timeout`, runs as the source's owner, takes `ACCESS SHARE` on the source and `ROW EXCLUSIVE` on the shadow, confirms the per-table lock session still holds the table (LK-1), and re-resolves both OIDs against the proof before writing (ST-6) ([copy_chunk.go](../pkg/copier/copy_chunk.go)). The chunks tile the whole key space, so a watermark at the largest key means complete (CO-4). | pg-sprite's per-table lock is an advisory lock on a separate session, so the copy must prove from inside its own transaction that the session still holds it; Spirit's MDL does this implicitly. |
| Copy ↔ apply ordering | During copy, the change feed deduplicates by PK and uses the copy watermark to skip events for rows not yet copied; after copy it switches to FIFO ([subscription_buffered.go](https://github.com/block/spirit/blob/main/pkg/change/subscription_buffered.go#L31-L48)). | Same idea as a stated invariant: a captured change for a key above the cut frontier is discarded, one inside an in-flight chunk is deferred until the chunk lands, one below is applied (CO-4, CO-5); `Position.Classify` is the rule ([position.go](../pkg/copier/position.go)). | pg-sprite makes the three-way rule explicit because the applier's correctness depends on it; Spirit's is a performance optimisation that the checksum backstops. |
| Checkpoint | One `REPLACE INTO` row in `_<t>_chkpnt`: copier low watermark, checksum watermark, binlog position, the `ALTER`, lifecycle metadata ([checkpoint.go](https://github.com/block/spirit/blob/main/pkg/checkpoint/checkpoint.go#L47-L79)). | One row per `(schema, table)` in `pgsprite.pgsprite_checkpoint` in the target database: phase, copy watermark (saved every 10 s by default), last applied LSN, slot and publication names, model fingerprints, the shadow proof, and the statement text ([checkpoint.go](../pkg/checkpoint/checkpoint.go), [D3](copy-and-swap-design.md#d3--store-checkpoints-in-the-target-database)). No checksum watermark: a run resumed in verification runs the whole pass again. Resume refuses a different statement and first clears the shadow above the watermark ([resume.go](../pkg/copier/resume.go)). | pg-sprite's clear exists because an earlier run's chunks above the watermark may have landed while the applier discards changes for those keys as uncut; Spirit relies on `INSERT IGNORE` to make the re-copy idempotent. |
| Throttling | Replica lag per replica DSN, Aurora commit latency, Aurora load; slowest child wins; copier waits before each chunk read ([runner.go](https://github.com/block/spirit/blob/main/pkg/migration/runner.go#L1269-L1344)). | Chunk-time sizing only ([D12](copy-and-swap-design.md#d12--throttle-by-chunk-time-and-slot-lag)). Slot lag is a hard ceiling (default 1 GiB) that ends the catch-up fail closed, not a throttle ([catchup.go](../pkg/applier/catchup.go)). Replica-lag throttling is deferred. | An unbounded slot pins WAL on the primary, a worse failure than a lagging replica, so pg-sprite bounds it absolutely. |
| Checksum | Mandatory before cutover, after a full drain and `ANALYZE` ([runner.go](https://github.com/block/spirit/blob/main/pkg/migration/runner.go#L697-L750)). Default checker locks both tables briefly to open shared `REPEATABLE READ` snapshots; experimental lockless checker uses unaligned `READ COMMITTED` reads with retries. | Non-skippable gate (CO-1), never under the cutover lock (LK-2). Per chunk, one read-only `REPEATABLE READ` transaction cuts the chunk and reads both digests — one snapshot across both relations, no table lock ([guard.go](../pkg/checksum/guard.go)). An offline run uses `Check`; a live run uses `CheckLive`, which settles chunks the stream has not yet caught up on. See the next two sections. | PostgreSQL gives one transaction a consistent view of two relations; MySQL needs the table lock to line up snapshots across connections. "Lockless" is therefore pg-sprite's baseline, not a mode. |
| Deferred cutover | `--defer-cutover` creates `_spirit_sentinel`; the run waits for an operator to drop it, re-verifying continuously meanwhile ([sentinel.go](https://github.com/block/spirit/blob/main/pkg/sentinel/sentinel.go#L92-L132)). | Deferred: cut over as soon as the gate passes ([D10](copy-and-swap-design.md#d10--cut-over-as-soon-as-the-gate-passes)). | A waiting state lengthens slot and disk exposure and adds control states without helping the core online path (D10). |
| Cutover | Flush; `LOCK TABLES source WRITE, new WRITE`; flush again under the lock; carry the live `AUTO_INCREMENT` forward; one atomic `RENAME TABLE`; stop the feed; unlock. A timer force-kills blockers found in `performance_schema.metadata_locks` ([cutover.go](https://github.com/block/spirit/blob/main/pkg/migration/cutover.go#L350-L414), [kill.go](https://github.com/block/spirit/blob/main/pkg/dbconn/kill.go#L66-L129)). | One transaction: `ACCESS EXCLUSIVE` on source and shadow under `lock_timeout`, rolled back and retried with backoff rather than queued (LK-2); a live run drains the stream to a fresh marker, an offline run confirms the fence is still in place; recheck the shape and the fidelity proofs; rename table, indexes, statistics objects, and identity sequence (D5, D8); recheck the catalog; commit. An ambiguous outcome is resolved by catalog inspection (LK-4). Blockers are not killed ([cutover.go](../pkg/schemachange/cutover.go), [step by step](copy-and-swap-design.md#cutover-transaction-step-by-step)). | Transactional DDL makes the swap and its name repairs atomic. The lock-queue pile-up ([mysql-vs-postgresql.md](mysql-vs-postgresql.md#why-ddl-is-dangerous-the-lock-queue)) is the same in both engines; Spirit kills the blocker, pg-sprite refuses to queue behind it. |
| Cleanup | Drop `_<t>_old` unless retained (then stamped); drop the checkpoint table ([runner.go](https://github.com/block/spirit/blob/main/pkg/migration/runner.go#L645-L693)). | Once the swap commits, a live run drops its slot and publication; then the old table is dropped in a separate bounded transaction unless `--keep-old` ([D9](copy-and-swap-design.md#d9--drop-the-old-table-after-commit-by-default)), and the row is saved `done`. A stopped run keeps its slot (or fence) under its row for the next run to resume from; `--restart` discards them, and `Store.ReapOrphanSlots` drops engine slots no live row owns (ST-3, [D11](copy-and-swap-design.md#d11--bound-and-reap-logical-decoding-state)). | The slot is the extra object: left behind, it pins WAL indefinitely, so its ownership and lag ceiling are invariants rather than a tidy-up. |

## Transactions and isolation

The question "does Spirit run one `REPEATABLE READ` transaction per chunk?" has a different
answer per phase, and the answer is what shapes pg-sprite's choices.

| Where | Spirit | pg-sprite |
| --- | --- | --- |
| Session default | `transaction_isolation = READ-COMMITTED` in the DSN ([conn.go](https://github.com/block/spirit/blob/main/pkg/dbconn/conn.go#L403-L415)). | No isolation level is set on the session; every transaction sets its own `lock_timeout` / `statement_timeout`, and the ones that need a snapshot level ask for it. |
| Copy: source read | A plain query on the connection, **no transaction** ([buffered.go](https://github.com/block/spirit/blob/main/pkg/copier/buffered.go#L149-L155)). | Inside the chunk transaction, in the same statement as the write. |
| Copy: shadow write | One `READ COMMITTED` transaction **per chunklet**, through the retrying executor ([dbconn.go](https://github.com/block/spirit/blob/main/pkg/dbconn/dbconn.go#L274-L317)); a chunk may become several chunklets. | **One transaction per chunk** at the session's default isolation (`READ COMMITTED` unless the database is configured otherwise), with `SET LOCAL` budgets, `SET LOCAL ROLE` owner, lock confirmation, `ACCESS SHARE` on the source and `ROW EXCLUSIVE` on the shadow, and the OID guard (LK-1, LK-2, ST-6) ([copy_chunk.go](../pkg/copier/copy_chunk.go)). |
| Copy snapshot | None. The copy is a sequence of per-statement MVCC reads reconciled by the feed and the checksum. | None across chunks, by design: a pass-long snapshot would hold the xmin horizon and block vacuum for the whole copy. The slot's consistent point anchors where the stream begins, not how the copy reads. |
| Default checksum | A pool of `REPEATABLE READ` + `START TRANSACTION WITH CONSISTENT SNAPSHOT` transactions, all opened **while both tables are locked**, then the lock is released and every chunk checks one out and reads source and shadow through it ([trxpool.go](https://github.com/block/spirit/blob/main/pkg/dbconn/trxpool.go#L51-L77), [single.go](https://github.com/block/spirit/blob/main/pkg/checksum/single.go#L301-L347)). So: one snapshot *instant*, N transactions, lock only for the instant they open. | One read-only **`REPEATABLE READ`** transaction **per chunk** takes `ACCESS SHARE` on both relations, then cuts the chunk and reads both digests: the snapshot is taken by the first query after the locks, and `REPEATABLE READ` keeps it for both digest statements, so source and shadow are compared at one instant ([guard.go](../pkg/checksum/guard.go), [verifier.go](../pkg/checksum/verifier.go)). No table lock beyond `ACCESS SHARE`, no snapshot outlives a chunk. |
| Lockless checksum | No transaction, no lock: source and target hashes are independent `READ COMMITTED` reads, explicitly "not aligned with each other" ([lockless.go](https://github.com/block/spirit/blob/main/pkg/checksum/lockless.go#L1698-L1715)); convergence rules replace the snapshot. | `CheckLive` keeps every read aligned — each is the same per-chunk `REPEATABLE READ` snapshot — and adds the convergence rules on top: a differing chunk is read again in a fresh snapshot once the catch-up has confirmed past a WAL marker written in the earlier read's transaction ([live.go](../pkg/checksum/live.go), [settle.go](../pkg/checksum/settle.go)). |
| Repair | Snapshot transaction returned to the pool first; then `DELETE` the chunk range and `INSERT IGNORE … VALUES` from a fresh current source read, in **separate** `READ COMMITTED` transactions ([repair.go](https://github.com/block/spirit/blob/main/pkg/checksum/repair.go#L113-L165)). | **One** guarded read-write transaction for every differing chunk of the pass: delete every chunk's shadow rows, then run the copier's own chunk statement for every chunk, then commit; a `repair` pass then rereads each chunk in a fresh `REPEATABLE READ` snapshot ([repair.go](../pkg/checksum/repair.go)). Under `CheckLive` the catch-up's flushes are held and the shadow is locked `IN SHARE MODE` for the recopy ([live_repair.go](../pkg/checksum/live_repair.go)). |
| Cutover | `LOCK TABLES … WRITE` on the lock-owning session; `RENAME TABLE` is atomic but not transactional. | One transaction holding `ACCESS EXCLUSIVE`; the drain, every rename, and the sequence handoff commit or roll back together. |

## Verification: the snapshot checker, the lockless checker, and pg-sprite's gate

Spirit's default checker, in sequence:

```diagram
 feed      lock session        snapshot pool (N trx)          workers
  |             |                      |                          |
  | flush ------+                      |                          |
  |             | LOCK TABLES src,new WRITE                       |
  | flush under lock                   |                          |
  |             |-- BEGIN RR + CONSISTENT SNAPSHOT xN -->|        |
  |             | UNLOCK TABLES        |                          |
  |             |                      |<-- get trx --------------| per chunk:
  |             |                      |    hash(src range)       |   both reads on
  |             |                      |    hash(new range)       |   the same trx
  |             |                      |-- put trx -------------->|
  |             |                      |                          | mismatch?
  |             |                      |                          |  -> recopy chunk
  |             |                      |                          |     (fresh current
  |             |                      |                          |      read, no snapshot)
  |             |                      |                          |
  |  pass had repairs -> repeat from "flush": new lock, new pool, chunker reset (<= 3)
```

The lock exists to make N connections' snapshots agree on one instant across two tables;
MySQL has no cross-connection snapshot primitive without it. The snapshots then outlive the
lock for the whole pass, which pins undo for as long as the pass runs. It is the same
write-blocking `LOCK TABLES … WRITE` the cutover takes, armed with the same force-kill timer, so
with the default checker Spirit has **two kinds of lock window**: one at the start of every
checksum attempt (up to three per run, and more under `--defer-cutover`, whose wait re-verifies
continuously) and one at cutover. Only the cutover window remains under the lockless checker.
pg-sprite has one by invariant (LK-2).

Spirit's lockless checker drops both the lock and the snapshot: a first mismatch is an
*observation*; after the feed flushes it re-reads, accepts the target if it equals the
previously witnessed *or* the current source signature ("lineage, not simultaneity" — sound
only because the schema change is the shadow's sole writer), splits a range whose source keeps
changing toward a small size, settles single rows against the stream's full after-image as a
last resort, and calls a mismatch *confirmed* only when the source is stable, the feed is
drained past the read, and a final re-read still differs
([lockless.go](https://github.com/block/spirit/blob/main/pkg/checksum/lockless.go#L1-L52)).

pg-sprite's gate ([verifier.go](../pkg/checksum/verifier.go), [live.go](../pkg/checksum/live.go),
[Live verification](copy-and-swap-design.md#live-verification)):

```diagram
 per chunk, one at a time (no table lock beyond ACCESS SHARE, no snapshot pool)
 +--------------------------------------------------------------+
 | BEGIN READ ONLY, REPEATABLE READ                             |
 | SET LOCAL budgets, ROLE owner, search_path                   |
 | LOCK source, shadow IN ACCESS SHARE MODE                     |
 | confirm lock session (LK-1); OID guard (ST-6)                |
 | cut the chunk; digest source range, digest shadow range:     |
 |   count + sha256 over per-row sha256, key order, every       |
 |   column cast to the shadow's type (D7)                      |
 | live pass, chunk differs: write a WAL marker, keep its LSN   |
 | COMMIT                                                       |
 +--------------------------------------------------------------+
   equal  -> chunk clean
   differ -> Check (offline run): a difference -> policy (CO-3)
          -> CheckLive (live run): wait until the catch-up confirms
             past the marker, reread in a fresh snapshot, judge:
               sides equal, or shadow == earlier source -> settled
               source or shadow moved                    -> reread, up to
                                                           Attempts, then
                                                           unresolved (hot)
               neither moved, still differ               -> divergence ->
                                                           policy (CO-3)
               catch-up never passed the marker          -> unresolved
                                                           (lagging)
   clean pass, nothing repaired -> CleanWatermark (CO-2); through the
   complete watermark -> VerifiedShadow, which the cutover gate requires
```

The run always states `abort` ([runner.go](../pkg/schemachange/runner.go),
[runner_live.go](../pkg/schemachange/runner_live.go)): a confirmed divergence stops it in the
`verifying` phase, and an unresolved chunk stops it for the next run to catch up and pass again.
A live proof is accepted at the swap only with a drain that applies the rest of the stream under
the swap's lock (CO-1).

## Repair and re-verification

| Aspect | Spirit (default checker) | pg-sprite |
| --- | --- | --- |
| Repair statement | `DELETE FROM new WHERE <range>` then stream current source rows through the applier as `INSERT IGNORE … VALUES` ([repair.go](https://github.com/block/spirit/blob/main/pkg/checksum/repair.go#L113-L165)); two transactions, serialised by a repair lock. | Every differing chunk at once, in one guarded transaction: all the deletes, then the copier's own chunk statement for each chunk, so a unique value that moved between two differing chunks cannot collide, and a half-repaired shadow is never visible ([repair.go](../pkg/checksum/repair.go)). |
| Repair reads | Current source, not the checksum snapshot. | Current source, in the repair transaction's own snapshot. |
| Is the repaired chunk re-read? | Not in place. The pass is marked dirty; `Run` repeats the **whole table** (new lock, new snapshots, chunker reset) up to three attempts and returns clean only from a pass with zero differences, else `ErrDifferencesExhausted` ([single.go](https://github.com/block/spirit/blob/main/pkg/checksum/single.go#L367-L479)). | Under `repair`, yes: each repaired chunk is reread in a fresh snapshot (under `CheckLive`, settled after the catch-up confirms past the recopy), and one that still differs is a `RepairError`. Even when every reread is clean the pass mints no proof; only a later full pass with no difference does (CO-1, CO-2, [check.go](../pkg/checksum/check.go)). Under `recopy` nothing is reread. Nothing re-runs a pass automatically. |
| Policy | Repair is always on for a schema change (`FixDifferences = true` in the runner). | Stated on every pass, never inferred: `abort`, `repair`, or `recopy` ([policy.go](../pkg/checksum/policy.go), [D14](copy-and-swap-design.md#d14--make-divergence-policy-explicit), CO-3). The run states `abort`; `recopy` is selected only by the reconcile after slot loss, whose recopied chunks the new slot keeps converging ([reconcile.go](../pkg/schemachange/reconcile.go)); `repair` is a library policy no CLI flag selects. |

Both agree on the rule that matters: **a repair never makes a pass clean**. Spirit enforces it
by re-running the pass; pg-sprite enforces it in the constructors of the proof types that the
cutover requires.

## What pg-sprite takes from Spirit's lockless checksum

Spirit is moving its default from the snapshot checker to the lockless one (tracked in its own
repository under `pkg/checksum`). What transfers to pg-sprite, and what does not:

| Spirit mechanism | pg-sprite position |
| --- | --- |
| Table lock to coordinate snapshot start across worker connections | **Not needed.** One PostgreSQL transaction's snapshot covers both relations. The cutover rule stands: no checksum under `ACCESS EXCLUSIVE` (LK-2). |
| Long-lived snapshot transactions pin undo | Same cost class on PostgreSQL (the xmin horizon holds back vacuum), so **short per-chunk snapshots, never a pass-long one**. |
| Mismatch = observed; retry after the feed flushes; confirmed only after a drain | **Ported**, keyed on LSN rather than binlog flush: the read's own transaction writes a WAL marker (`pg_logical_emit_message`), and the chunk is reread once the catch-up has confirmed past it ([marker.go](../pkg/checksum/marker.go), [settle.go](../pkg/checksum/settle.go)). |
| Lineage acceptance (target equals a witnessed source signature) | **Ported**: a reread whose shadow equals the earlier read's source is settled. The sole-writer premise holds: only the copier (`DO NOTHING`) and the applier write the shadow, both under the LK-1 session. |
| Hot-range split toward a small range, bounded attempts | **Bounded attempts ported, split not**: a chunk that keeps moving is reread up to `LiveOptions.Attempts` (default 3) and then reported unresolved, cause `hot`. |
| Row-level settlement against the stream's full after-image (`binlog_row_image=FULL`) | **Not ported.** pgoutput under `REPLICA IDENTITY DEFAULT` omits unchanged TOAST columns (D6, CO-8), so no full after-image is guaranteed; a chunk that cannot settle fails closed as unresolved. |
| Repair does not make the pass clean; zero recopies and zero deferred ranges to cut over | **Already** CO-1 / CO-2, enforced in the `CleanWatermark` / `VerifiedShadow` constructors. |
| Cross-chunk PK-move evasion within one pass | The change event carries the old key, and the applier judges the deletion and the new image against the watermark separately (CO-4); a key moved across a chunk boundary between a chunk's two reads is left to the next pass ([Live verification](copy-and-swap-design.md#live-verification)). |
| `CRC32` XOR + count digest, collision caveat | pg-sprite's digest is the row count plus a SHA-256 over the key-ordered per-row SHA-256s ([digest.go](../pkg/checksum/digest.go), [D7](copy-and-swap-design.md#d7--checksum-through-the-destination-types)): order-sensitive and collision-resistant. SHA-256 rather than `md5` because an OpenSSL in FIPS mode refuses MD5. |
| Ten unresolved passes fail closed | **Ported per chunk**: `LiveOptions.Attempts` and `SettleWait` bound each chunk, and an unsettled chunk is a typed `UnresolvedError` (`ErrVerificationUnresolved`, cause `hot` or `lagging`); the caller decides whether to run the pass again ([live.go](../pkg/checksum/live.go)). |

Net: pg-sprite's checksum was never going to take a write-blocking lock, so "lockless" is the
starting point, not a destination. What transferred is the *verification protocol under live
replication* — position-gated reread, lineage acceptance, bounded attempts, fail closed —
layered on the exact single-snapshot compare the offline path already needs.

## Summary of the departures

- **Classify, don't fall through.** Spirit tries the native DDL and falls back to the copy;
  pg-sprite routes from the grammar, and its one blind attempt runs under budgets and is
  refused, not copied, when it does real work.
- **Two modes.** A live run streams through a slot; an offline run fences writes with a trigger
  and decodes nothing. Spirit always streams.
- **Copy on the server.** Spirit moves rows through the client in `INSERT IGNORE … VALUES`
  chunklets; pg-sprite runs one guarded `INSERT … SELECT … ON CONFLICT DO NOTHING` per chunk.
- **No snapshot pool, no table lock for the checksum.** One `REPEATABLE READ` transaction per
  chunk sees both relations at one instant.
- **Abort, not self-heal, inside a run.** Spirit repairs and re-runs the whole pass up to three
  times; pg-sprite's run stops on a confirmed divergence, and a repair pass — available to
  library callers and used after slot loss — mints no proof, so only a later clean pass can.
- **Transactional cutover.** The drain, the renames, the name repairs, and the sequence handoff
  commit together; a `lock_timeout` retry replaces the force-kill of blockers.
- **The slot is a first-class liability.** Its ownership and its lag ceiling are invariants,
  not tidy-ups, because an orphaned slot pins WAL on the primary.
- **TOAST-omitted columns** are a PostgreSQL-only hazard the applier has to respect (D6, CO-8);
  nothing in Spirit corresponds.
