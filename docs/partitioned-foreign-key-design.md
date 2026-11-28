# Adding a foreign key to a partitioned table online

On PostgreSQL 14 to 17, `ALTER TABLE parent ADD FOREIGN KEY ... NOT VALID` is refused for a
partitioned table ("cannot add NOT VALID foreign key on partitioned table"). The only native
form validates every partition while it holds `SHARE ROW EXCLUSIVE` on the whole tree, which
blocks every writer for the whole scan.

The same plain statement adopts a partition's existing foreign key of the same definition
instead of creating and validating one. `executor.AddPartitionedForeignKey` builds on that:

1. For each leaf partition, `ADD CONSTRAINT <derived> FOREIGN KEY ... NOT VALID` in a brief
   transaction, then `VALIDATE CONSTRAINT`, which scans under `SHARE UPDATE EXCLUSIVE` while
   writes continue.
2. On the parent, in one brief transaction: lock the tree and the referenced table, prove every
   leaf has exactly one validated, unattached foreign key of the definition (the one step 1
   recorded), run the plain `ADD CONSTRAINT <name> FOREIGN KEY ...`, and read back that every
   leaf's foreign key now has `conparentid` set to the parent's under its recorded OID. Any other
   outcome rolls the transaction back.

The derived leaf name is the parent constraint's name, `_p`, and the leaf's OID, cut with a digest
when it would pass the identifier limit (the partitioned index build's derivation). A rerun reads
each leaf and resumes: a validated matching foreign key under any name is reused, a `NOT VALID`
one is validated, and nothing is added twice.

## Resume decision on one leaf

| What the leaf has | Step |
| --- | --- |
| One matching foreign key, validated, attached to nothing | none (`found`) |
| One matching foreign key, `NOT VALID`, attached to nothing | `VALIDATE CONSTRAINT` (`validated`) |
| No matching foreign key, derived name free | `ADD ... NOT VALID`, then `VALIDATE` (`added`) |
| No matching foreign key, derived name held by another constraint | refused, `partitioned-foreign-key-name-taken` |
| Two or more matching foreign keys | refused, `partitioned-foreign-key-conflict` |
| A matching foreign key already attached to a parent's | refused, `partitioned-foreign-key-conflict` |

"Matching" is the comparison the server makes before it adopts: the same referenced table, the
same referencing and referenced columns in order, the same `MATCH`, `ON UPDATE`, `ON DELETE`, and
deferrability, and no `ON DELETE` column list. Foreign keys of another definition are left alone.

## Verified against PostgreSQL

Checked on PostgreSQL 14, 16, 17, and 18 in `psql` (three partitions, one million rows each), and
by the integration tests in `pkg/executor`:

- Leaf `ADD FOREIGN KEY ... NOT VALID` holds `ShareRowExclusiveLock` on the leaf and on the
  referenced table, and scans nothing.
- Leaf `VALIDATE CONSTRAINT` holds `ShareUpdateExclusiveLock` on the leaf, `RowShareLock` on the
  referenced table, and `AccessShareLock` on its unique index. A writer's open transaction on the
  leaf does not hold it up.
- The parent's plain `ADD FOREIGN KEY` over validated leaf foreign keys holds
  `ShareRowExclusiveLock` on the parent and every leaf, and `ShareRowExclusiveLock` and
  `AccessExclusiveLock` on the referenced table: adopting a leaf's foreign key drops that foreign
  key's own action triggers on the referenced table. Its duration does not grow with rows (about
  2 ms at ten rows and at one million rows per leaf, against about 50 ms per leaf when it has to
  validate), and every leaf's foreign key keeps its OID and gets `conparentid`.
- The parent creates and validates a new foreign key on a leaf, under the parent's locks, and
  leaves the leaf's own beside it, when the leaf's differs in `ON DELETE`, `ON UPDATE`,
  deferrability, `MATCH`, or column order, and, on 14 to 17, when the leaf's is still `NOT VALID`.
  On 18 a `NOT VALID` leaf foreign key is adopted and validated under the parent's locks. The
  read-back catches each case: the adopted foreign key's OID is not the recorded one.
- With two matching leaf foreign keys, the parent adopts one and leaves the other, so the leaf
  step refuses that state before the parent's statement can choose.
- PostgreSQL 18 accepts `ADD FOREIGN KEY ... NOT VALID` on the parent (the same locks as the plain
  form, and no scan), and `VALIDATE CONSTRAINT` on the parent holds `ShareUpdateExclusiveLock`.
  The ordinary online route, `NOT VALID` then `VALIDATE`, therefore works there, and
  `AddPartitionedForeignKey` refuses PostgreSQL 18 and later with
  `partitioned-foreign-key-not-valid-supported` rather than keep a second path.

## Out of scope

- A sub-partitioned partition (`partitioned-foreign-key-sub-partitioned`), a foreign table
  partition (`foreign-table-partition`), and an unfinished detach (`partition-detach-pending`).
- A partitioned, foreign, or view referenced table, or a referenced table inside the tree
  (`partitioned-foreign-key-referenced-table`): the parent's statement would also clone the
  foreign key onto every referenced partition, which the build neither proves nor reads back.

## Routing from a desired file

A foreign key a desired file adds to a partitioned table plans as an `add-foreign-key` statement
whose referenced table the diff qualifies with the target schema, as it does for every added
foreign key, with the referenced columns the server prints. On PostgreSQL 14–17 the plan
(`diffplan.Plan`, through `plan.RouteDesiredPartitionedForeignKeyBuilds`) gives that statement
the execution `partitioned-foreign-key-build`, the same one `migrate --alter` plans, so `diff`,
`migrate --desired --dry-run`, and the fingerprint show the build. `migrate --desired` runs the
statement through `migrate.Run`'s routing below and ends in the same verdicts; its admission
checks `migrate.Options.Connection` before any statement runs, as for a partitioned index build.
A desired run has no admission between the plan and the run, so the plan routes only a foreign
key the build's static admission takes; one it does not, such as an `ON DELETE` column list,
keeps the `NOT VALID` plan and is refused as `unsupported-partitioned-parent`, class
`environmental`, before anything runs. On 18 nothing is routed.

The plan's routing reads the statement, not the catalog, so the refusals the build makes from the
catalog are read at admission instead. Before any statement runs, `migrate --desired`
(`migrate.RunDesired`, through `migrate.AdmitPlanPartitionedForeignKeys`) runs the build's own
catalog checks for every statement routed to it with `executor.AdmitPartitionedForeignKey`, which
resolves the referenced table and lists the tree as the build does (`resolveFKReference`,
`refuseFKTree`) in one bounded read, without the table lock. A foreign key to a partitioned,
foreign, or view table, including one that references the partitioned table itself or one of its
partitions, and a tree with a sub-partitioned partition refuse the whole plan
(`unsupported-statement`, `capability-boundary`); a tree with a foreign-table partition or a
detach pending stops it as a failed result with no verdict, whose error wraps the build's
`ErrForeignTablePartition` or `ErrPartitionDetachPending`. Either way nothing has executed, so a desired file that
also adds a column does not keep the column. `migrate --desired --dry-run` runs the same checks
and prints the same result. The build makes the checks again under its lock when it runs, and
that reading decides; a tree that changes between admission and the build still stops there.

## Routing from `migrate --alter`

`migrate --alter` (`migrate.Run`) routes a lone `ADD FOREIGN KEY` without `NOT VALID` on a
partitioned table to `AddPartitionedForeignKeyAsStep` on PostgreSQL 14–17, on the submitted
statement (`preflight.RoutesToPartitionedForeignKeyBuild`), before the planner's plain-table
`NOT VALID` then `VALIDATE` steps meet the partitioned-parent refusal. On 18 those steps run as
they are. A forced run and a submitted `NOT VALID` are not routed. The dry run plans the
statement with the execution `partitioned-foreign-key-build` and reports the build's shape
refusals (`not-add-foreign-key`, `partitioned-foreign-key-unsupported-shape`) and, through
`migrate.AdmitPartitionedForeignKeyCatalog` and `executor.AdmitPartitionedForeignKey`, the
build's refusals of the referenced table and the sub-partitioned tree, with the verdict the apply
gives; a foreign-table partition or a pending detach ends the dry run in the build's error, as
the apply ends in a failed verdict. The parent's constraints are checked by the apply only, under
the build's lock. The build's catalog refusals are refusals when the build stopped before any partition got the foreign key,
and failed verdicts after; every other stop is a failed verdict with the executor's code
([schemabot-integration](schemabot-integration.md#outcomes-of-a-partitioned-foreign-key-build)).
