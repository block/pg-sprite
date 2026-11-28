package preflight

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionTimeZone returns the TimeZone setting a session on pool runs
// with: the zone ALTER COLUMN TYPE reads each value in when it converts a
// timestamp column to timestamptz, or a timestamptz column to timestamp.
func SessionTimeZone(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var zone string
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('TimeZone')").Scan(&zone); err != nil {
		return "", fmt.Errorf("read the session time zone: %w", err)
	}
	return zone, nil
}

// UTCTimeZone reports whether zone is one of the names PostgreSQL resolves
// to UTC with no offset and no daylight saving time. The set is
// deliberately narrow: a zone that is not on it is treated as one that
// moves the stored values. The planner and the executor both decide with
// it, so a change planned to relabel is never refused at execution for a
// zone the planner accepted.
func UTCTimeZone(zone string) bool {
	switch zone {
	case "UTC", "Etc/UTC":
		return true
	}
	return false
}

// KeyedColumns names the columns of schema.table that an index or a
// foreign key reads, on the table or on any of its partitions or
// inheritance children, since a type change on the table changes the
// column there too. A change between timestamp and timestamptz rebuilds
// every index on the column and rechecks every foreign key the column is
// part of under its exclusive lock, so a column in this set never relabels.
//
// Three catalog records cover the readers. A plain index depends on each
// column it reads, as a key, in an expression, or in a predicate. An index
// that backs a primary key, unique, or exclusion constraint depends on the
// constraint instead, and the constraint depends on the columns, so the
// index's own key columns are read from pg_index and the constraint's
// columns, expressions and predicate from its dependencies. A foreign key
// on the table depends on each of its referencing columns the same way.
func KeyedColumns(ctx context.Context, pool *pgxpool.Pool, schema, table string) (map[string]bool, error) {
	// INV: CO-9 — every function, operator, and relation is
	// pg_catalog-qualified, and the table is found by name, not through
	// the session search_path.
	const q = `
		WITH RECURSIVE tree(relid) AS (
			SELECT c.oid
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
			WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
			UNION
			SELECT i.inhrelid
			FROM pg_catalog.pg_inherits i
			JOIN tree t ON i.inhparent OPERATOR(pg_catalog.=) t.relid
		)
		SELECT DISTINCT a.attname
		FROM tree t
		JOIN pg_catalog.pg_attribute a
		  ON a.attrelid OPERATOR(pg_catalog.=) t.relid
		 AND a.attnum OPERATOR(pg_catalog.>) 0
		 AND NOT a.attisdropped
		WHERE EXISTS (
			SELECT FROM pg_catalog.pg_index x
			WHERE x.indrelid OPERATOR(pg_catalog.=) t.relid
			  AND a.attnum OPERATOR(pg_catalog.=) ANY (x.indkey::pg_catalog.int2[])
		) OR EXISTS (
			SELECT FROM pg_catalog.pg_depend d
			JOIN pg_catalog.pg_index x ON x.indexrelid OPERATOR(pg_catalog.=) d.objid
			WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND d.refobjid OPERATOR(pg_catalog.=) t.relid
			  AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum
		) OR EXISTS (
			SELECT FROM pg_catalog.pg_depend d
			JOIN pg_catalog.pg_constraint k ON k.oid OPERATOR(pg_catalog.=) d.objid
			WHERE d.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_constraint'::pg_catalog.regclass
			  AND d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND d.refobjid OPERATOR(pg_catalog.=) t.relid
			  AND d.refobjsubid OPERATOR(pg_catalog.=) a.attnum
			  AND k.conrelid OPERATOR(pg_catalog.=) t.relid
			  AND k.contype OPERATOR(pg_catalog.=) ANY (ARRAY['p', 'u', 'x', 'f']::"char"[])
		)`
	rows, err := pool.Query(ctx, q, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read the keyed columns of %s: %w", qualifiedName(schema, table), err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the keyed columns of %s: %w", qualifiedName(schema, table), err)
	}
	keyed := make(map[string]bool, len(names))
	for _, name := range names {
		keyed[name] = true
	}
	return keyed, nil
}
