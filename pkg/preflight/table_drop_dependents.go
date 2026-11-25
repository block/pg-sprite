package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// ReadTableDropDependents names the objects that make PostgreSQL refuse a
// DROP TABLE of schema.table without CASCADE, other than the foreign keys
// of other tables, which the table's model lists as IncomingForeignKeys.
// They are the objects with a normal dependency on the table, on one of
// its columns, or on its row type or that type's array type: views and
// materialized views that read it, rules and policies of other tables that
// read it, functions whose body or signature names it, and columns of
// other tables typed by its row type. Each is named by the server's
// description of it with its schema, a view by the view rather than its
// rewrite rule, in a stable order. Objects that belong to the table, such as its own
// constraints, indexes, triggers, rules, and policies, go with the drop
// and are left out. A table that does not exist has none.
//
// The read is a planning fact: the server's own refusal at execution
// stays the backstop for a dependent created after it.
func ReadTableDropDependents(ctx context.Context, pool *pgxpool.Pool, schema, table string) ([]string, error) {
	name := pgx.Identifier{schema, table}.Sanitize()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the read of the objects that keep %s from being dropped: %w", name, err)
	}
	defer func() {
		// A read-only transaction holds nothing worth keeping: the rollback
		// only returns the session, and the read's result does not depend
		// on it. It runs detached, so a cancelled read still ends its
		// transaction, and bounded, so a connection that stops answering
		// cannot hold the caller.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// The server leaves the schema out of an object's description when the
	// schema is on the search_path. Clearing it names every object with its
	// schema, whatever the session's search_path is.
	if _, err := tx.Exec(ctx, dbconn.LocalSearchPath("pg_catalog")); err != nil {
		return nil, fmt.Errorf("clear the search_path to read the objects that keep %s from being dropped: %w", name, err)
	}
	// INV: CO-9 — every catalog name in this query is pg_catalog-qualified.
	rows, err := tx.Query(ctx, `
		WITH target AS (
			SELECT c.oid, c.reltype, t.typarray
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid OPERATOR(pg_catalog.=) c.relnamespace
			JOIN pg_catalog.pg_type t ON t.oid OPERATOR(pg_catalog.=) c.reltype
			WHERE n.nspname OPERATOR(pg_catalog.=) $1 AND c.relname OPERATOR(pg_catalog.=) $2
		), dependents AS (
			SELECT DISTINCT d.classid, d.objid, d.objsubid
			FROM pg_catalog.pg_depend d, target
			WHERE d.deptype OPERATOR(pg_catalog.=) 'n'
			  AND d.classid OPERATOR(pg_catalog.<>) 'pg_catalog.pg_constraint'::pg_catalog.regclass
			  AND ((d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			        AND d.refobjid OPERATOR(pg_catalog.=) target.oid)
			    OR (d.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_type'::pg_catalog.regclass
			        AND d.refobjid OPERATOR(pg_catalog.=) ANY (ARRAY[target.reltype, target.typarray])))
		)
		SELECT DISTINCT COALESCE(CASE
		           WHEN dep.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
		                AND r.rulename OPERATOR(pg_catalog.=) '_RETURN'
		           THEN pg_catalog.pg_describe_object('pg_catalog.pg_class'::pg_catalog.regclass, r.ev_class, 0)
		           ELSE pg_catalog.pg_describe_object(dep.classid, dep.objid, dep.objsubid) END, '')
		FROM dependents dep
		CROSS JOIN target
		LEFT JOIN pg_catalog.pg_rewrite r
		       ON dep.classid OPERATOR(pg_catalog.=) 'pg_catalog.pg_rewrite'::pg_catalog.regclass
		      AND r.oid OPERATOR(pg_catalog.=) dep.objid
		WHERE NOT EXISTS (
			SELECT FROM pg_catalog.pg_depend own
			WHERE own.classid OPERATOR(pg_catalog.=) dep.classid
			  AND own.objid OPERATOR(pg_catalog.=) dep.objid
			  AND own.refclassid OPERATOR(pg_catalog.=) 'pg_catalog.pg_class'::pg_catalog.regclass
			  AND own.refobjid OPERATOR(pg_catalog.=) target.oid
			  AND own.deptype OPERATOR(pg_catalog.=) ANY (ARRAY['a', 'i']::pg_catalog.char[]))
		ORDER BY 1`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("read the objects that keep %s from being dropped: %w", name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			return nil, fmt.Errorf("scan an object that keeps %s from being dropped: %w", name, err)
		}
		out = append(out, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the objects that keep %s from being dropped: %w", name, err)
	}
	return out, nil
}
