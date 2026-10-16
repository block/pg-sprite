package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// AddColumnRewrites reports, for each column the ALTER TABLE in sql adds to
// schema.table, whether PostgreSQL rewrites the table to add it. The server
// answers, not the statement's shape: a constant DEFAULT still rewrites
// every row when the column's type is a domain with constraints, and a
// DEFAULT now() does not, because the server stores any non-volatile
// default in the catalog.
//
// Each column is added on its own (statement.AddColumnProbes) to an empty
// temporary table with no columns, inside a transaction that is always
// rolled back, and a rewrite shows as the probe table's storage being
// replaced (pg_relation_filenode changes). The probe rewrites exactly when
// schema.table would, since the decision depends on the added column's
// type and default, never on the table's rows or its other columns: a
// default cannot name a column, and a generated column, whose expression
// can, gets no probe. The live table is never opened, so the proof takes
// no lock on it and needs no privilege on it. The pool's lock_timeout and
// statement_timeout bound it.
//
// A probe that fails is returned as an error, never read as either answer:
// the role may lack TEMPORARY on the database, or the column's type may
// not exist, and either way the plan cannot say what the statement would
// do.
func AddColumnRewrites(ctx context.Context, pool *pgxpool.Pool, schema, table, sql string) (map[string]bool, error) {
	probeTable, err := probeTableName()
	if err != nil {
		return nil, err
	}
	probes, err := statement.AddColumnProbes(sql, "pg_temp", probeTable)
	if err != nil {
		return nil, fmt.Errorf("build add-column probes for %s: %w", qualifiedName(schema, table), err)
	}
	if len(probes) == 0 {
		return nil, nil
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return nil, fmt.Errorf("parse add-column statement for %s: %w", qualifiedName(schema, table), err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	// The probe transaction is never committed: rollback is the cleanup on
	// success and failure alike, and drops the temporary table with it.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()

	// The probes resolve the statement's unqualified names, a column's
	// type above all, on the path the native executor runs it under: the
	// statement's own schema then public, or the session's path when the
	// statement names no schema.
	if st.Schema() != "" {
		if _, err := tx.Exec(ctx, dbconn.LocalSearchPath(st.Schema(), "public")); err != nil {
			return nil, fmt.Errorf("set the add-column probe's search_path for %s: %w", qualifiedName(schema, table), err)
		}
	}

	probeIdent := pgx.Identifier{"pg_temp", probeTable}.Sanitize()
	rewrites := make(map[string]bool, len(probes))
	for column, probe := range probes {
		if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE "+pgx.Identifier{probeTable}.Sanitize()+" ()"); err != nil {
			return nil, fmt.Errorf("create the table that proves whether adding column %q rewrites %s: %w", column, qualifiedName(schema, table), err)
		}
		var before, after uint32
		if err := tx.QueryRow(ctx, "SELECT pg_relation_filenode($1::regclass)", probeIdent).Scan(&before); err != nil {
			return nil, fmt.Errorf("read the probe table's storage before adding column %q: %w", column, err)
		}
		if _, err := tx.Exec(ctx, probe); err != nil {
			return nil, fmt.Errorf("add column %q of %s to the probe table: %w", column, qualifiedName(schema, table), err)
		}
		if err := tx.QueryRow(ctx, "SELECT pg_relation_filenode($1::regclass)", probeIdent).Scan(&after); err != nil {
			return nil, fmt.Errorf("read the probe table's storage after adding column %q: %w", column, err)
		}
		rewrites[column] = before != after
		if _, err := tx.Exec(ctx, "DROP TABLE "+probeIdent); err != nil {
			return nil, fmt.Errorf("drop the probe table after column %q: %w", column, err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		return nil, fmt.Errorf("roll back add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	return rewrites, nil
}

// probeTableName returns a temporary table name no other object in the
// session's temporary schema has. The table never outlives its
// transaction.
func probeTableName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate probe table name: %w", err)
	}
	return "pgsprite_probe_" + hex.EncodeToString(b[:]), nil
}
