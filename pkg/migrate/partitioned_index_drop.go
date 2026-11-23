package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// GateReadsCatalog reports whether [Gate]'s refusal of st is provisional
// until the catalog is read: a DROP INDEX of exactly one index, plain or
// CONCURRENTLY. Whether its concurrent idiom exists depends on the index —
// PostgreSQL has no concurrent drop for an index of a partitioned table, and
// will not drop an index attached under a parent index on its own — and
// only the catalog says which kind of index the name resolves to. A front
// door that refuses gated statements before dialing must dial for this one
// and take its refusal from [GateWithCatalog] (which [Run] calls) instead.
func GateReadsCatalog(st statement.Statement) bool {
	return st.Kind() == statement.KindDropIndex && st.IndexTarget() == statement.IndexTargetSingleRelation
}

// GateWithCatalog is [Gate] with the catalog read [GateReadsCatalog] calls
// for. For a single-index DROP INDEX it reads the named index on the pool's
// search_path: an index attached under a parent index is refused as a drop
// PostgreSQL never runs, naming the root partitioned index to drop instead;
// a partitioned index is refused without the concurrent idiom PostgreSQL
// rejects for it, naming the bounded plain drop. Every other statement, a
// plain index, and a name that resolves to no index keep Gate's verdict.
// It executes nothing.
func GateWithCatalog(ctx context.Context, pool *pgxpool.Pool, st statement.Statement) (verdict.Verdict, bool, error) {
	v, refused := Gate(st)
	if !refused || !GateReadsCatalog(st) {
		return v, refused, nil
	}
	index, found, err := readDroppedIndex(ctx, pool, st.IndexRelation())
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == sqlstateInsufficientPrivilege {
		// A role that cannot read the index's schema cannot drop the
		// index either; the gate's refusal stands, as it did before the
		// catalog was read.
		return v, true, nil
	}
	if err != nil {
		return verdict.Verdict{}, false, err
	}
	if !found {
		// Nothing to refine: the gate's refusal stands, and an accepted
		// run reports the missing relation when it resolves the table.
		return v, true, nil
	}
	if index.constraintBacked() {
		return constraintIndexDropVerdict(st, index), true, nil
	}
	if index.attached() {
		return attachedIndexDropVerdict(st, index), true, nil
	}
	// INV: RF-6 — a partitioned index's drop, plain or concurrent, is
	// refused with the partitioned-parent cause, never pointed at the
	// concurrent drop PostgreSQL rejects for it.
	if index.partitioned() {
		pv, err := partitionedIndexDropVerdict(st, index)
		if err != nil {
			return verdict.Verdict{}, false, err
		}
		return pv, true, nil
	}
	return v, true, nil
}

// droppedIndex is what the catalog says about the index a DROP INDEX
// names: its relkind, its owning table, when it is attached under a
// parent index the root partitioned index of its tree and that index's
// table, and the primary key, unique, or exclusion constraint, if any,
// that owns the index whose drop the verdict would advise (the root when
// the index is attached), with that constraint's table quoted for SQL.
type droppedIndex struct {
	relkind              string
	table                string
	rootIndex, rootTable string
	constraint           string
	constraintTable      string
}

// constraintBacked reports whether a partitioned or attached index's drop
// would be refused by PostgreSQL because a constraint owns the index it
// comes down to: the constraint, not any DROP INDEX, removes it.
func (d droppedIndex) constraintBacked() bool {
	return d.constraint != "" && (d.partitioned() || d.attached())
}

// sqlstateInsufficientPrivilege is the SQLSTATE PostgreSQL raises when the
// role may not look up a name in a schema it has no USAGE on.
const sqlstateInsufficientPrivilege = "42501"

// partitioned reports whether the index is a partitioned index, one whose
// table is a partitioned parent.
func (d droppedIndex) partitioned() bool { return d.relkind == relkindPartitionedIndex }

// attached reports whether the index hangs under a parent index, so
// PostgreSQL refuses to drop it on its own.
func (d droppedIndex) attached() bool { return d.rootIndex != "" }

const relkindPartitionedIndex = "I"

// readDroppedIndex reads the index a single-index DROP INDEX names. found
// is false when the name resolves to nothing, or to a relation that is not
// an index. An unqualified name resolves against the session search_path,
// exactly as the statement would.
func readDroppedIndex(ctx context.Context, pool *pgxpool.Pool, rel statement.IndexRelation) (droppedIndex, bool, error) {
	if rel.Kind != statement.IndexRelationIndex {
		return droppedIndex{}, false, fmt.Errorf("%w: DROP INDEX names no single index", executor.ErrInvariantViolation)
	}
	// One row for an index: its kind, its table, and, when the index is a
	// partition of a parent index (relispartition), the top of that index
	// tree and the table it is on. An attached index's own parent may be
	// attached in turn, so the root is the only index whose drop
	// PostgreSQL accepts.
	//
	// INV: CO-9 — every function, operator, relation, and type is
	// pg_catalog-qualified, so a planted overload on the session's
	// search_path cannot change which index is judged. to_regclass itself
	// still resolves the bare name on that path, exactly as the statement
	// will.
	//
	// The last two columns name the primary key, unique, or exclusion
	// constraint on the index's own table that the index backs, and the
	// one the root index backs: PostgreSQL drops such an index only with
	// its constraint.
	const q = `
		SELECT c.relkind::pg_catalog.text,
		       tn.nspname, t.relname,
		       rn.nspname, r.relname,
		       rtn.nspname, rt.relname,
		       cc.conname, rc.conname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_index i ON i.indexrelid OPERATOR(pg_catalog.=) c.oid
		JOIN pg_catalog.pg_class t ON t.oid OPERATOR(pg_catalog.=) i.indrelid
		JOIN pg_catalog.pg_namespace tn ON tn.oid OPERATOR(pg_catalog.=) t.relnamespace
		LEFT JOIN pg_catalog.pg_constraint cc
		       ON cc.conindid OPERATOR(pg_catalog.=) c.oid
		      AND cc.conrelid OPERATOR(pg_catalog.=) i.indrelid
		      AND cc.contype OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog."char"[])
		LEFT JOIN pg_catalog.pg_class r
		       ON c.relispartition
		      AND r.oid OPERATOR(pg_catalog.=) pg_catalog.pg_partition_root(c.oid::pg_catalog.regclass)
		LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid OPERATOR(pg_catalog.=) r.relnamespace
		LEFT JOIN pg_catalog.pg_index ri ON ri.indexrelid OPERATOR(pg_catalog.=) r.oid
		LEFT JOIN pg_catalog.pg_class rt ON rt.oid OPERATOR(pg_catalog.=) ri.indrelid
		LEFT JOIN pg_catalog.pg_namespace rtn ON rtn.oid OPERATOR(pg_catalog.=) rt.relnamespace
		LEFT JOIN pg_catalog.pg_constraint rc
		       ON rc.conindid OPERATOR(pg_catalog.=) r.oid
		      AND rc.conrelid OPERATOR(pg_catalog.=) ri.indrelid
		      AND rc.contype OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x']::pg_catalog."char"[])
		WHERE c.oid OPERATOR(pg_catalog.=) pg_catalog.to_regclass($1)`
	name := regclassName(rel)
	var (
		relkind, tableSchema, table string
		// NULL unless the index is attached under a parent index.
		rootSchema, rootIndex, rootTableSchema, rootTable *string
		// NULL unless a constraint owns the index, or the root index.
		constraint, rootConstraint *string
	)
	err := pool.QueryRow(ctx, q, name).Scan(&relkind, &tableSchema, &table,
		&rootSchema, &rootIndex, &rootTableSchema, &rootTable, &constraint, &rootConstraint)
	if errors.Is(err, pgx.ErrNoRows) {
		return droppedIndex{}, false, nil
	}
	if err != nil {
		return droppedIndex{}, false, fmt.Errorf("read the kind of index %s: %w; nothing was executed", name, err)
	}
	d := droppedIndex{relkind: relkind, table: tableSchema + "." + table}
	if rootIndex == nil {
		if constraint != nil {
			d.constraint, d.constraintTable = *constraint, pgx.Identifier{tableSchema, table}.Sanitize()
		}
		return d, true, nil
	}
	// A root index always has a schema, a pg_index row, and a table, so a
	// partial row is a catalog this read does not understand.
	if anyNull(rootSchema, rootTableSchema, rootTable) {
		return droppedIndex{}, false, fmt.Errorf("%w: index %s is attached under %s, whose schema or table the catalog did not return",
			executor.ErrInvariantViolation, name, *rootIndex)
	}
	d.rootIndex = *rootSchema + "." + *rootIndex
	d.rootTable = *rootTableSchema + "." + *rootTable
	if rootConstraint != nil {
		d.constraint, d.constraintTable = *rootConstraint, pgx.Identifier{*rootTableSchema, *rootTable}.Sanitize()
	}
	return d, true, nil
}

// anyNull reports whether any scanned column was NULL.
func anyNull(cols ...*string) bool {
	for _, c := range cols {
		if c == nil {
			return true
		}
	}
	return false
}

// partitionedIndexDropVerdict refuses a DROP INDEX of a partitioned index
// with the by-design partitioned-parent cause the declarative front door
// uses for the same drop, without the concurrent idiom PostgreSQL rejects
// for it. The plain form stays eligible for the accepted blocking path,
// which runs the one drop PostgreSQL accepts; the detail names it and what
// it locks.
func partitionedIndexDropVerdict(st statement.Statement, index droppedIndex) (verdict.Verdict, error) {
	r, ok := partitionRefusal(preflight.PartitionCauseConcurrentIndexDrop)
	if !ok {
		return verdict.Verdict{}, fmt.Errorf("%w: partitioned index drop carries unclassified cause %q",
			executor.ErrInvariantViolation, preflight.PartitionCauseConcurrentIndexDrop)
	}
	locks := fmt.Sprintf("takes ACCESS EXCLUSIVE on %s, every partition, and every partition's index for as long as it runs", index.table)
	bounded := fmt.Sprintf("--accept-blocking %s and --allow-destructive %s, with a statement timeout", index.table, index.table)
	detail := "PostgreSQL cannot drop an index of a partitioned table concurrently in any supported version, so this plain " +
		"DROP INDEX is the only drop there is, and it " + locks + "; to run it under bounded budgets, rerun it with " + bounded
	if st.Concurrent() {
		detail = "PostgreSQL rejects DROP INDEX CONCURRENTLY for an index of a partitioned table in every supported version; " +
			"the drop it accepts is the plain DROP INDEX of the same index, which " + locks + " — submit that with " + bounded
	}
	return verdict.Verdict{
		Statement: st.SQL(),
		Table:     index.table,
		Detail:    detail,
	}.WithRefusal(r), nil
}

// constraintIndexDropVerdict refuses a DROP INDEX of a partitioned or
// attached index that comes down to an index a primary key, unique, or
// exclusion constraint owns: PostgreSQL refuses that drop in either form
// whatever the acknowledgements, and removes the index only with the
// constraint, so the detail names the ALTER TABLE ... DROP CONSTRAINT
// that does.
func constraintIndexDropVerdict(st statement.Statement, index droppedIndex) verdict.Verdict {
	constraint := pgx.Identifier{index.constraint}.Sanitize()
	return verdict.Verdict{
		Statement: st.SQL(),
		Table:     index.table,
		Detail: fmt.Sprintf("constraint %[1]s on %[2]s owns this index or the partitioned index it is attached under, "+
			"so PostgreSQL refuses every DROP INDEX of it, whatever the acknowledgements, and removes it only together "+
			"with the constraint; to remove it, drop the constraint: ALTER TABLE %[2]s DROP CONSTRAINT %[1]s",
			constraint, index.constraintTable),
	}.WithRefusal(constraintIndexDropRefusal())
}

// attachedIndexDropVerdict refuses a DROP INDEX of an index attached under
// a parent index: PostgreSQL refuses it in either form, because the parent
// index requires it, so no flag can make it run. The detail names the root
// partitioned index whose drop removes it along with the rest of the tree.
func attachedIndexDropVerdict(st statement.Statement, index droppedIndex) verdict.Verdict {
	return verdict.Verdict{
		Statement: st.SQL(),
		Table:     index.table,
		Detail: fmt.Sprintf("this index is attached under the partitioned index %[1]s on %[2]s, and PostgreSQL drops an attached "+
			"index only together with its parent, in either form; drop %[1]s instead — PostgreSQL has no concurrent drop for it, "+
			"so that is a plain DROP INDEX of %[1]s under --accept-blocking %[2]s and --allow-destructive %[2]s, which takes "+
			"ACCESS EXCLUSIVE on %[2]s, every partition, and every partition's index for as long as it runs",
			index.rootIndex, index.rootTable),
	}.WithRefusal(attachedIndexDropRefusal())
}
