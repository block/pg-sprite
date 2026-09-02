# PostgreSQL online DDL operations reference

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

## The three buckets: what MySQL's `ALGORITHM` states map to

MySQL 8.0 asserts one of three work-done states per `ALTER`: `INSTANT` (metadata only),
`INPLACE` (rebuilt in place — usually a full scan, sometimes a rebuild — while DML continues),
and `COPY` (a full table copy). In practice MySQL tooling treats this as binary — INSTANT, or
a copy — because INPLACE on a large table costs about what COPY does.

PostgreSQL has the same three states; it just does not name them. Crossing the two axes
above (lock × work done) gives exactly three buckets every DDL in this reference falls into:

| Bucket | Work done | Lock | MySQL analog | Examples | pg-sprite route |
| --- | --- | --- | --- | --- | --- |
| **Catalog-only** | none — a catalog entry changes; no row is read or written | brief `ACCESS EXCLUSIVE` (milliseconds once acquired) | `ALGORITHM=INSTANT` | `ADD COLUMN` (nullable or constant default), `DROP COLUMN`, `SET/DROP DEFAULT`, `DROP NOT NULL`, renames, [binary-coercible type changes](binary-coercible-type-changes.md) | native, as written |
| **Full scan, no rewrite** | every row is *read* (to validate or to build an index); the heap is not rewritten | native online forms hold `SHARE UPDATE EXCLUSIVE` for the scan; the as-written forms hold a blocking lock for the whole scan | `ALGORITHM=INPLACE` | `SET NOT NULL`, `ADD CONSTRAINT … CHECK/FOREIGN KEY`, `ADD CONSTRAINT … UNIQUE`, `CREATE INDEX` | native, **safer sequence** substituted (`NOT VALID` + `VALIDATE`, `CONCURRENTLY` + `USING INDEX`) |
| **Full rewrite** | every row is *written* into a new heap; all indexes rebuilt | `ACCESS EXCLUSIVE` for the whole rewrite | `ALGORITHM=COPY` | `ALTER COLUMN TYPE` (non-coercible), `ADD COLUMN … DEFAULT <volatile>`, `ADD COLUMN … GENERATED … STORED`, `SET TABLESPACE`, `VACUUM FULL`/`CLUSTER` | **copy-and-swap** (typed refusal until the executor lands) |

Two things fall out of this. For a coarse "does this cost scale with table size?" question,
the last two buckets answer alike — only the first is free. For deciding *how* to run the
change, the split between the second and third bucket is what matters: the second bucket
nearly always has a native online form that keeps DML flowing (the lock, not the scan, was
the problem — exclusion constraints are the notable exception); the third has none, and only
a copy-and-swap avoids the long exclusive lock. The
plan report's `decisions[].reason` vocabulary
([plan-report.md](plan-report.md#planner-decision-reasons-decisionsreason)) is this table
made machine-readable: `metadata-only` / `fast-default` / `binary-coercible` are the first
bucket, `safer-idiom` the second, `type-rewrite` / `volatile-default` / `generated-stored` /
`relocation` the third.

The MySQL-side detail — how `ALGORITHM=` and `LOCK=` are asserted and how the two engines'
lock models line up — is in
[mysql-vs-postgresql.md](mysql-vs-postgresql.md#how-online-ddl-is-expressed).

## Table of contents

- [The three buckets: what MySQL's `ALGORITHM` states map to](#the-three-buckets-what-mysqls-algorithm-states-map-to)
- [Lock levels (weakest → strongest)](#lock-levels-weakest--strongest)
- [Why DDL is dangerous: the lock queue](#why-ddl-is-dangerous-the-lock-queue)
- [Column operations](#column-operations)
- [Index operations](#index-operations)
- [Constraint operations](#constraint-operations)
- [Table / partition operations](#table--partition-operations)
- [The headline takeaway (what the engine must cover)](#the-headline-takeaway-what-the-engine-must-cover)
- [Aurora PostgreSQL specifics](#aurora-postgresql-specifics)
- [Dry-run diagnostic codes](#dry-run-diagnostic-codes)

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
and the brief-exclusive-lock parallel live in the dedicated comparison doc:
[mysql-vs-postgresql.md § Lock model comparison](mysql-vs-postgresql.md#lock-model-comparison).

## Why DDL is dangerous: the lock queue

This is covered in the comparison reference:
[mysql-vs-postgresql.md § Why DDL is dangerous: the lock queue](mysql-vs-postgresql.md#why-ddl-is-dangerous-the-lock-queue).
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

## Dry-run diagnostic codes

`pg-sprite migrate --dry-run` renders each finding as a compiler-style diagnostic —
`severity[code]: message` — and links every code to its anchor below; `lint` reports
the same codes in the conventional `file:line:column: severity: code` shape. The
codes are the same typed values the `--json` report carries, so automation can gate
on them without parsing prose. The dry-run exit code is part of the contract:
**0** when every statement is executable, **2** (the refusal exit code) when any
statement would be refused — or when the target table does not exist, because a
plan classified from zero facts must not read as green. The exit code answers
*should this proceed*; the report answers *why not* — exit 2 covers every
refusal cause (no safe path, backend unavailable, target facts, missing
table), so automation that needs the cause reads the typed `reason` and
disposition from the `--json` report. A destructive-but-executable change
(`DROP COLUMN`, `DROP TABLE`) is **not** a refusal: it warns and exits 0. A
gate that must stop drops checks `.statements[].destructive` in the JSON
report.

### `metadata-only`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| runs as written | at most a short `ACCESS EXCLUSIVE` (see table below) | none | 0 |

The operation is a brief catalog-only change: it takes at most a short
`ACCESS EXCLUSIVE` lock but does not scan or rewrite the table. Safe to run as
written on any table size, provided the lock can be acquired promptly
(pg-sprite runs every session under a bounded `lock_timeout`).

`ACCESS EXCLUSIVE` is the bucket's worst case, not its uniform cost — several
forms take only `SHARE UPDATE EXCLUSIVE`, which does not block reads or
writes, only competing DDL and vacuum:

| Operation | Lock actually taken |
|---|---|
| `ADD COLUMN` (plain), `DROP COLUMN`, `SET DEFAULT` / `DROP DEFAULT`, `DROP NOT NULL`, `SET SCHEMA`, `DROP CONSTRAINT` | brief `ACCESS EXCLUSIVE` |
| `ALTER INDEX ... RENAME`, `SET STATISTICS`, `SET (attribute_option)`, `SET (storage_parameter)` | `SHARE UPDATE EXCLUSIVE` — reads and writes proceed |
| `CREATE TABLE` (standalone, not a partition) | no lock on any existing relation |

### `online-idiom`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| runs as written | never held for the duration of a scan against readers or writers (see table below) | per operation (see table below) | 0 |

The submitted form is already PostgreSQL's safe online idiom (for example
`CREATE INDEX CONCURRENTLY` or `ADD CONSTRAINT ... NOT VALID`). It runs as
written and does not block the table while it runs.

The bucket is not uniform: the concurrent builds and `VALIDATE` hold only
`SHARE UPDATE EXCLUSIVE` while they work, and the `NOT VALID` / `USING INDEX`
forms take a stronger lock but only for a brief catalog change with no scan:

| Operation | Lock actually taken | Scan |
|---|---|---|
| `CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `REINDEX CONCURRENTLY`, `DETACH PARTITION CONCURRENTLY` | `SHARE UPDATE EXCLUSIVE` — reads and writes proceed | non-blocking index build / drop |
| `VALIDATE CONSTRAINT` | `SHARE UPDATE EXCLUSIVE` (foreign-key validation also takes `ROW SHARE` on the referenced table) | full validation scan, non-blocking |
| `ADD CONSTRAINT ... NOT VALID` | brief `ACCESS EXCLUSIVE` for `CHECK`; a foreign key takes `SHARE ROW EXCLUSIVE` on both tables | none — validation is deferred |
| `ADD CONSTRAINT ... USING INDEX` | brief `ACCESS EXCLUSIVE` | none — the index already exists |

### `fast-default`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| runs as written | brief `ACCESS EXCLUSIVE` | none — default stored in the catalog | 0 |

`ADD COLUMN ... DEFAULT <non-volatile>` on PostgreSQL 11+ stores the default in
the catalog instead of rewriting the table. Catalog-only; runs as written.

### `binary-coercible`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| runs as written | brief `ACCESS EXCLUSIVE` | none — column relabeled in place | 0 |

The type change is binary-coercible (for example `varchar(50)` → `varchar(100)`,
or `varchar` → `text`), so PostgreSQL relabels the column in place without a
table rewrite. Runs as written. How PostgreSQL decides, which changes only
*look* free (shortening `varchar`, changing `numeric` scale, `char(n)` → `text`),
and the exact rules pg-sprite accepts are in
[binary-coercible-type-changes.md](binary-coercible-type-changes.md).

### `safer-idiom`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| substituted — the online sequence runs instead | brief per-step locks; no whole-operation blocking lock | non-blocking index build or validation scan | 0 |
| refused (`rewrite-required`) — no online sequence could be constructed | would block as written | — | 2 |

The statement reaches a safe end state, but as written it holds a blocking lock
for the whole operation (for example `ADD CONSTRAINT ... UNIQUE`, which builds
the index under `ACCESS EXCLUSIVE`). When pg-sprite can construct the
equivalent online sequence — such as `CREATE UNIQUE INDEX CONCURRENTLY`
followed by `ADD CONSTRAINT ... UNIQUE USING INDEX` — it substitutes it, with
each step committing on its own. The sequence is not transactionally
equivalent to the original statement and must not run inside a transaction
block. When no online sequence can be constructed (for example
`ADD COLUMN ... UNIQUE`, `ATTACH PARTITION`, or `ADD CONSTRAINT ... NOT NULL`),
the classification stays `safer-idiom` but the statement is refused with
[`rewrite-required`](#rewrite-required) and exits 2.

### `app-breaking-rename`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| runs as written — coordinate with application deploys | brief `ACCESS EXCLUSIVE` | none | 0 |

`RENAME COLUMN` / `RENAME TABLE` is a brief catalog change, but every query
still using the old name fails the moment it commits. pg-sprite flags it so the
rename is coordinated with application deploys.

### `volatile-default`

| Verdict | Lock (as written) | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| routed to the copy-and-swap backend | `ACCESS EXCLUSIVE` for the whole rewrite | full table rewrite | 2 until the backend lands (refused `backend-unavailable`) |

`ADD COLUMN ... DEFAULT <volatile>` (for example `now()`, `gen_random_uuid()`)
must compute the default per row: a full table rewrite under `ACCESS EXCLUSIVE`
that blocks reads and writes. Routed to the copy-and-swap backend.

### `generated-stored`

| Verdict | Lock (as written) | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| routed to the copy-and-swap backend | `ACCESS EXCLUSIVE` for the whole rewrite | full table rewrite | 2 until the backend lands (refused `backend-unavailable`) |

Adding a `GENERATED ... STORED` column computes and stores the value for every
row: a full table rewrite under `ACCESS EXCLUSIVE`. Routed to the copy-and-swap
backend.

### `type-rewrite`

| Verdict | Lock (as written) | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| routed to the copy-and-swap backend | `ACCESS EXCLUSIVE` for the whole rewrite | full table rewrite | 2 until the backend lands (refused `backend-unavailable`) |

The column type change is not binary-coercible (for example `int` → `bigint`,
`text` → `jsonb`), so PostgreSQL rewrites the whole table under
`ACCESS EXCLUSIVE`. Routed to the copy-and-swap backend.

### `relocation`

| Verdict | Lock (as written) | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| routed to the copy-and-swap backend | exclusive lock for the whole move | rewrite-scale I/O | 2 until the backend lands (refused `backend-unavailable`) |

The operation physically moves the table's storage (for example
`SET TABLESPACE`): rewrite-scale I/O under an exclusive lock. Routed to the
copy-and-swap backend.

### `partition-parent-lock`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| runs as written — schedule deliberately | brief `ACCESS EXCLUSIVE` on the partitioned parent | none | 0 |

The operation takes a brief exclusive lock on a partitioned parent, briefly
blocking access to every partition. Flagged so it is scheduled deliberately.

### `unsupported-operation`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| refused — never executed | — | — | 2 |

pg-sprite does not recognize the operation and will not run it. Fail-closed:
an unclassified statement is never executed.

### `destructive`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| warning — does not change the routing decision | per the routing decision | per the routing decision | unchanged by this code |

The change discards live data or structure: `DROP COLUMN`, `DROP TABLE`,
truncating conversions, a dropped constraint or index, or `DROP NOT NULL`
(which discards the same guarantee as dropping the equivalent constraint;
`DROP DEFAULT` is deliberately not destructive — a default guarantees nothing
about existing rows and is recreated by a metadata-only statement). Emitted
alongside the routing decision as a warning so destructive intent is always
visible in review.

### `rewrite-required`

| Verdict | Lock (as written) | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| refused — never executed | would block | — | 2 |

The statement blocks as written and pg-sprite could not construct an online
replacement (for example `ADD COLUMN ... UNIQUE`, where the column and the
constraint arrive in one statement). Refused; rewrite the change as separate
online steps and dry-run each one.

### `backend-unavailable`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| refused — nothing executes | — | — | 2 |

The plan routes to a backend — an online shadow-table copy with a cutover —
that this build does not implement yet. Refused; nothing executes. The
copy-and-swap backend is on the roadmap; until it lands these changes need a
manual online plan.

### `unsupported-partitioned-parent`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| refused — never executed | — | — | 2 |

The routed plan builds an index concurrently, and the target is a partitioned
parent — PostgreSQL cannot `CREATE INDEX CONCURRENTLY` on a partitioned table.
Refused; build the index on each partition concurrently, then attach.

### `unsupported-statement`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| refused — never executed | — | — | 2 |

The planner found no known safe path for the statement and no more specific
refusal cause applies (for example `CLUSTER ON`, `SET LOGGED`, or an
`EXCLUDE` constraint). Refused; run the change through a maintenance window
or rework it into forms the planner classifies. The dry run and the run
path report this refusal under the same code, and both name the refused
operation when the classifier recognized which part of the statement it
cannot route.

### `table-not-found`

| Verdict | Lock | Scan / rewrite | Dry-run exit |
|---|---|---|---|
| refused — never executed | — | — | 2 |

The target table does not exist on the session `search_path`, so the dry
run classified the statement from zero facts — the conservative worst
case — and running without `--dry-run` would fail. The plan report carries
`table_exists: false` so automation can see the same state the diagnostic
reports. Fix the table name (or create the table) and dry-run again.
