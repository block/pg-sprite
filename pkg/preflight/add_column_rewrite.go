package preflight

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// temporary table with no columns, inside a savepoint of a transaction that
// is always rolled back, and a rewrite shows as the probe table's storage
// being replaced (pg_relation_filenode changes). The probe rewrites exactly
// when schema.table would, since the decision depends on the added column's
// type and default, never on the table's rows or its other columns: a
// default cannot name a column, and a generated column, whose expression
// can, gets no probe. The live table is never opened by the probe itself,
// and needs no privilege.
//
// Adding the column still evaluates its default once, and its domain's
// checks, the way the real ALTER does, and a function either one calls can
// read any table, the live one included. A probe never waits for a lock:
// one that cannot be granted at once proves nothing, and the column is
// reported as rewriting, the answer that keeps the change off the native
// path. Each probe's savepoint is rolled back as soon as it is read, which
// releases whatever it locked.
//
// An ADD COLUMN IF NOT EXISTS for a column the table already has is skipped
// by the server, so it gets no proof and the column is absent from the
// result. A probe that fails for any other reason is returned as an error,
// never read as either answer: the role may lack TEMPORARY on the
// database, or the column's type may not exist, and either way the plan
// cannot say what the statement would do.
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
	// statement names no schema. A lock_timeout of one millisecond, the
	// smallest that is not "wait for ever", keeps a probe from queueing
	// behind another session's lock.
	setup := "SET LOCAL lock_timeout = 1"
	if st.Schema() != "" {
		setup += "; " + dbconn.LocalSearchPath(st.Schema(), "public")
	}
	if _, err := tx.Exec(ctx, setup); err != nil {
		return nil, fmt.Errorf("set up the add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	existing, err := existingColumns(ctx, tx, schema, table, probes)
	if err != nil {
		return nil, err
	}

	probeIdent := pgx.Identifier{"pg_temp", probeTable}.Sanitize()
	rewrites := make(map[string]bool, len(probes))
	for column, probe := range probes {
		if probe.IfNotExists && existing[column] {
			continue
		}
		rewrites[column], err = probeAddColumn(ctx, tx, probeTable, probeIdent, column, probe.SQL)
		if err != nil {
			return nil, fmt.Errorf("add column %q of %s to the probe table: %w", column, qualifiedName(schema, table), err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		return nil, fmt.Errorf("roll back add-column probe for %s: %w", qualifiedName(schema, table), err)
	}
	return rewrites, nil
}

// sqlstateLockNotAvailable is lock_not_available, raised when a lock is
// not granted within lock_timeout.
const sqlstateLockNotAvailable = "55P03"

// probeAddColumn adds one column to a fresh probe table inside a savepoint
// and reports whether doing so replaced the table's storage. A lock the
// probe cannot take at once reports a rewrite (see AddColumnRewrites). The
// savepoint is rolled back either way, dropping the probe table and
// releasing its locks.
func probeAddColumn(ctx context.Context, tx pgx.Tx, probeTable, probeIdent, column, probe string) (bool, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT pgsprite_probe"); err != nil {
		return false, fmt.Errorf("open the probe savepoint: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE "+pgx.Identifier{probeTable}.Sanitize()+" ()"); err != nil {
		return false, fmt.Errorf("create the probe table: %w", err)
	}
	var before, after uint32
	if err := tx.QueryRow(ctx, "SELECT pg_relation_filenode($1::regclass)", probeIdent).Scan(&before); err != nil {
		return false, fmt.Errorf("read the probe table's storage before adding column %q: %w", column, err)
	}
	rewrites := false
	_, err := tx.Exec(ctx, probe)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable:
		rewrites = true
	case err != nil:
		return false, err
	default:
		if err := tx.QueryRow(ctx, "SELECT pg_relation_filenode($1::regclass)", probeIdent).Scan(&after); err != nil {
			return false, fmt.Errorf("read the probe table's storage after adding column %q: %w", column, err)
		}
		rewrites = before != after
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pgsprite_probe"); err != nil {
		return false, fmt.Errorf("roll back the probe savepoint: %w", err)
	}
	return rewrites, nil
}

// existingColumns reads which of the columns an IF NOT EXISTS probe adds
// schema.table already has, from the catalog alone: it takes no lock on
// the table.
func existingColumns(ctx context.Context, tx pgx.Tx, schema, table string, probes map[string]statement.AddColumnProbe) (map[string]bool, error) {
	var names []string
	for column, probe := range probes {
		if probe.IfNotExists {
			names = append(names, column)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT a.attname FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		  AND a.attname = ANY($3)`, schema, table, names)
	if err != nil {
		return nil, fmt.Errorf("read the existing columns of %s: %w", qualifiedName(schema, table), err)
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the existing columns of %s: %w", qualifiedName(schema, table), err)
	}
	out := make(map[string]bool, len(existing))
	for _, name := range existing {
		out[name] = true
	}
	return out, nil
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
