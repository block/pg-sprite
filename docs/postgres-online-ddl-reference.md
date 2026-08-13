# Aurora PostgreSQL online DDL operations reference

The PostgreSQL equivalent of MySQL's
[InnoDB Online DDL Operations](https://dev.mysql.com/doc/refman/8.4/en/innodb-online-ddl-operations.html).

PostgreSQL has **no** `ALGORITHM={INSTANT,INPLACE,COPY}` knob. What matters instead is:

1. **Which lock level** the DDL acquires, and
2. **Whether it rewrites the table** (or does a full scan), and therefore how long it holds
   that lock.

These are exactly the two dimensions MySQL lets authors *assert* with `ALGORITHM=` and
`LOCK=` (failing closed when either can't be honored). PostgreSQL offers no such clause —
pg-sprite's `diff` and `migrate --dry-run` are that missing declaration today: they classify
and route the change. Routed execution lands with the Phase 3 executor.

The rest of this document breaks down both dimensions per operation. 

## Table of contents

- [Lock levels (weakest → strongest)](#lock-levels-weakest--strongest)
- [Why DDL is dangerous: the lock queue](#why-ddl-is-dangerous-the-lock-queue)
- [Column operations](#column-operations)
- [Index operations](#index-operations)
- [Constraint operations](#constraint-operations)
- [Table / partition operations](#table--partition-operations)
- [The headline takeaway (what the engine must cover)](#the-headline-takeaway-what-the-engine-must-cover)
- [Aurora PostgreSQL specifics](#aurora-postgresql-specifics)

## Lock levels (weakest → strongest)

```
ACCESS SHARE  <  ROW SHARE  <  ROW EXCLUSIVE  <  SHARE UPDATE EXCLUSIVE
   <  SHARE  <  SHARE ROW EXCLUSIVE  <  EXCLUSIVE  <  ACCESS EXCLUSIVE
```

- `SELECT` takes `ACCESS SHARE`.
- `INSERT/UPDATE/DELETE` take `ROW EXCLUSIVE`.
- **Reads and writes continue concurrently** as long as the DDL holds
  `≤ SHARE UPDATE EXCLUSIVE`. That is the "online" threshold.
- `ACCESS EXCLUSIVE` blocks **everything**, including reads.

### Comparison with MySQL / InnoDB

The PostgreSQL lock-mode → MySQL 8.0 (MDL + InnoDB) mapping, the `ALGORITHM=`/`LOCK=` contrast,
and the brief-exclusive-lock parallel now live in the dedicated comparison doc:
**12-mysql-vs-postgresql.md § Lock model comparison**.

## Why DDL is dangerous: the lock queue

This is covered in the comparison reference:
**12-mysql-vs-postgresql.md § Why DDL is dangerous: the lock queue**.
It explains the three-step lock-queue pile-up, why the DDL (not the long query) is the
catalyst, how long the impact lasts, the mitigations the engine relies on, and why MySQL has
the same dynamic via metadata locks. Read it first if any of that is unfamiliar.

## Column operations

| Operation | Lock held | Table rewrite | Concurrent DML | Needs copy-and-swap? | Notes |
| --- | --- | --- | --- | --- | --- |
| `ADD COLUMN` (no default / nullable) | ACCESS EXCLUSIVE (brief) | No | Yes (after lock) | ❌ No | Metadata only |
| `ADD COLUMN ... DEFAULT <constant>` | ACCESS EXCLUSIVE (brief) | **No** (PG 11+) | Yes | ❌ No | "Fast default" stored in catalog; pre-PG11 this rewrote |
| `ADD COLUMN ... DEFAULT <volatile>` (e.g. `now()`, `random()`, `uuid_generate_v4()`) | ACCESS EXCLUSIVE | **Yes** (full rewrite) | No | ✅ **Yes** | The expensive case |
| `ADD COLUMN ... GENERATED ALWAYS AS (...) STORED` | ACCESS EXCLUSIVE | Yes | No | ✅ **Yes** | Values must be computed |
| `ADD COLUMN ... UNIQUE` / `PRIMARY KEY` / `REFERENCES` / `CHECK` (inline constraint) | ACCESS EXCLUSIVE + index build or validation | No | No | ➖ Native pattern | Same work as the `ADD CONSTRAINT` form, under the `ADD COLUMN` lock — split: add the column first, then build the constraint online (`CONCURRENTLY` + `USING INDEX`, or `NOT VALID` + `VALIDATE`) |
| `DROP COLUMN` | ACCESS EXCLUSIVE (brief) | No | Yes | ❌ No | Metadata only; disk space reclaimed lazily by VACUUM |
| `ALTER COLUMN TYPE` — binary-coercible (`varchar(50)→varchar(100)`, `varchar→text`, `numeric(10,2)→numeric(12,2)`) | ACCESS EXCLUSIVE (brief) | **No** | No (brief) | ❌ No | No scan when binary-coercible and no length restriction is added |
| `ALTER COLUMN TYPE` — general (`int→bigint`, `text→jsonb`, `timestamp→timestamptz` w/ conversion) | ACCESS EXCLUSIVE | **Yes** (rewrite + reindex + revalidate FKs) | No | ✅ **Yes** | The classic "needs a tool" case |
| `ALTER COLUMN SET DEFAULT` / `DROP DEFAULT` | ACCESS EXCLUSIVE (brief) | No | Yes | ❌ No | Metadata only |
| `ALTER COLUMN SET NOT NULL` | ACCESS EXCLUSIVE | No, but **full scan** | No | ➖ Native pattern | Use `NOT VALID` `CHECK` + `VALIDATE` (PG 12+) to skip the blocking scan |
| `ALTER COLUMN DROP NOT NULL` | ACCESS EXCLUSIVE (brief) | No | Yes | ❌ No | Metadata only |
| `RENAME COLUMN` | ACCESS EXCLUSIVE (brief) | No | Yes | ❌ No | Metadata only |
| `ALTER COLUMN SET STATISTICS` / `SET STORAGE` / `SET (n_distinct=...)` | SHARE UPDATE EXCLUSIVE / ACCESS EXCLUSIVE | No | varies | ❌ No | Metadata only |

**Legend for "Needs copy-and-swap?":** 
- ✅ **Yes** = genuine table rewrite with no native
online path, so the heavy **shadow-copy + atomic cutover** path is required · 
- ➖ Native pattern = no copy-and-swap needed; a safe native sequence does it (`CONCURRENTLY` /
`NOT VALID`+`VALIDATE` / `USING INDEX` / fast-default)
- ❌ No = metadata-only or already online, just guard with `lock_timeout`.

> **`Needs copy-and-swap? = No` does not mean "don't use the engine".** The engine still
> helps on the ➖ and ❌ rows — it classifies the change and shows the correct *native*
> sequence; automated execution of routed sequences lands with the Phase 3 executor. Only the
> ✅ rows force a full copy. See
> 03-why-build-this-engine.md § The engine helps on every change, not just rewrites.

> GitHub-rendered markdown tables are not interactively sortable (no client-side JS). The
> two collapsible views below are the same column operations **pre-sorted** by `Table
> rewrite` and by `Concurrent DML`. The `↓` marks the column each view is sorted on.

<details>
<summary><b>Sorted by Table rewrite</b> (No → No (full scan) → Yes)</summary>

| Operation | Table rewrite ↓ | Concurrent DML | Needs copy-and-swap? | Lock held |
| --- | --- | --- | --- | --- |
| `ADD COLUMN` (no default / nullable) | No | Yes | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ADD COLUMN ... DEFAULT <constant>` | No (PG 11+) | Yes | ❌ No | ACCESS EXCLUSIVE (brief) |
| `DROP COLUMN` | No | Yes | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN SET DEFAULT` / `DROP DEFAULT` | No | Yes | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN DROP NOT NULL` | No | Yes | ❌ No | ACCESS EXCLUSIVE (brief) |
| `RENAME COLUMN` | No | Yes | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN SET STATISTICS` / `SET STORAGE` / `SET (n_distinct=...)` | No | varies | ❌ No | SHARE UPDATE EXCLUSIVE / ACCESS EXCLUSIVE |
| `ALTER COLUMN TYPE` — binary-coercible | No | No (brief) | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN SET NOT NULL` | No, but full scan | No | ➖ Native pattern | ACCESS EXCLUSIVE |
| `ADD COLUMN ...` (inline constraint) | No, but index build / validation | No | ➖ Native pattern | ACCESS EXCLUSIVE |
| `ADD COLUMN ... DEFAULT <volatile>` | Yes | No | ✅ **Yes** | ACCESS EXCLUSIVE |
| `ADD COLUMN ... GENERATED ALWAYS AS (...) STORED` | Yes | No | ✅ **Yes** | ACCESS EXCLUSIVE |
| `ALTER COLUMN TYPE` — general | Yes | No | ✅ **Yes** | ACCESS EXCLUSIVE |

</details>

<details>
<summary><b>Sorted by Concurrent DML</b> (Yes → varies → No)</summary>

| Operation | Concurrent DML ↓ | Table rewrite | Needs copy-and-swap? | Lock held |
| --- | --- | --- | --- | --- |
| `ADD COLUMN` (no default / nullable) | Yes | No | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ADD COLUMN ... DEFAULT <constant>` | Yes | No (PG 11+) | ❌ No | ACCESS EXCLUSIVE (brief) |
| `DROP COLUMN` | Yes | No | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN SET DEFAULT` / `DROP DEFAULT` | Yes | No | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN DROP NOT NULL` | Yes | No | ❌ No | ACCESS EXCLUSIVE (brief) |
| `RENAME COLUMN` | Yes | No | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN SET STATISTICS` / `SET STORAGE` / `SET (n_distinct=...)` | varies | No | ❌ No | SHARE UPDATE EXCLUSIVE / ACCESS EXCLUSIVE |
| `ALTER COLUMN TYPE` — binary-coercible | No (brief) | No | ❌ No | ACCESS EXCLUSIVE (brief) |
| `ALTER COLUMN SET NOT NULL` | No | No, but full scan | ➖ Native pattern | ACCESS EXCLUSIVE |
| `ADD COLUMN ...` (inline constraint) | No | No, but index build / validation | ➖ Native pattern | ACCESS EXCLUSIVE |
| `ADD COLUMN ... DEFAULT <volatile>` | No | Yes | ✅ **Yes** | ACCESS EXCLUSIVE |
| `ADD COLUMN ... GENERATED ALWAYS AS (...) STORED` | No | Yes | ✅ **Yes** | ACCESS EXCLUSIVE |
| `ALTER COLUMN TYPE` — general | No | Yes | ✅ **Yes** | ACCESS EXCLUSIVE |

</details>

### Safe pattern: `SET NOT NULL` without a blocking scan (PG 12+)

```sql
-- 1. brief lock, no scan
ALTER TABLE t ADD CONSTRAINT t_col_nn CHECK (col IS NOT NULL) NOT VALID;
-- 2. SHARE UPDATE EXCLUSIVE, scans but allows reads+writes
ALTER TABLE t VALIDATE CONSTRAINT t_col_nn;
-- 3. now this is cheap (PG recognises the validated CHECK)
ALTER TABLE t ALTER COLUMN col SET NOT NULL;
ALTER TABLE t DROP CONSTRAINT t_col_nn;  -- optional cleanup
```

## Index operations

Index operations never rewrite the **heap** (the table's row data); their cost is an index
build / rebuild and the table scan(s) it requires. The "Table rewrite" column is therefore
uniformly "No" — included for consistency with the other tables.

| Operation | Lock held | Table rewrite | Concurrent DML | Needs copy-and-swap? | Notes |
| --- | --- | --- | --- | --- | --- |
| `CREATE INDEX` | SHARE (blocks writes) | No (builds index) | **No** | ➖ Use `CONCURRENTLY` | Do not use on hot tables |
| `CREATE INDEX CONCURRENTLY` | SHARE UPDATE EXCLUSIVE | No (builds index) | **Yes** | ❌ No | Two table scans; slower; **not transactional** — a failure can leave an `INVALID` index that must be dropped and rebuilt |
| `DROP INDEX` | ACCESS EXCLUSIVE (brief) | No | No | ➖ Use `CONCURRENTLY` | |
| `DROP INDEX CONCURRENTLY` | SHARE UPDATE EXCLUSIVE | No | Yes | ❌ No | PG 9.2+ |
| `REINDEX INDEX` | ACCESS EXCLUSIVE | No (rebuilds index) | No | ➖ Use `CONCURRENTLY` | |
| `REINDEX INDEX CONCURRENTLY` | SHARE UPDATE EXCLUSIVE | No (rebuilds index) | Yes | ❌ No | PG 12+ |
| `ALTER INDEX ... RENAME` | SHARE UPDATE EXCLUSIVE | No | Yes | ❌ No | |

## Constraint operations

Constraint operations never rewrite the **heap** either; the heavy cost is the **validation
scan** (or, for a directly-added PK/UNIQUE, an index build). "Table rewrite" is therefore
uniformly "No"; the "Validation scan" column captures the part that actually costs time.

| Operation | Lock held | Table rewrite | Validation scan | Concurrent DML | Needs copy-and-swap? | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| `ADD PRIMARY KEY` / `ADD UNIQUE` (direct) | ACCESS EXCLUSIVE | No | Index build (blocks) | No | ➖ Use `USING INDEX` | Avoid; build the index concurrently first |
| `ADD PRIMARY KEY / UNIQUE USING INDEX <idx>` | ACCESS EXCLUSIVE (brief) | No | No | No (brief) | ❌ No | Pattern: `CREATE UNIQUE INDEX CONCURRENTLY` then attach |
| `ADD CHECK` / `ADD FOREIGN KEY` (default) | ACCESS EXCLUSIVE | No | **Full scan (blocks)** | No | ➖ Use `NOT VALID` | Blocks for the whole scan |
| `ADD CHECK / FOREIGN KEY ... NOT VALID` | ACCESS EXCLUSIVE (brief) | No | No | Yes | ❌ No | First step of the safe pattern |
| `VALIDATE CONSTRAINT` | SHARE UPDATE EXCLUSIVE | No | Scan (non-blocking) | **Yes** | ❌ No | Safe second step |
| `DROP CONSTRAINT` | ACCESS EXCLUSIVE (brief) | No | No | Yes | ❌ No | |

### Safe pattern: add a foreign key / check without a blocking scan

```sql
ALTER TABLE child ADD CONSTRAINT fk FOREIGN KEY (pid) REFERENCES parent (id) NOT VALID;  -- brief lock
ALTER TABLE child VALIDATE CONSTRAINT fk;  -- SHARE UPDATE EXCLUSIVE, online
```

### Safe pattern: add a primary key / unique constraint online

```sql
CREATE UNIQUE INDEX CONCURRENTLY t_pkey ON t (id);   -- online build
ALTER TABLE t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey;  -- brief lock
```

## Table / partition operations

| Operation | Lock held | Table rewrite | Concurrent DML | Needs copy-and-swap? | Notes |
| --- | --- | --- | --- | --- | --- |
| `RENAME TABLE` | ACCESS EXCLUSIVE (brief) | No | Yes | ❌ No | Metadata only — basis for cutover swap |
| `SET SCHEMA` | ACCESS EXCLUSIVE (brief) | No | Yes | ❌ No | |
| `SET TABLESPACE` | ACCESS EXCLUSIVE | **Yes** (moves heap) | No | ✅ **Yes** (repack-style) | Rewrite/move; use a repack-style copy instead |
| `SET (fillfactor=...)` and most reloptions | SHARE UPDATE EXCLUSIVE | No | Yes | ❌ No | Applies to new rows |
| `CLUSTER` / `VACUUM FULL` | ACCESS EXCLUSIVE | **Yes** (full rewrite) | No | ✅ **Yes** (`pg_repack`) | Use `pg_repack` |
| `CREATE TABLE ... PARTITION OF` | ACCESS EXCLUSIVE on **parent** (brief) | No | Blocked on parent while held | ❌ No | Brief and no scan, but it queues behind long-running queries and then blocks every reader of the parent |
| `ATTACH PARTITION` | SHARE UPDATE EXCLUSIVE on parent + scan of child | No | Yes | ➖ Native pattern | Add a validated `CHECK` matching the bound on the child first to skip the scan |
| `DETACH PARTITION` | ACCESS EXCLUSIVE | No | No | ➖ Use `CONCURRENTLY` | |
| `DETACH PARTITION CONCURRENTLY` | SHARE UPDATE EXCLUSIVE | No | Yes | ❌ No | PG 14+ |

## The headline takeaway (what the engine must cover)

The operations that genuinely need a **log-based copy-and-swap** (shadow-table copy + atomic
cutover) are the **table-rewrite** ones, because PostgreSQL cannot do them online natively:

- general `ALTER COLUMN TYPE` (`int→bigint`, `text→jsonb`, etc.)
- `ADD COLUMN` with a **volatile** default
- adding a `STORED` generated column
- column reordering / table repack (bloat removal)
- changing a column to/from `NOT NULL` on very large tables where even the scan is too long

**Everything else can be done natively-safe** with PostgreSQL's own
`CONCURRENTLY` / `NOT VALID`+`VALIDATE` / fast-default / `USING INDEX` patterns. By the
[*classify-before-copy* principle](design-principles.md#classify-first-leverage-native-postgresql),
the plan **detects those and routes them to native DDL** instead of a copy, and `migrate`
executes the routed sequence — the same bypass Spirit applies when it attempts `INSTANT`/`INPLACE`
before falling back to a table copy. A shadow-table copy is the last resort, used only when no
native online path exists.

## Aurora PostgreSQL specifics

- **Lock behaviour is identical** to community PostgreSQL — Aurora uses the same query
  engine; only storage/replication differs.
- **Reader instances** serve `ACCESS EXCLUSIVE`-blocked reads from the same WAL stream, so
  a blocking DDL on the writer still stalls readers of that table.
- **`CREATE INDEX CONCURRENTLY` interacts with long-running reader transactions**: it waits
  for transactions that can see the table to finish, including on Aurora replicas. A
  long analytics query on a reader can stall a concurrent index build on the writer.
- For the engine's CDC path, Aurora supports **logical replication / logical decoding**, but
  it must be enabled via the `rds.logical_replication=1` cluster parameter (static →
  requires a reboot) which sets `wal_level=logical`. See
  [low-level-design.md](low-level-design.md).
