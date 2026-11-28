package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The foreign key step of an online ATTACH PARTITION. An ATTACH gives the
// attached table every enforced foreign key of the partitioned parent: it
// adopts a validated foreign key of the same definition the table already
// has, and otherwise creates one and validates it while it holds ACCESS
// EXCLUSIVE on the table, so the attach lasts as long as that scan. This
// holds for a parent's foreign key that is NOT VALID too, from PostgreSQL
// 18: the attach still validates the new partition. This file gives the
// table each of them first, while writes go on, through the partitioned
// foreign key build's leaf step (ensureLeafForeignKey): ADD FOREIGN KEY ...
// NOT VALID, then VALIDATE CONSTRAINT. The attach that follows adopts them
// and scans nothing for them. A NOT ENFORCED foreign key of the parent
// (PostgreSQL 18) needs nothing: the attach clones it without a scan.

// errAttachForeignKeyUnsupported is returned for a foreign key of the parent
// the leaf step cannot render: one whose ON DELETE names a column list.
var errAttachForeignKeyUnsupported = errors.New("the step cannot give the table this foreign key of the partitioned parent")

// attachForeignKeyResult is the table's foreign key for one foreign key of
// the parent.
type attachForeignKeyResult struct {
	parentFK string
	leafFKResult
}

// parentForeignKey is one foreign key of the partitioned parent.
type parentForeignKey struct {
	spec partitionedFKSpec
	// deleteSetColumns is whether its ON DELETE names a column list.
	deleteSetColumns bool
	// notEnforced is whether it is NOT ENFORCED.
	notEnforced bool
}

// ensureAttachForeignKeys gives the table every enforced foreign key of the
// parent, validated, and returns what it did for each, in the parent foreign
// keys' OID order. Each is the leaf step's work, which reads and decides
// every call, so a rerun resumes, and refuses, typed, a derived name held by
// another constraint and two foreign keys of the definition. A foreign key
// the step cannot give the table is refused before anything is added for
// it; earlier ones stay. A foreign key to a partitioned table is refused by
// the leaf step's reference resolution. The table is a standalone node, not
// yet a partition, and every statement proves it still is. Each VALIDATE is
// bounded by b. The caller holds the parent's advisory table lock (LK-1).
func ensureAttachForeignKeys(ctx context.Context, pool *pgxpool.Pool, parentOID uint32, table partitionNode, b ValidateBudget,
	started leafFKStatementStarted) ([]attachForeignKeyResult, error) {
	if !table.standalone || table.relkind != "r" {
		return nil, fmt.Errorf("%w: the attach foreign key step takes a standalone plain table, got %s of kind %q",
			ErrInvariantViolation, table.qualified(), table.relkind)
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	tx := briefTx{pool: pool}
	var (
		major int
		fks   []parentForeignKey
	)
	err := tx.read(ctx, "list the foreign keys of the partitioned parent of "+table.qualified(), func(ctx context.Context, q querier) error {
		var versionNum int
		if err := q.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::pg_catalog.int4`).Scan(&versionNum); err != nil {
			return fmt.Errorf("read server_version_num: %w", err)
		}
		major = versionNum / 10000
		var err error
		fks, err = readParentForeignKeys(ctx, q, parentOID, major)
		return err
	})
	if err != nil {
		return nil, err
	}
	results := make([]attachForeignKeyResult, 0, len(fks))
	for _, pfk := range fks {
		if pfk.notEnforced {
			continue
		}
		res, err := ensureAttachForeignKey(ctx, pool, tx, pfk, table, major, b, started)
		if err != nil {
			return results, fmt.Errorf("give %s the foreign key %s: %w", table.qualified(), pfk.spec.name, err)
		}
		results = append(results, attachForeignKeyResult{parentFK: pfk.spec.name, leafFKResult: res})
	}
	return results, nil
}

// ensureAttachForeignKey refuses a foreign key the leaf step cannot render,
// resolves its referenced table, and runs the leaf step on the table.
func ensureAttachForeignKey(ctx context.Context, pool *pgxpool.Pool, tx briefTx, pfk parentForeignKey, table partitionNode,
	major int, b ValidateBudget, started leafFKStatementStarted) (leafFKResult, error) {
	if pfk.deleteSetColumns {
		return leafFKResult{}, fmt.Errorf("%w: its ON DELETE names a column list", errAttachForeignKeyUnsupported)
	}
	var ref fkReference
	err := tx.read(ctx, "resolve the table the foreign key "+pfk.spec.name+" references", func(ctx context.Context, q querier) error {
		var err error
		ref, err = resolveFKReference(ctx, q, pfk.spec)
		return err
	})
	if err != nil {
		return leafFKResult{}, err
	}
	return ensureLeafForeignKey(ctx, pool, resolvedFK{spec: pfk.spec, ref: ref, serverMajor: major}, table, b, started)
}

// parentForeignKeysSQLFormat lists the foreign keys of the table, in OID
// order, by name: the referencing and referenced columns in key order and
// the referenced table's schema and name. A foreign key to a partitioned
// table puts on the referencing table one more foreign key per referenced
// partition, each a child of the table's own; those are left out, as the
// attach clones them from the one they belong to. Every relation, operator,
// and function is pg_catalog-qualified (CO-9).
const parentForeignKeysSQLFormat = `SELECT k.conname,
		        ARRAY(SELECT a.attname::pg_catalog.text
		                FROM pg_catalog.unnest(k.conkey) WITH ORDINALITY AS u(attnum, ord)
		                JOIN pg_catalog.pg_attribute a
		                  ON a.attrelid OPERATOR(pg_catalog.=) k.conrelid
		                 AND a.attnum OPERATOR(pg_catalog.=) u.attnum
		               ORDER BY u.ord),
		        rn.nspname, rc.relname,
		        ARRAY(SELECT a.attname::pg_catalog.text
		                FROM pg_catalog.unnest(k.confkey) WITH ORDINALITY AS u(attnum, ord)
		                JOIN pg_catalog.pg_attribute a
		                  ON a.attrelid OPERATOR(pg_catalog.=) k.confrelid
		                 AND a.attnum OPERATOR(pg_catalog.=) u.attnum
		               ORDER BY u.ord),
		        k.confmatchtype::pg_catalog.text, k.confupdtype::pg_catalog.text, k.confdeltype::pg_catalog.text,
		        %s, %s,
		        k.condeferrable, k.condeferred
		   FROM pg_catalog.pg_constraint k
		   JOIN pg_catalog.pg_class rc ON rc.oid OPERATOR(pg_catalog.=) k.confrelid
		   JOIN pg_catalog.pg_namespace rn ON rn.oid OPERATOR(pg_catalog.=) rc.relnamespace
		  WHERE k.conrelid OPERATOR(pg_catalog.=) $1
		    AND k.contype::pg_catalog.text OPERATOR(pg_catalog.=) 'f'
		    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint p
		                     WHERE p.oid OPERATOR(pg_catalog.=) k.conparentid
		                       AND p.conrelid OPERATOR(pg_catalog.=) k.conrelid)
		  ORDER BY k.oid`

// readParentForeignKeys reads the foreign keys of the parent. Version 14 has
// no ON DELETE column lists, and versions before 18 no NOT ENFORCED foreign
// keys, so their queries read false in those columns' places.
func readParentForeignKeys(ctx context.Context, q querier, parentOID uint32, serverMajor int) ([]parentForeignKey, error) {
	deleteSetColumns := `k.confdelsetcols IS NOT NULL`
	if serverMajor < 15 {
		deleteSetColumns = `false`
	}
	notEnforced := `NOT k.conenforced`
	if serverMajor < 18 {
		notEnforced = `false`
	}
	rows, err := q.Query(ctx, fmt.Sprintf(parentForeignKeysSQLFormat, deleteSetColumns, notEnforced), parentOID)
	if err != nil {
		return nil, fmt.Errorf("list the foreign keys of table %d: %w", parentOID, err)
	}
	defer rows.Close()
	var fks []parentForeignKey
	for rows.Next() {
		var k parentForeignKey
		if err := rows.Scan(&k.spec.name, &k.spec.columns, &k.spec.refSchema, &k.spec.refTable, &k.spec.refColumns,
			&k.spec.match, &k.spec.onUpdate, &k.spec.onDelete, &k.deleteSetColumns, &k.notEnforced,
			&k.spec.deferrable, &k.spec.initiallyDeferred); err != nil {
			return nil, fmt.Errorf("scan the foreign keys of table %d: %w", parentOID, err)
		}
		fks = append(fks, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the foreign keys of table %d: %w", parentOID, err)
	}
	return fks, nil
}
