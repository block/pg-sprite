package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// A reviewed drop on a table with tables below it. A declarative partition
// owns none of its columns or inherited constraints: they are the parent's,
// PostgreSQL refuses to drop them on the partition alone, and a drop on the
// parent removes them from every partition in the same catalog change. So a
// reviewed column or constraint drop on a partitioned table may reach its
// partitions, and an inheritance child, which owns its copies and anything
// built on them, still refuses the drop. Every catalog read here is
// pg_catalog-qualified (CO-9) and runs under the table lock the drop's
// check took.

// ErrDroppedColumnPartitionKey means a reviewed column drop names a column
// that is part of the partition key of the partitioned table or of a
// sub-partitioned partition below it, read under the table's lock.
// PostgreSQL refuses to drop a partition key column at any level of the
// tree, so the drop cannot run; nothing was dropped. The outcome is
// permanent.
var ErrDroppedColumnPartitionKey = errors.New("the column the plan drops is part of a partition key")

// ErrDroppedColumnPartitionDependent means a reviewed column drop on a
// partitioned table would also remove an object a partition carries of its
// own and that uses the partition's copy of the column — an index, a CHECK
// or foreign key, a statistics object, an owned sequence, or any other
// object with a dependency on the column — read under the locks of the
// whole partition tree. The plan reviewed the parent's column and the
// parent's objects only, so nothing was dropped. Objects a partition holds
// as copies of the parent's — attached indexes, inherited constraints,
// cloned triggers — are the parent's reviewed objects and do not refuse.
// The outcome is permanent: drop or change the partition's own object
// first, then re-plan.
var ErrDroppedColumnPartitionDependent = errors.New("the column the plan drops has an unreviewed dependent on a partition")

// tableBelow is one table at any depth below a drop's target.
type tableBelow struct {
	name        string
	isPartition bool
}

// tablesBelow lists the target's relkind and every table below it, at any
// depth, through pg_inherits, each with whether it is a declarative
// partition.
func tablesBelow(ctx context.Context, tx pgx.Tx, schema, table string) (targetPartitioned bool, below []tableBelow, err error) {
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	rows, err := tx.Query(ctx, `
		WITH RECURSIVE target AS (
		        SELECT c.oid, c.relkind OPERATOR(pg_catalog.=) 'p' AS partitioned
		          FROM pg_catalog.pg_class c
		          JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		         WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2),
		     tree AS (
		        SELECT i.inhrelid AS oid
		          FROM pg_catalog.pg_inherits i
		          JOIN target ON i.inhparent OPERATOR(pg_catalog.=) target.oid
		        UNION
		        SELECT i.inhrelid
		          FROM pg_catalog.pg_inherits i
		          JOIN tree ON i.inhparent OPERATOR(pg_catalog.=) tree.oid)
		SELECT target.partitioned,
		       pg_catalog.quote_ident(n.nspname) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c.relname),
		       c.relispartition
		  FROM target
		  LEFT JOIN tree ON true
		  LEFT JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) tree.oid
		  LEFT JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		 ORDER BY 2`, schema, table)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name *string
		var isPartition *bool
		if err := rows.Scan(&targetPartitioned, &name, &isPartition); err != nil {
			return false, nil, err
		}
		if name == nil {
			continue
		}
		if isPartition == nil {
			return false, nil, fmt.Errorf("table %s below %s.%s has no relispartition", *name, schema, table)
		}
		below = append(below, tableBelow{name: *name, isPartition: *isPartition})
	}
	return targetPartitioned, below, rows.Err()
}

// declarativePartitionTree reports whether every table below a partitioned
// target is a declarative partition. A partitioned table cannot have an
// inheritance child, and a partition cannot be an inheritance parent, so a
// mixed tree is a catalog the server should never hold; it answers false.
func declarativePartitionTree(targetPartitioned bool, below []tableBelow) bool {
	if !targetPartitioned {
		return false
	}
	for _, t := range below {
		if !t.isPartition {
			return false
		}
	}
	return true
}

// inheritanceChildren names the tables below the target that are not
// declarative partitions.
func inheritanceChildren(below []tableBelow) []string {
	var names []string
	for _, t := range below {
		if !t.isPartition {
			names = append(names, t.name)
		}
	}
	return names
}

// dropSaysOnly reports whether st's one operation says ONLY. dropsObject
// has already proven st an ALTER TABLE whose sole operation is the drop.
func dropSaysOnly(st statement.Statement) (bool, error) {
	ops, err := statement.ParseOps(st.SQL())
	if err != nil {
		return false, fmt.Errorf("read the operations of %q: %w", st.SQL(), err)
	}
	if len(ops) != 1 {
		return false, fmt.Errorf("%w: %q has %d operations, and only a step whose sole operation is the drop is checked",
			ErrInvariantViolation, st.SQL(), len(ops))
	}
	return ops[0].Only, nil
}

// refusePartitionedColumnDrop checks a reviewed column drop on a
// partitioned table whose partitions it reaches. It first locks the whole
// partition tree in ACCESS EXCLUSIVE, the lock the drop takes on every
// partition anyway, so no partition can gain an object between this read
// and the drop; the lock is waited for under the attempt's lock_timeout,
// like the drop's own. It then refuses when the column is in a partition
// key at any level of the tree, and when a partition carries an object of
// its own that the drop would remove with the partition's copy of the
// column.
func refusePartitionedColumnDrop(ctx context.Context, tx pgx.Tx, st statement.Statement, d schemadiff.DroppedObject, target string) error {
	// INV: RF-10 — the tree is locked before its catalog is read, so the
	// objects read are the objects the drop removes, and a reviewed drop
	// removes nothing from a partition that the plan did not review.
	if _, err := tx.Exec(ctx, "LOCK TABLE "+target+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("lock the partitions of %s to check what dropping column %s removes from them: %w", target, d.Name, err)
	}
	keyed, err := partitionKeysWithColumn(ctx, tx, st.Schema(), st.Table(), d.Name)
	if err != nil {
		return fmt.Errorf("read the partition keys under %s before dropping column %s: %w", target, d.Name, err)
	}
	if len(keyed) > 0 {
		return fmt.Errorf("%w: column %s is in the partition key of %s, and PostgreSQL does not drop a partition key column",
			ErrDroppedColumnPartitionKey, d.Name, strings.Join(keyed, ", "))
	}
	local, err := partitionLocalDependents(ctx, tx, st.Schema(), st.Table(), d.Name)
	if err != nil {
		return fmt.Errorf("read what depends on column %s in the partitions of %s: %w", d.Name, target, err)
	}
	if len(local) > 0 {
		return fmt.Errorf("%w: dropping column %s of %s would also remove %s, which a partition carries of its own and the plan did not review; "+
			"drop or change it first, then re-plan", ErrDroppedColumnPartitionDependent, d.Name, target, strings.Join(local, "; "))
	}
	return nil
}

// partitionKeysWithColumn names the partitioned tables, the target and any
// sub-partitioned partition below it, whose partition key includes column,
// as a key column (pg_partitioned_table.partattrs) or inside a key
// expression (the internal dependency PostgreSQL records from the column
// to its table for every column a key uses), each rendered schema.table.
func partitionKeysWithColumn(ctx context.Context, tx pgx.Tx, schema, table, column string) ([]string, error) {
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	return queryNames(ctx, tx, `
		WITH RECURSIVE tree AS (
		        SELECT c.oid
		          FROM pg_catalog.pg_class c
		          JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		         WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		        UNION
		        SELECT i.inhrelid
		          FROM pg_catalog.pg_inherits i
		          JOIN tree ON i.inhparent OPERATOR(pg_catalog.=) tree.oid)
		SELECT pg_catalog.quote_ident(n.nspname) OPERATOR(pg_catalog.||) '.' OPERATOR(pg_catalog.||) pg_catalog.quote_ident(c.relname)
		  FROM tree
		  JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) tree.oid
		  JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		  JOIN pg_catalog.pg_partitioned_table pt ON pt.partrelid OPERATOR(pg_catalog.=) c.oid
		  JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) c.oid
		   AND a.attname OPERATOR(pg_catalog.=) $3 AND NOT a.attisdropped
		 WHERE a.attnum OPERATOR(pg_catalog.=) ANY (pt.partattrs::pg_catalog.int2[])
		    OR EXISTS (
		        SELECT 1 FROM pg_catalog.pg_depend d
		         WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		           AND d.objid OPERATOR(pg_catalog.=) c.oid
		           AND d.objsubid OPERATOR(pg_catalog.=) a.attnum
		           AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		           AND d.refobjid OPERATOR(pg_catalog.=) c.oid
		           AND d.refobjsubid OPERATOR(pg_catalog.=) 0
		           AND d.deptype OPERATOR(pg_catalog.=) 'i')
		 ORDER BY 1`, schema, table, column)
}

// partitionLocalDependents names every object that depends on a
// partition's copy of column, at any depth below the target, and that is
// the partition's own rather than a copy of a parent's object, each as
// its type and fully qualified identity (pg_identify_object). Left out,
// as the parent's own column dependents leave them out or as copies of
// the parent's reviewed objects:
//   - the column's own default, which the column's definition carries;
//   - a NOT NULL constraint, which PostgreSQL 18 catalogs and earlier
//     versions keep on the column;
//   - an index attached to a parent's index;
//   - a constraint inherited from a parent's (coninhcount > 0) or cloned
//     from one (conparentid set);
//   - a trigger cloned from a parent's (tgparentid set).
func partitionLocalDependents(ctx context.Context, tx pgx.Tx, schema, table, column string) ([]string, error) {
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	return queryNames(ctx, tx, `
		WITH RECURSIVE tree AS (
		        SELECT i.inhrelid AS oid
		          FROM pg_catalog.pg_inherits i
		          JOIN pg_catalog.pg_class c ON c.oid OPERATOR(pg_catalog.=) i.inhparent
		          JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
		         WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		        UNION
		        SELECT i.inhrelid
		          FROM pg_catalog.pg_inherits i
		          JOIN tree ON i.inhparent OPERATOR(pg_catalog.=) tree.oid),
		     deps AS (
		        SELECT d.classid, d.objid, d.objsubid, a.attnum
		          FROM tree
		          JOIN pg_catalog.pg_attribute a ON a.attrelid OPERATOR(pg_catalog.=) tree.oid
		           AND a.attname OPERATOR(pg_catalog.=) $3 AND NOT a.attisdropped
		          JOIN pg_catalog.pg_depend d ON d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		           AND d.refobjid OPERATOR(pg_catalog.=) tree.oid
		           AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum)
		SELECT DISTINCT o.type OPERATOR(pg_catalog.||) ' ' OPERATOR(pg_catalog.||) o.identity
		  FROM deps
		 CROSS JOIN LATERAL pg_catalog.pg_identify_object(deps.classid, deps.objid, deps.objsubid) o
		 WHERE NOT EXISTS (
		        SELECT 1 FROM pg_catalog.pg_attrdef ad
		         WHERE deps.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_attrdef'::pg_catalog.regclass
		           AND ad.oid OPERATOR(pg_catalog.=) deps.objid
		           AND ad.adnum OPERATOR(pg_catalog.=) deps.attnum)
		   AND NOT EXISTS (
		        SELECT 1 FROM pg_catalog.pg_inherits ii
		         WHERE deps.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
		           AND ii.inhrelid OPERATOR(pg_catalog.=) deps.objid)
		   AND NOT EXISTS (
		        SELECT 1 FROM pg_catalog.pg_constraint con
		         WHERE deps.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_constraint'::pg_catalog.regclass
		           AND con.oid OPERATOR(pg_catalog.=) deps.objid
		           AND (con.contype OPERATOR(pg_catalog.=) 'n'
		                OR con.coninhcount OPERATOR(pg_catalog.>) 0
		                OR con.conparentid OPERATOR(pg_catalog.<>) 0))
		   AND NOT EXISTS (
		        SELECT 1 FROM pg_catalog.pg_trigger tg
		         WHERE deps.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_trigger'::pg_catalog.regclass
		           AND tg.oid OPERATOR(pg_catalog.=) deps.objid
		           AND tg.tgparentid OPERATOR(pg_catalog.<>) 0)
		 ORDER BY 1`, schema, table, column)
}

// queryNames runs a query whose rows are one text column and returns them.
func queryNames(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return names, nil
}
