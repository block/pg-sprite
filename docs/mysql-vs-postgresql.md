# MySQL vs PostgreSQL: the comparison reference

One place for every MySQL ↔ PostgreSQL comparison pg-sprite relies on: how each engine
expresses online DDL, how their lock models map, **why DDL is dangerous (the lock-queue
pile-up — the same failure mode in both engines)**, and how
[Spirit](https://github.com/block/spirit)'s MySQL primitives translate to the PostgreSQL
copy-and-swap executor. The other docs link here instead of repeating it. If you are new to
why an apparently instant `ALTER TABLE` can take an app down, start with
[Why DDL is dangerous: the lock queue](#why-ddl-is-dangerous-the-lock-queue).

## Table of contents

- [How online DDL is expressed](#how-online-ddl-is-expressed)
- [Lock model comparison](#lock-model-comparison)
- [Why DDL is dangerous: the lock queue](#why-ddl-is-dangerous-the-lock-queue)
  - [The mechanism: a three-step pile-up](#the-mechanism-a-three-step-pile-up)
  - [The DDL is the catalyst, not the long query](#the-ddl-is-the-catalyst-not-the-long-query)
  - [How long does the impact last?](#how-long-does-the-impact-last)
  - [Mitigations the engine and operators rely on](#mitigations-the-engine-and-operators-rely-on)
  - [The lock queue is the same in both engines](#the-lock-queue-is-the-same-in-both-engines)
- [Copy-and-swap executor: Spirit (MySQL) → PostgreSQL primitive mapping](#copy-and-swap-executor-spirit-mysql--postgresql-primitive-mapping)

## How online DDL is expressed

The two engines reach "online schema change" by **different routes**, which is why a tool
designed for one does not port mechanically to the other:

| Aspect | MySQL 8.0 / InnoDB | PostgreSQL |
| --- | --- | --- |
| How you ask for online behaviour | Explicit `ALTER … ALGORITHM={INSTANT\|INPLACE\|COPY}, LOCK={NONE\|SHARED\|EXCLUSIVE}` | **No such knob.** Each DDL has a *fixed* lock level and rewrite behaviour you cannot lower |
| What "online" means | The server runs the change in place and lets you request `LOCK=NONE` | Some operations are inherently online (metadata-only, `CONCURRENTLY`, `NOT VALID`); others always take `ACCESS EXCLUSIVE` and/or rewrite |
| How much work the change does | Three server-asserted states: `INSTANT` (metadata only), `INPLACE` (rebuilt in place, often a full scan), `COPY` (full table copy) | The same three buckets exist — catalog-only, full scan without rewrite, full rewrite — but nothing asserts them; you infer them per operation. See [the three buckets](postgres-online-ddl-reference.md#the-three-buckets-what-mysqls-algorithm-states-map-to) |
| Avoiding a rewrite | `ALGORITHM=INSTANT`/`INPLACE` | Use the **native-safe pattern** (`CONCURRENTLY`, `NOT VALID`+`VALIDATE`, fast default, `ADD PK USING INDEX`, [binary-coercible type change](binary-coercible-type-changes.md)) |
| When neither is possible | `ALGORITHM=COPY` (server-side table rebuild) or an OSC tool (gh-ost / Spirit) | An **OSC tool** (pg_osc, pg_repack, or pg-sprite's copy-and-swap executor) |
| Where the engine decides | Spirit attempts INSTANT/INPLACE, else copies | pg-sprite **classifies** the change → native-safe, copy-and-swap, or refuse (see [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md)) |

The practical upshot: in MySQL the *server* exposes the online machinery and you opt into it; in
PostgreSQL the online machinery is a **set of per-operation idioms**, and the value of an engine
is knowing which idiom applies (or that a copy is unavoidable).

## Lock model comparison

The two engines model locking differently, so the mapping is by **what concurrent access is
allowed**, not a 1:1 equivalence:

- **PostgreSQL** has a single explicit **table-lock hierarchy** (8 modes). Each DDL statement
  acquires a *fixed* mode you cannot lower — you can only bound how long it *waits* with
  `lock_timeout`. (The full weakest→strongest list lives in
  [postgres-online-ddl-reference.md](postgres-online-ddl-reference.md#lock-levels-weakest--strongest).)
- **MySQL 8.0** splits the concern: **metadata locks (MDL)** protect the schema, **InnoDB row
  locks** protect DML, and for online DDL you *request* the concurrency you want via the
  `ALGORITHM=` / `LOCK=` clause (`LOCK=NONE | SHARED | EXCLUSIVE`).

| PostgreSQL lock mode | Typically acquired by | Closest MySQL 8.0 concept | Concurrent access |
| --- | --- | --- | --- |
| `ACCESS SHARE` | `SELECT` | `MDL_SHARED_READ` held by a query | reads |
| `ROW SHARE` | `SELECT ... FOR UPDATE/SHARE` | MDL read + InnoDB shared row locks | reads + locking reads |
| `ROW EXCLUSIVE` | `INSERT` / `UPDATE` / `DELETE` | `MDL_SHARED_WRITE` + InnoDB `IX`/row locks | reads + writes |
| `SHARE UPDATE EXCLUSIVE` | `CREATE INDEX CONCURRENTLY`, `VALIDATE CONSTRAINT`, `VACUUM`, `ANALYZE`, online `ALTER`s | online DDL `ALGORITHM=INPLACE/INSTANT, LOCK=NONE` | **reads + writes** (the "online" threshold) |
| `SHARE` | `CREATE INDEX` (non-concurrent) | online DDL `LOCK=SHARED` | reads only (writes blocked) |
| `SHARE ROW EXCLUSIVE` | `CREATE TRIGGER`, some `ALTER`s | `LOCK=SHARED` (no exact analog) | reads only |
| `EXCLUSIVE` | `REFRESH MATERIALIZED VIEW CONCURRENTLY` | between `LOCK=SHARED` and `LOCK=EXCLUSIVE` | plain reads only (no locking reads/writes) |
| `ACCESS EXCLUSIVE` | most `ALTER TABLE`, `DROP`, `TRUNCATE`, `REINDEX`, `VACUUM FULL`, the cutover swap | online DDL `LOCK=EXCLUSIVE` / the table lock of `ALGORITHM=COPY` | **nothing** (reads + writes blocked) |

**Shared parallel worth noting:** even a fully "online" change needs a *brief* exclusive lock
at the metadata boundaries in both engines — PostgreSQL takes `ACCESS EXCLUSIVE` momentarily to
publish the catalog change, and MySQL takes a brief exclusive MDL at the start/end of a
`LOCK=NONE` operation. In both, that brief exclusive lock is what can still get stuck behind a
long-running transaction.

## Why DDL is dangerous: the lock queue

This is the prerequisite background for the whole design set: *why* an apparently instant
`ALTER TABLE` can still take an application down, and why online-schema-change tools exist at
all on both engines. It lives here because the failure mode and its mitigations are best
understood as a direct consequence of the [lock model comparison](#lock-model-comparison)
above. The per-operation lock/rewrite details are in
[postgres-online-ddl-reference.md](postgres-online-ddl-reference.md); the engine that
works around this is in [low-level-design.md](low-level-design.md).

### The mechanism: a three-step pile-up

The risk in PostgreSQL DDL is usually not the DDL's own work — it is the **lock queue**. The
mechanism has three steps:

1. An `ALTER TABLE` needs `ACCESS EXCLUSIVE`, which conflicts with the `ACCESS SHARE` that
   every `SELECT` already holds. So the `ALTER` cannot start until the in-flight queries on
   that table finish; it **waits**.
2. PostgreSQL queues lock requests roughly in **arrival order** — the order in which each
   statement requests the lock — and it does not let a later request jump ahead of an
   already-waiting one, even if the later request would be compatible with the current holder.
   So once the `ALTER` is waiting, **every statement that requests the lock after it lines up
   behind it** — including plain `SELECT`s that would otherwise run fine alongside the
   in-flight queries.
3. Each blocked query keeps holding its client connection. If enough pile up, they exhaust the
   connection pool / `max_connections`, at which point **even queries on unrelated tables**
   can't get a connection. That is how a single table's lock contention becomes an app-wide
   outage.

```
time ──▶
  long query (holds ACCESS SHARE) ═══════════════════════════════╗ still running
  ALTER TABLE (wants ACCESS EXCLUSIVE)          ░░░░░░░░░░░░░░░░░░░║ waiting at head of queue
  later SELECTs (want ACCESS SHARE)                 ░░░░░░░░░░░░░░░║ blocked behind the ALTER
                                                                  └─ backlog grows; pool may exhaust
```

### The DDL is the catalyst, not the long query

Without the `ALTER`, the long-running `SELECT` would **not** block the later `SELECT`s at all:
they all take `ACCESS SHARE`, which is compatible with itself, so any number of readers run
concurrently regardless of how long one of them takes (`ACCESS SHARE` only conflicts with
`ACCESS EXCLUSIVE`). The pile-up exists purely because the `ALTER`'s pending `ACCESS EXCLUSIVE`
request sits in the queue and the later readers will not jump ahead of it. Remove the DDL and
there is no queue.

### How long does the impact last?

It is **not** a fixed number — it is roughly *"how long the blocking transaction keeps
running"* plus the time to drain the backlog afterward. The `ALTER` clears the instant the
conflicting transaction(s) commit/abort and it acquires the lock (then its own work is
milliseconds for a metadata-only change). So the worst cases are driven by **how long
something holds a conflicting lock**:

- a long analytics `SELECT` → impact lasts about as long as that query runs;
- an **idle-in-transaction** session that `SELECT`ed the table and never committed → impact
  lasts until that session is closed or `idle_in_transaction_session_timeout` fires, which
  can be effectively unbounded;
- without `lock_timeout`, the `ALTER` itself waits indefinitely, so the backlog keeps growing
  the whole time.

The takeaway is the *causal chain*, not a specific duration: a metadata-only `ALTER` can
amplify one slow or stuck transaction into widespread blocking.

### Mitigations the engine and operators rely on

- **Always set `lock_timeout`** (e.g. `SET lock_timeout = '3s'`) before DDL on a hot table,
  so the `ALTER` gives up quickly instead of sitting at the head of the queue and growing a
  backlog. Retry with backoff rather than waiting indefinitely.
- Keep `ACCESS EXCLUSIVE` windows as short as possible — this is exactly why the cutover swap
  is the only `ACCESS EXCLUSIVE` step in the engine's design.
- Avoid running blocking DDL while long analytics queries or idle-in-transaction sessions hold
  locks on the table; consider `idle_in_transaction_session_timeout` to bound the worst case.

### The lock queue is the same in both engines

MySQL has the **same dynamic**, via **metadata locks (MDL)** rather than table locks. A DDL
needs an exclusive MDL; it waits behind any open transaction still holding a shared MDL on the
table (the familiar `Waiting for table metadata lock` state), and subsequent queries queue
behind the waiting DDL — same three-step pile-up. The MySQL equivalents of the mitigations are
`lock_wait_timeout` (bound how long the DDL waits) and avoiding long/abandoned transactions.

This is precisely why online-schema-change tools exist on **both** engines and why their
**cutover** is the delicate step: Spirit/gh-ost take a brief, bounded `RENAME TABLE` under a
metadata lock; pg-sprite takes a brief, bounded `ACCESS EXCLUSIVE` swap. The whole point of
the copy-and-swap approach is to replace one long lock-holding `ALTER` with a short, retryable
locked window.

## Copy-and-swap executor: Spirit (MySQL) → PostgreSQL primitive mapping

Spirit's copy-and-swap lifecycle maps cleanly onto PostgreSQL, but **every database-specific
primitive must be swapped**. This is the per-primitive translation the
[copy-and-swap executor](low-level-design.md#copy-and-swap-executor-lifecycle) is built on
(see the [Spirit repository](https://github.com/block/spirit) for how the MySQL original
works). Rows marked *Aurora* name the managed-service signal; the vanilla-PostgreSQL
equivalent is given alongside.

| Spirit (MySQL) | PostgreSQL equivalent |
| --- | --- |
| Binlog (`go-mysql` `BinlogSyncer`) | **Logical decoding** via a replication slot (`pgoutput` / `wal2json` / `test_decoding`) — the faithful, low-overhead analog of the binlog. Trigger-based capture is the fallback. |
| `binlog_row_image=FULL` | `REPLICA IDENTITY FULL` (or default = PK) on the source table so updates/deletes carry enough identity |
| `SHOW BINARY LOG STATUS` → file:offset position | LSN + slot `confirmed_flush_lsn` |
| `REPLACE INTO target VALUES (...)` (apply) | `INSERT ... ON CONFLICT (pk) DO UPDATE SET ...` + explicit delete handling |
| `INSERT IGNORE ... SELECT` (copy) | `INSERT INTO shadow SELECT ... FROM src WHERE <pk range> ON CONFLICT DO NOTHING` |
| `CRC32(CONCAT(col,...))` checksum | `md5(row::text)` aggregated per chunk, or `sum(hashtext(...))` / count compare |
| `RENAME TABLE old→_old, new→old` under `LOCK TABLES` (needs MySQL 8.0.13+) | `BEGIN; LOCK TABLE src IN ACCESS EXCLUSIVE MODE; <final drain>; ALTER TABLE src RENAME TO src_old; ALTER TABLE shadow RENAME TO src; COMMIT;` — **PostgreSQL's transactional DDL makes this cleaner than MySQL** |
| Force-kill via `performance_schema` | `pg_terminate_backend()` + `lock_timeout`/`statement_timeout` to bound the cutover wait |
| TiDB SQL parser (`pkg/statement`) | [`wasilibs/go-pgquery`](https://github.com/wasilibs/go-pgquery) (libpg_query compiled to Wasm — the real PostgreSQL grammar, no cgo) for parsing `ALTER` / `CREATE TABLE` |
| Aurora MySQL throttling (active threads, replica lag) | Replication **slot lag** (`pg_replication_slots`), WAL generation rate, replica lag (`pg_stat_replication`; *Aurora:* `aurora_replica_status()`, CloudWatch) |
| `AUTO_INCREMENT` optimistic chunker | `bigint`/`identity`/`serial` PK range chunker; composite-PK chunker otherwise |
| TLS / RDS CA auto-detection | same idea, the RDS/Aurora CA bundle for `pgx` when the target is a managed service |
