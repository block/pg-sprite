# Partition-aware index builds on a partitioned parent

_Status: implemented. The `pkg/statement` edits, `executor.BuildPartitionedIndex`, and the planner, preflight, migrate, diff, and plan report wiring below ship together. Tracks [block/pg-sprite#34](https://github.com/block/pg-sprite/issues/34)._

## Problem

PostgreSQL cannot run `CREATE INDEX CONCURRENTLY` on a partitioned table, and a
plain `CREATE INDEX` on one builds every partition's index while holding a lock
that blocks writes to the whole tree. Before this flow, pg-sprite refused both
(`parent-concurrent-index-build`, `parent-blocking-index-build`, RF-6), so a
schema change that adds an index to a partitioned table could not run at all.

PostgreSQL documents an online alternative (the "Partition Maintenance" section
of the partitioning chapter):

1. `CREATE INDEX name ON ONLY parent ...` creates the parent's index without
   building anything. It is marked invalid, because no partition has an index
   attached to it yet.
2. On each partition, `CREATE INDEX CONCURRENTLY` builds a matching index.
3. `ALTER INDEX name ATTACH PARTITION partition_index` attaches each one. Once
   every partition has an attached index, PostgreSQL marks the parent's index
   valid on its own.

This document specifies how pg-sprite runs that flow.

## What the flow does

The flow takes one `CREATE INDEX` whose target is a partitioned table, and ends
with the parent's index valid and every partition's index attached to it. It
accepts the statement with or without `CONCURRENTLY` and with or without
`ON ONLY`: `pg_get_indexdef` prints a partitioned index as `ON ONLY`, so that is
the form a desired-schema diff produces. Every form is a request for a complete,
valid index on the whole tree. An operator who wants only the parent's invalid
index runs `CREATE INDEX ... ON ONLY` directly.

The flow refuses a `TABLESPACE` clause before it creates anything
(`ErrPartitionedIndexTablespace`). A partition's index is built from the
definition `pg_get_indexdef` prints for the parent's index, which never names a
tablespace, so the clause would place the parent's index and leave every
partition's index in the default tablespace, while a partition added later
inherits the parent's. A partition's tablespace is chosen when the partition is
created.

### Steps

Under the advisory table lock (`dbconn.AcquireTableLock`, LK-1), so a second
pg-sprite run on the same parent is refused:

1. **Parent index.** If no index with the requested name exists on the parent,
   create it with `CREATE INDEX name ON ONLY parent ...`. It builds nothing, but
   takes a `ShareLock` on the parent until its transaction commits, so it waits
   for open writers under the step's `lock_timeout` and then blocks new writers
   for one catalog insert. An index already under the name is resumed only when
   it is a partitioned index (`relkind = 'I'`) whose definition matches the
   request; anything else under the name is refused.
2. **Each partition, recursively.** List the parent's direct partitions through
   `pg_catalog.pg_inherits`. For each one that has no index attached to the
   parent's index:
   - A **sub-partitioned** partition gets its own `ON ONLY` index under the
     derived name, is processed recursively, and then has that index attached.
   - A **leaf** partition reuses a matching index (see "Adopting an existing
     index") or gets one built with the concurrent index build executor
     (`buildIndexConcurrently`) under the derived name, and then attaches it.
   - A **foreign table** partition cannot carry an index, so the flow refuses the
     tree before step 1 (`ErrForeignTablePartition`).
   - A partition whose `DETACH PARTITION ... CONCURRENTLY` did not finish is
     left out by the server when it attaches an index, so the flow refuses the
     tree before step 1 (`ErrPartitionDetachPending`), and the attach step's
     proof treats such a partition as no longer in the tree.
   - A **unique** index must contain the partition key of every partitioned
     table in the tree among its plain key columns. PostgreSQL checks each
     partitioned table only when that table gets its index, so the flow checks
     every level once, after the first tree read and before step 1, and refuses
     a mismatch with nothing created (`*UniqueIndexPartitionKeyError`, wrapping
     `ErrUniqueIndexPartitionKey`; `Missing` names each table, as `Schema` and
     `Table`, and the columns its key lacks, in key order). `migrate` maps it to
     an `unsupported-statement`, `by-design` refusal. A dry run reads the same
     keys and reports the same refusal; a host that plans through the library
     calls `migrate.AdmitPartitionedUniqueIndex` with a pool from `pkg/dbconn`.
     A desired-state plan never reaches this check: building the desired table
     on the scratch schema runs the server's own check first, and the run stops
     with that server error before anything executes.

     The check matches columns by exact name. The server also requires each
     matched column to share the partition key's collation and equality
     operator. On the parent a mismatch is refused at step 1, which creates
     nothing; on a sub-partitioned partition it is refused only at that
     partition's step, after the parent's invalid index exists, and a rerun
     resumes into that index and fails at the same step. Once the parent's index
     exists, the server refuses a partition created or attached later whose key
     the index does not contain; a sub-partitioned partition attached between
     the check and step 1 is seen by neither and fails the run at its own step
     the same way.
3. **New partitions.** A partition created during the flow
   (`CREATE TABLE ... PARTITION OF`) gets a clone of every index on its parent,
   already attached. A table attached with `ATTACH PARTITION` ends up with an
   attached index too: the server attaches a matching index the table has, and
   otherwise builds one without `CONCURRENTLY`, which the flow cannot prevent.
   The flow therefore walks the tree again, at most five times in all, and
   refuses with `ErrPartitionedIndexUnsettled` when the last walk still found
   work.
4. **Verify.** Re-read `pg_index.indisvalid` for the parent's index. Success is
   reported only when it is valid; an index the server left invalid is an
   invariant violation, not a success.

### Derived names

A partition's index is named `<parent index name>_p<partition OID>` when that
fits in 63 bytes. Otherwise the parent name is cut at a UTF-8 character
boundary and a short digest of the full name takes its place:
`<cut name>_<8 hex digits of sha256(full name)>_p<partition OID>`. Two parent
indexes on one table that share a long prefix therefore never share a partition
index name. A name that fits is never changed, so earlier builds keep theirs.
The name is deterministic, so a rerun finds the index an earlier run built. If
the derived name is held by anything that is not a matching index on that
partition, or by an index that backs a constraint, the flow refuses rather than
choose another name.

The requested index name is the one the parser yields, already cut to 63 bytes
the way the server cuts it, so the report names the index that exists.

### Adopting an existing index

A leaf partition may already have a matching index under another name. The flow
adopts it instead of building a second copy. Attaching sets `relispartition`, so
from then on the server refuses to drop it on its own: it goes only with the
parent's index. A rerun does not undo the attach. The report marks the partition
`adopted` and names the index, so an operator sees which of their own indexes
the build took over. An index is adopted only when it is valid and ready, is
attached to no partitioned index, and backs no constraint. An index under the
derived name from an earlier run is `reused`, not `adopted`.

### Matching an existing index

An index matches the parent's when PostgreSQL would accept it in
`ATTACH PARTITION`. The flow compares each index's `pg_get_indexdef` output, read
with `search_path = pg_catalog` so every object prints schema-qualified. The key
is the uniqueness marker plus the definition from the access method on: key and
included columns, operator classes, collations, sort options, expressions,
predicate, storage parameters, and `NULLS NOT DISTINCT` (15+). The index name
and table are cut off by their exact printed form, never by searching for a
keyword, because a quoted name can contain one. A definition that does not begin
with the form the server is known to print fails closed.

A resumed parent index is matched through the server: the flow creates a probe
`ON ONLY` index from the request, in a transaction that always rolls back, under
a name no relation holds (`pgsprite_probe_<oid>`, with a numeric suffix when
taken), and compares the probe's printed definition with the existing index's.

### Debris and resume

- An invalid index under the derived name on a leaf, with no backend building
  it, is the debris of an earlier failed concurrent build. The flow removes it
  the way `RebuildAbandonedIndex` does: it proves abandonment under the table
  lock, quarantines it by OID, drops it concurrently, and builds again. The pool
  needs two connections, the concurrent build's own minimum.
- An invalid index under the derived name with a live builder refuses the run.
  The advisory lock makes that unreachable for two pg-sprite runs; it guards
  against an operator's own build.
- A recovery that renamed an abandoned index to `pgsprite_abandoned_<oid>` and
  stopped before the drop leaves the derived name free and the debris in place,
  and a concurrent build refuses a table that still carries it. The flow lists
  the quarantine entries on each leaf even when the name is free, and sweeps them
  before it builds.
- A crash between steps leaves a valid prefix: an invalid parent index and some
  attached partitions. Rerunning the same statement resumes from there.
- A partition whose attached index is invalid is not rebuilt: the parent's index
  could never become valid through it. The flow fails with an invariant
  violation naming the partition and the index.

### A tree that changes during a run

The tree is listed at the start of each pass, and a build can take long. Every
statement names its relation and the server resolves the name when it runs, so
each step proves identity first and refuses with an invariant violation naming
ST-7 when a name moved:

- **Leaf build.** The flow proves the partition's OID still has the listed
  schema and name and is still a direct child of the same parent. The concurrent
  build repeats the check on its own session, after it resolves the table name.
  A concurrent build cannot run in a transaction, so one that lands on a
  replacement table is found afterwards, from the index's own `indrelid`, and
  the flow refuses. Nothing is dropped, because the flow cannot prove that index
  is the one it created: the error names the index and the table it is on for an
  operator to inspect.
- **`ON ONLY` statements** (the parent's, a sub-partitioned partition's, and the
  probe's). One transaction locks the table by name under the bounded
  `lock_timeout`, proves it is still the partitioned table with the expected OID,
  runs the statement, and reads the index it created: it must be a partitioned
  index on that OID, or the transaction rolls back. The parent's index is also
  read again by name before the flow relies on it, and its OID and printed
  definition must be the ones created or checked, so an index dropped and
  recreated under its name refuses the run.
- **Attach.** One transaction locks the partition in `SHARE UPDATE EXCLUSIVE`
  mode, which a detach cannot take until the transaction ends and which does not
  block the partition's writers, then proves the partition, the parent's index,
  and the partition's index by OID, runs the attach, and reads `pg_inherits`
  again: the partition's index must be attached to the parent's index by OID, or
  the transaction rolls back. A partition detached after its index was built is
  refused with ST-7. The error gives the last-known names and OIDs of the index
  and the partition and asks an operator to inspect the catalog, because a name
  captured earlier may no longer name the same relation. The index stays: the
  flow drops nothing it did not just prove abandoned, and a rerun does not list a
  table that left the tree.

### Locks and budgets

- Every statement except the concurrent builds runs in its own short
  transaction under the pool's bounded `lock_timeout` and `statement_timeout`
  (LK-2); a session where either is 0 is refused. `ALTER INDEX ... ATTACH
  PARTITION` takes `ShareUpdateExclusiveLock` on the parent's index,
  `AccessShareLock` on the parent and the partition, and `AccessExclusiveLock` on
  the partition's index until its transaction commits. A statement opens the
  indexes of the table it reads or writes and releases them when it ends, so the
  attach waits behind DML statements running on that partition (not behind a
  writer's open transaction), and while it holds or waits for the index, every
  new statement on that partition that opens it queues behind. The step's
  `lock_timeout` bounds the attach's wait to acquire the lock: a wait that runs
  out is `budget-lock-exceeded`, the finished partitions stay attached, and a
  retry resumes. Once acquired, the lock is held through the read-back and the
  commit, each statement bounded by `statement_timeout`, and statements queued
  behind it wait under their own clients' timeouts. Other partitions are never
  affected, and the transaction is brief.
- Before the attach, the step locks the partition `SHARE UPDATE EXCLUSIVE`, so
  that the proof that it is still in the tree holds until commit. That lock does
  not conflict with DML, but it waits behind anything that holds
  `SHARE UPDATE EXCLUSIVE` or a stronger lock on the partition: `VACUUM`,
  `ANALYZE`, `CREATE INDEX [CONCURRENTLY]`, and `REINDEX CONCURRENTLY`. An autovacuum normally gives way to the lock request,
  but an anti-wraparound autovacuum does not, so on a large partition it can make
  every attach `budget-lock-exceeded` until it finishes; the finished partitions
  stay attached, and a retry resumes.
- The step locks the partition before the attach takes `ACCESS SHARE` on the
  parent, which is the opposite order from a plain `DETACH PARTITION` (the
  parent `ACCESS EXCLUSIVE` first, then the partition). When the two overlap, the
  server either lets the step's `lock_timeout` run out (`budget-lock-exceeded`)
  or detects a deadlock and cancels one side with SQLSTATE 40P01. When the step
  is the side cancelled, the outcome is `execution-failed`: nothing of that step
  committed, and a rerun resumes from the partitions already attached. The leaf builds, being
  concurrent, do not block writers.
- Statements derived from the parent's definition are fully schema-qualified, so
  they read the same under any `search_path`. The statements that carry the
  request's own text (`ON ONLY` and the probe) run with the target's schema,
  then `public`, on the path, the same path a concurrent build pins.
- The table lock is taken on a session of its own, dialed from the
  `dbconn.Config`, and every step runs on the pool. Before the first step the
  flow asks the pool's database which backend holds the lock, and refuses with
  LK-1 unless it is the lock session's backend, matched by PID and backend
  start, so a config and a pool that point at different databases or servers
  are caught before anything is created. When the pool's role cannot see that
  session's backend start, which `pg_stat_activity` hides from another role
  without `pg_read_all_stats`, the flow refuses with `ErrTableLockUnconfirmed`
  instead.
- A lock that is lost voids the run: the flow's context ends with it, and the
  error joins `ErrInvariantViolation` and `dbconn.ErrTableLockLost` (LK-1),
  whatever the interrupted step reported, so a lost lock is never a retryable
  `invalid-index-*` outcome.
- A step's transaction rolls back under a detached context bounded by
  `sessionCleanupTimeout`, so a cancelled run leaves no transaction open on a
  pooled connection.
- Catalog reads are `pg_catalog`-qualified (CO-9). Generated SQL quotes every
  identifier with `pgx.Identifier{...}.Sanitize()`.

### Building the statements

The parent and partition statements come from the request through
`pkg/statement`, the parse boundary: `statement.OnlyIndex(sql)` returns the
request as `CREATE INDEX ... ON ONLY` without `CONCURRENTLY`, and
`statement.PartitionIndex(parentIndexDef, schema, table, name, form)` takes the
parent index's `pg_get_indexdef` and returns
`CREATE INDEX CONCURRENTLY name ON schema.table ...` (`PartitionIndexConcurrently`,
a leaf) or `CREATE INDEX name ON ONLY schema.table ...` (`PartitionIndexOnly`, a
sub-partitioned partition and the probe). They set the name, the target relation,
and the `ONLY` and `CONCURRENTLY` flags, and change no column, expression, or
predicate.

## Where it plugs in

- **Executor (core).** `executor.BuildPartitionedIndex(ctx, cfg, pool, pt, sql,
  budget)` runs the flow as a library call, and
  `BuildPartitionedIndexWithProgress` adds a progress tracker. `cfg` is the
  `dbconn.Config` the pool was made from, because the advisory lock opens a
  dedicated session the pool cannot supply. It is admitted only for a
  partitioned target (ST-7). Its refusals are typed: `ErrNotPartitionedTable`,
  `ErrPartitionedIndexNameTaken`, `ErrForeignTablePartition`,
  `ErrUniqueIndexPartitionKey`, `ErrTableLockUnconfirmed`, `ErrPartitionDetachPending`,
  `ErrPartitionedIndexTablespace`, and `ErrPartitionedIndexUnsettled`, beside the
  concurrent build's own. The report lists each partition the run gave an index
  (`built`, `reused`, `adopted`, or `partitioned`) and whether it recovered
  debris.
- **Progress.** Step one is the parent's index and each partition that needs
  work is one step after it. `total_steps` starts at the partitions found plus
  one and grows when the flow finds more (`Tracker.AddSteps`). It is an upper
  bound, not a forecast: whether a partition needs work is read from the catalog
  when the flow reaches it, so a partition that already has its index attached
  takes no step and a resumed run finishes below the total. A step never goes
  back: a sub-partitioned partition's own attach stays on the step of the last
  partition below it.
- **Preflight, planner, migrate, plan report.** A `CREATE INDEX` on a partitioned
  parent routes to the flow instead of being refused, as a new execution kind
  that names it. The blocking substitute stays refused, and so do index adoption
  and the pre-18 `NOT VALID` foreign key. Partitions are listed at execution time,
  not baked into the plan, because they can change between plan and apply.
- **Diff.** An invalid partitioned index on the live parent whose definition
  matches the desired one plans as an unfinished build (the same `CREATE INDEX`),
  so a desired run that crashed mid-flow resumes instead of re-planning empty.

## Primary key and unique constraints

`executor.AddPartitionedKey(ctx, cfg, pool, pt, sql, budget)` (and
`AddPartitionedKeyAsStep`) adds a `PRIMARY KEY` or `UNIQUE` constraint to a
partitioned table online. `migrate --alter` (`migrate.Run`) routes a lone
`ADD PRIMARY KEY` or `ADD UNIQUE` on a partitioned table to it, on the submitted
statement (`preflight.RoutesToPartitionedKeyBuild`), before the plain table's
`USING INDEX` sequence the planner makes for it meets the partitioned-parent
refusal; a forced run and `ADD CONSTRAINT ... USING INDEX` still meet that
refusal. The dry run plans it with the execution `partitioned-key-build` and
reports the shape, partition key, and sub-partitioned tree refusals; the
parent's checks are read only under the build's lock, so for those the dry run
reports the build and the apply refuses. Desired-state plans (`diff`,
`migrate --desired`) do not route it yet and refuse the statement. A plain
`ALTER TABLE parent ADD CONSTRAINT` builds every partition's index under
`ACCESS EXCLUSIVE`, but it adopts a partition's unattached constraint of the
same definition instead of building one. The flow relies on that:

1. **Admission**, before the lock: exactly one schema-qualified
   `ALTER TABLE parent ADD CONSTRAINT name {PRIMARY KEY | UNIQUE} (columns)`.
   The name is required, because each partition's index and constraint are named
   `<name>_p<partition OID>` (the derived names above) and a rerun finds them by
   those names. `INCLUDE`, `NULLS NOT DISTINCT`, `DEFERRABLE`, `INITIALLY
   DEFERRED`, `WITH (...)`, `USING INDEX TABLESPACE`, `USING INDEX`, `ONLY`, and
   a repeated column are refused (`partitioned-key-unsupported-shape`); a dry run
   calls `AdmitPartitionedKeyShape` for the same answer.
2. **Under the advisory table lock**, before any partition is touched: the
   target must be partitioned; the key must include every partition key column
   (the unique index check above, with its error); the parent must not already
   have another constraint under the name, a relation holding the name its index
   would take, or, for a primary key, another primary key
   (`partitioned-key-parent-conflict`); a primary key's columns must be
   `NOT NULL` on the parent. A parent that already has exactly the requested
   constraint, adopted on every partition, is reported done: that is what a run
   that stopped after its last commit leaves.
3. **The tree** must hold only leaf partitions directly under the parent: a
   foreign table (`foreign-table-partition`) and a sub-partitioned partition
   (`partitioned-key-sub-partitioned`) are refused, and so is a partition whose
   `DETACH PARTITION ... CONCURRENTLY` has not finished
   (`partition-detach-pending`): it is leaving the tree, so a key given to it
   would not stay the parent's.
4. **Each leaf** gets its unique index built concurrently and then
   `ADD CONSTRAINT ... USING INDEX` in a brief transaction (`ensureLeafKey`),
   resumed from whatever the leaf holds.
5. **The parent's step**, one brief transaction under `lock_timeout` and
   `statement_timeout`:

   ```sql
   LOCK TABLE schema.parent IN ACCESS EXCLUSIVE MODE;
   -- prove the name is still the resolved table; list the tree; every leaf must
   -- have exactly one unattached constraint of the key, the one its leaf step
   -- recorded on the recorded index
   ALTER TABLE schema.parent ADD CONSTRAINT name PRIMARY KEY (columns);
   -- read back: the parent's constraint and its valid partitioned index, and on
   -- every leaf the adopted constraint (conparentid) is the recorded one, on
   -- the recorded index OID, now attached to the parent's index
   COMMIT;
   ```

   The `LOCK TABLE` takes the locks the `ADD CONSTRAINT` takes, so the proof
   holds until the statement runs. A leaf no leaf step covered (a partition
   created meanwhile) would have its index built under the lock, so the step
   adds nothing and the walk goes round again, up to five passes
   (`partitioned-key-unsettled`). Any other mismatch, before or after the
   statement, is an invariant violation and the transaction rolls back. The
   window holds `ACCESS EXCLUSIVE` on the tree for catalog reads and writes only,
   a few per partition.

The report (`PartitionedKeyReport`) lists each leaf's constraint, index, and
action (`found`, `constrained`, or `built`, and whether it recovered debris), the
parent constraint's OID, the passes, and the duration.

## Out of scope

- A changed index on a parent, which plans as a drop and a recreate. The drop
  needs a reviewed drop from a schema file (M2.2) and stays refused.
- Primary key and unique constraints added with `ADD CONSTRAINT ... USING INDEX`
  on a parent. PostgreSQL does not support them (`parent-index-adoption`);
  `AddPartitionedKey` above builds the plain form instead.
- A primary key or unique constraint on a tree with sub-partitioned partitions.

## Verified against PostgreSQL

Checked on PostgreSQL 14, 16, and 18, in `psql` and by the integration tests in
`pkg/executor`:

- `CREATE INDEX ... ON ONLY` on a partitioned table creates an invalid index and
  holds `ShareLock` on the parent. On a table with no partitions it is valid at
  once.
- `ALTER INDEX ... ATTACH PARTITION` holds the lock modes listed above, the same
  on every major (`TestAttachPartitionLocksThePartitionsIndex`, read from
  `pg_locks`). The parent's index becomes valid with the last partition's attach.
- A partition created with `PARTITION OF` after the parent's `ON ONLY` index
  exists gets an index already attached, even while the parent's index is still
  invalid. A table added with `ATTACH PARTITION` gets its matching index attached
  or a new one built.
- A sub-partitioned partition's `ON ONLY` index can be attached to the parent's
  index before its own partitions have theirs; both turn valid once the lowest
  level is covered.
- A foreign table partition leaves the parent's index invalid for good.
- Another backend's build under the derived name is an invalid index with a
  `pg_stat_progress_create_index` row, and the flow refuses it with that backend's
  process ID. A failed unique build leaves the index invalid, and the next run
  removes it.
- Writers are not blocked during the leaf builds and are held back only briefly
  by each attach: a writer under a one-second `lock_timeout` keeps committing for
  the whole run.
