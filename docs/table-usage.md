# Table usage evidence

**Before a table is dropped, someone should be able to ask: is anything still using it?**
pg-sprite answers that from PostgreSQL's own statistics, and is honest about how much
those statistics can prove. This page explains what the check reads, how it decides, and
— most importantly — what it cannot see.

`DROP TABLE` is still refused by both front doors today
([capabilities](capabilities.md#deliberately-operator-owned)). The check described here is
a read-only primitive, `preflight.ReadTableUsage` and `TableUsage.Judge`, that a later
drop path will consult. Nothing calls it yet.

## Read this first: what the check can see

> **This is an instance-specific check. It reads the cumulative statistics of the one
> PostgreSQL server pg-sprite is connected to, and nothing else.**
>
> **pg-sprite runs it on the writer — the primary it executes schema changes on. Reads
> served by read replicas, Aurora reader instances, or any other server are NOT counted,
> so a table that is read only from replicas looks idle.**

The other limits, each covered below:

- **Statistics can be reset** — by `pg_stat_reset()`, `pg_stat_reset_single_table_counters()`,
  and the other `pg_stat_reset*` functions, and by crash recovery, which discards them. A
  reset inside the window makes the answer **unknown, never idle**.
- **PostgreSQL 14 and 15 do not record when a table was last scanned**, so on those
  versions the answer is **always unknown** (unless another session holds a lock on the
  table right now, which is recent).
- **Writes are not timestamped.** The insert, update, and delete counters are read and
  reported, but never used to decide. Updates and deletes are seen through the scans that
  find their rows; inserts are seen only while their transaction holds its lock.
- **A failover changes which history you see.** After a standby is promoted, its
  statistics are the reads it served as a standby, not what the old primary served.
- **A partitioned table is judged by its partitions.** The parent keeps no statistics, so
  every leaf partition is read and the most recently used one decides; a partitioned table
  with no partition is **unknown**, never idle.

Because of all this, a usage check is evidence, not permission. The recommended
complement is **rename-then-drop**: rename the table, wait through the window, and drop
it only if nothing broke. A rename breaks every reader by name — including readers on
replicas, which this check cannot see — while staying trivially reversible.

## What is read

`ReadTableUsage(ctx, pool, schema, table)` runs one read-only transaction under its own
`lock_timeout` and `statement_timeout`, takes no lock on the table or any of its partitions,
and qualifies every catalog name with `pg_catalog` (invariant [CO-9](invariants.md#co-9--decision-reads-resolve-to-the-real-catalog)).
It reads, all from the server the pool is connected to:

| Fact | Source | Notes |
| --- | --- | --- |
| Server version | `server_version_num` | Decides whether scan times exist. |
| In recovery | `pg_is_in_recovery()` | A standby's statistics describe that standby only. |
| Counting on | `track_counts` | With it off the server collects no table statistics. |
| Read time | `now()` | The server's clock, never the client's; every window is measured back from it. |
| Database reset time | `pg_stat_database.stats_reset` | When the current database's statistics were last reset; empty when never reset or discarded by crash recovery. |
| Last sequential scan | `pg_stat_all_tables.last_seq_scan` | PostgreSQL 16+ only; the end time of the transaction that scanned. |
| Last index scan | `pg_stat_all_tables.last_idx_scan` | PostgreSQL 16+ only; across all of the table's indexes. |
| Scan counts | `seq_scan`, `idx_scan` | Cumulative since the last reset. |
| Write counts | `n_tup_ins`, `n_tup_upd`, `n_tup_del` | Cumulative, with no time. Reported only. |
| Lock holders | `pg_locks` joined to `pg_stat_activity` | Every other session holding a granted lock on the table — or on any of its partitions — in this database, with its PID, `application_name`, lock modes, and the relation it locked (schema, name, OID). A prepared transaction holds locks with no backend and is reported with PID 0. |

An ordinary table or a partitioned table is read; a view, a foreign table, or any other
relation is `ErrNotTable`. A single partition named directly is read as the ordinary table
it is.

### Partitioned tables

A partitioned table stores no rows, and the server keeps no scan statistics for it: every
scan lands on a partition. So the read follows the table's partitions down to the **leaf
partitions** — the ones that hold rows — at every level: the partitions of a
sub-partitioned partition are leaves of the table, and the default partition is a leaf like
any other. A sub-partitioned partition holds no rows and is not a leaf. The tree is walked
through `pg_inherits` in the same transaction, not with `pg_partition_tree()`, which takes
`ACCESS SHARE` on every partition.

For each leaf the read keeps its own evidence (`TableUsage.Partitions()`): schema, name,
OID, whether it is a foreign table, its last sequential and index scan times, and its scan
counts. The table's own figures are the leaves taken together: its last scan times are the
latest across the leaves, and its counters are the sums. Lock holders are read on every
relation of the tree, the table and its sub-partitioned partitions included, and each
names the relation it locked.

Two cases have no per-leaf statistics to read:

- **No partitions.** A partitioned table with no leaf partition holds no rows, and a query
  against it scans nothing, so the server records no trace of it. Only a lock holder can
  show use; otherwise the answer is **unknown** (`no-partitions`), never idle. Idle would
  claim proof the statistics cannot give: a reporting query against the empty table every
  minute would look exactly like no query at all.
- **Foreign-table partitions.** A foreign table can be a partition; its rows live on
  another server and this server keeps no scan statistics for it. The answer is
  **unknown** (`foreign-partition`) unless another relation shows use.

**Inheritance children are not followed.** An ordinary table's inheritance children (the
`INHERITS` form, not declarative partitioning) are tables of their own: a read through the
parent scans the parent and is recorded on it, a lock on a child is not a lock on the
parent, and `DROP TABLE` on the parent fails without `CASCADE` while any child exists, so
a drop safeguard meets each child explicitly.

### Per-version availability

Verified against PostgreSQL 14 through 18:

| Version | `last_seq_scan` / `last_idx_scan` | Per-table reset time | `stats_reset` of a new database |
| --- | --- | --- | --- |
| 14 | absent | none | set at creation |
| 15 | absent | none | empty until the first reset |
| 16, 17, 18 | present | none | empty until the first reset |

No version keeps a per-table reset time. `pg_stat_reset_single_table_counters()` zeroes
the table's own counters and clears its `last_seq_scan`, but leaves its indexes'
`last_idx_scan` standing; on PostgreSQL 15 and later it also moves the database's
`stats_reset` to the time of the call. That database-wide time is the only reset evidence
there is, so the check uses it for both kinds of reset. (On PostgreSQL 14 a single-table
reset was not seen to move it, which does not matter there: without scan times the
answer is unknown anyway.)

Maintenance does not count as use: `VACUUM` and `ANALYZE` leave both scan counts
unchanged. A backup or export that reads the table on the writer — `pg_dump`, `COPY … TO`
— does count, so a nightly dump on the writer makes every table it covers recent. That
errs toward "not idle", which is the safe direction.

## How it decides

`TableUsage.Judge(window)` returns a `UsageVerdict` with one of three outcomes, a typed
reason, the window, and the evidence it was decided on. The rule is fail-closed: **only
evidence that proves the table idle answers idle** (invariant
[RF-11](invariants.md#refusals-and-preflight-rf)).

The checks run in this order; the first that applies decides. For an ordinary table "the
leaves" is the table itself; for a partitioned table it is every leaf partition. The
"Names" column is what `UsageVerdict.Partition()` returns for a partitioned table (an
ordinary table's verdict names none):

| # | Condition | Outcome | Reason | Names |
| --- | --- | --- | --- | --- |
| 1 | Another session holds a lock on the table or any of its partitions | `recent` | `lock-held` | — (each `LockHolder` names its relation) |
| 2 | The server records no scan times (PostgreSQL 14, 15) | `unknown` | `scan-times-not-recorded` | — |
| 3 | The latest sequential scan across the leaves is inside the window | `recent` | `seq-scan-in-window` | the leaf scanned most recently |
| 4 | The latest index scan across the leaves is inside the window | `recent` | `index-scan-in-window` | the leaf scanned most recently |
| 5 | The server is in recovery | `unknown` | `in-recovery` | — |
| 6 | `track_counts` is off | `unknown` | `counts-not-tracked` | — |
| 7 | The database's statistics were reset inside the window | `unknown` | `stats-reset-in-window` | — |
| 8 | A partitioned table has no leaf partition | `unknown` | `no-partitions` | — |
| 9 | A leaf partition is a foreign table | `unknown` | `foreign-partition` | that partition |
| 10 | Some leaf has no scan of either kind recorded and the server reports no reset time | `unknown` | `stats-start-unknown` | the first such leaf |
| 11 | Otherwise | `idle` | `no-scan-in-window` | — |

An instant exactly at the window's start counts as inside it, so every edge falls toward
"not idle". Evidence of use (rows 1, 3, 4) wins over anything that would make the answer
unknown: a scan a standby served an hour ago is still a scan, and a partition scanned an
hour ago makes the table recent however little the other partitions say. Server-level
reasons (rows 2, 5–7) come before any one partition's reason (rows 8–10).

**The partition rule:** a partitioned table is idle only if it has at least one leaf
partition, every leaf partition would be idle on its own, and no other session holds a
lock on the table or any partition. The most recently used partition decides a recent
answer and is named, and any partition that could not be shown idle on its own makes the
table unknown unless another relation shows use.

Row 10 exists because "never scanned" is only meaningful if counting began before the
window. A recorded scan time is itself proof that counting began at least that early for
that relation; with no scan time at all, only the reset time could say, and a database that
has never been reset — or whose statistics crash recovery discarded — reports none. Each
leaf partition needs its own bound: one partition's old scan time says nothing about when
another's counting began.

### Writes

The write counters have no timestamp: one read cannot tell a write a second ago from one
a year ago, and the check never invents a time for them. What the check does instead:

- An `UPDATE` or `DELETE` finds its rows through a sequential or index scan, and that scan
  is recorded with a time, so these writes are covered through rows 3 and 4.
- An `INSERT` scans nothing. It is seen only while its transaction is open, as a lock
  holder (row 1). A table that only ever receives inserts, from short transactions, can
  look idle.
- Foreign key checks from a referencing table look up the referenced table's key with an
  index scan, so a parent table kept alive by its children's writes shows index scans.

### The window and publishing delay

A scan is recorded with the end time of the transaction that ran it, but the backend
publishes it to the shared statistics only when it next goes idle — at most once a
second, and up to a minute later when the statistics are contended. A transaction still
running holds its lock on the table until it ends, so it is seen as a lock holder; a
transaction that ended within that delay and holds nothing is not seen at all.
`Judge` therefore refuses a window shorter than `MinUsageWindow` (five minutes) with a
`*UsageWindowError`. A window of hours or days is what the question is really about.

## What it cannot see

Restated plainly, because these are the ways an idle answer can be wrong:

- **Other servers.** Streaming replicas, Aurora reader instances, logical subscribers, and
  any other PostgreSQL server keep their own statistics. A table read only there looks
  idle on the writer.
- **Reads that happened before a failover.** A promoted standby keeps its own history; the
  old primary's reads are gone with it.
- **A recent transaction not yet published**, within the delay described above.
- **Inserts from short transactions**, as above.
- **Queries on a partitioned table that touch no partition**: a query whose conditions the
  planner prunes to no partition at all scans nothing, so nothing records it.
- **Uses that did not happen inside the window**: a view or function that references the
  table but has not run, or a job that runs monthly checked against a one-week window.

Rename-then-drop covers most of these; choosing a window longer than the slowest periodic
job covers the rest.
