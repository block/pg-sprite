package preflight

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/statement"
)

// rowReadStatementTimeout bounds the row read. It stops at the first row
// it finds, so the bound is not expected to be reached; it keeps the read
// from holding ACCESS SHARE indefinitely on a pool with no
// statement_timeout of its own.
const rowReadStatementTimeout = 10 * time.Second

// sqlstateNotNullViolation is not_null_violation, raised when an added NOT
// NULL column would hold NULL in a row the table already has.
const sqlstateNotNullViolation = "23502"

// sqlstateClassIntegrityConstraintViolation is the SQLSTATE class of every
// constraint the probe row can fail, a domain CHECK among them.
const sqlstateClassIntegrityConstraintViolation = "23"

// NullFilledColumns reports which of columns, each added NOT NULL by the
// ALTER TABLE sql with no DEFAULT of its own, PostgreSQL would refuse to
// add to schema.table because the table holds a row it would leave NULL
// in. It is empty when none would: when every column gets a value from
// somewhere else, such as its domain's DEFAULT, or when the table holds no
// row. The columns come from planner.NullFillCandidates.
//
// Each column is first added to a temporary table holding one row
// (AddColumnFillsNull), which needs no privilege on the table and no lock
// on it; the table's rows are read (TableHasRows) only when one of the
// columns would be left NULL there.
func NullFilledColumns(ctx context.Context, pool *pgxpool.Pool, schema, table, sql string, columns []string) (map[string]bool, error) {
	if len(columns) == 0 {
		return nil, nil
	}
	fills, err := AddColumnFillsNull(ctx, pool, schema, table, sql, columns)
	if err != nil {
		return nil, err
	}
	if len(fills) == 0 {
		return nil, nil
	}
	rows, err := TableHasRows(ctx, pool, schema, table)
	if err != nil {
		return nil, err
	}
	if !rows {
		return nil, nil
	}
	return fills, nil
}

// AddColumnFillsNull reports which of columns the ALTER TABLE sql adds
// with NULL in an existing row: each is added, as the statement adds it,
// to a temporary table holding one row, and the server's
// not_null_violation is the answer. The server decides, not the
// statement's shape, so a domain DEFAULT that fills the column counts.
// A probe the server refuses for another reason, such as a domain CHECK
// the row's value fails, is not a NULL fill and does not count; the
// add-column proof and the server's own refusal judge that change.
//
// The probes run like AddColumnRewrites': in a transaction that is always
// rolled back, each in a savepoint, on the statement's search_path, and
// never waiting for a lock. The live table is never opened.
func AddColumnFillsNull(ctx context.Context, pool *pgxpool.Pool, schema, table, sql string, columns []string) (map[string]bool, error) {
	probeTable, err := probeTableName()
	if err != nil {
		return nil, err
	}
	probes, err := statement.AddColumnProbes(sql, "pg_temp", probeTable)
	if err != nil {
		return nil, fmt.Errorf("build add-column probes for %s: %w", qualifiedName(schema, table), err)
	}
	st, err := statement.ParseOne(sql)
	if err != nil {
		return nil, fmt.Errorf("parse add-column statement for %s: %w", qualifiedName(schema, table), err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin the NULL-fill probe for %s: %w", qualifiedName(schema, table), err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// INV: LK-2 — the probe bounds itself rather than trusting the pool's
	// session defaults.
	setup := "SET LOCAL lock_timeout = 1; SET LOCAL statement_timeout = " +
		strconv.FormatInt(rowReadStatementTimeout.Milliseconds(), 10)
	if st.Schema() != "" {
		setup += "; " + dbconn.LocalSearchPath(st.Schema(), "public")
	}
	if _, err := tx.Exec(ctx, setup); err != nil {
		return nil, fmt.Errorf("set up the NULL-fill probe for %s: %w", qualifiedName(schema, table), err)
	}
	fills := map[string]bool{}
	for _, column := range columns {
		probe, ok := probes[column]
		if !ok {
			return nil, fmt.Errorf("the statement on %s adds no column %q to probe", qualifiedName(schema, table), column)
		}
		filled, err := probeFillsNull(ctx, tx, probeTable, probe.SQL)
		if err != nil {
			return nil, fmt.Errorf("add column %q of %s to the one-row probe table: %w", column, qualifiedName(schema, table), err)
		}
		if filled {
			fills[column] = true
		}
	}
	return fills, nil
}

// probeFillsNull adds a column with probe to a fresh one-row probe table
// inside a savepoint and reports whether the server refused it as a NULL
// in a NOT NULL column. The savepoint is rolled back either way.
func probeFillsNull(ctx context.Context, tx pgx.Tx, probeTable, probe string) (bool, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT pgsprite_probe"); err != nil {
		return false, fmt.Errorf("open the probe savepoint: %w", err)
	}
	if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE "+pgx.Identifier{probeTable}.Sanitize()+" ()"); err != nil {
		return false, probeTableError(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{"pg_temp", probeTable}.Sanitize()+" DEFAULT VALUES"); err != nil {
		return false, fmt.Errorf("insert the probe row: %w", err)
	}
	_, probeErr := tx.Exec(ctx, probe)
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pgsprite_probe"); err != nil {
		return false, fmt.Errorf("roll back the probe savepoint: %w", err)
	}
	var pgErr *pgconn.PgError
	switch {
	case probeErr == nil:
		return false, nil
	case errors.As(probeErr, &pgErr) && pgErr.Code == sqlstateNotNullViolation:
		return true, nil
	case errors.As(probeErr, &pgErr) && pgErr.Code == sqlstateLockNotAvailable:
		return false, fmt.Errorf("%w: %w", ErrTableRowsLocked, probeErr)
	case errors.As(probeErr, &pgErr) && strings.HasPrefix(pgErr.Code, sqlstateClassIntegrityConstraintViolation):
		return false, nil
	default:
		return false, probeErr
	}
}

// ErrTableRowsLocked means another session holds or awaits a lock that
// blocks reading schema.table, so whether it holds rows is unknown. The
// plan cannot say whether adding a NOT NULL column without a default
// would fail, so it fails rather than guess; a later attempt reads the
// table once the lock is released.
var ErrTableRowsLocked = errors.New("the table cannot be read without waiting for another session's lock")

// TableHasRows reports whether schema.table, or any partition or
// inheritance child below it, holds a row the session can see. Adding a
// NOT NULL column with no default fills every existing row with NULL, so
// PostgreSQL refuses it exactly when this is true.
//
// The read runs in a read-only transaction that never waits for a lock: a
// lock_timeout of one millisecond, as for the add-column proof, keeps it
// from queueing behind another session's ACCESS EXCLUSIVE lock, and a read
// the lock blocks returns ErrTableRowsLocked. It stops at the first row
// it finds, under a statement_timeout of its own. Rows a row-level security policy hides from the session's
// role do not count, so for such a role the server's own refusal at
// execution remains the backstop.
func TableHasRows(ctx context.Context, pool *pgxpool.Pool, schema, table string) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, fmt.Errorf("begin the row read of %s: %w", qualifiedName(schema, table), err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	// INV: LK-2 — the read bounds itself rather than trusting the pool's
	// session defaults.
	budgets := "SET LOCAL lock_timeout = 1; SET LOCAL statement_timeout = " +
		strconv.FormatInt(rowReadStatementTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, budgets); err != nil {
		return false, fmt.Errorf("set up the row read of %s: %w", qualifiedName(schema, table), err)
	}
	q := "SELECT EXISTS (SELECT FROM " + pgx.Identifier{schema, table}.Sanitize() + ")"
	var rows bool
	err = tx.QueryRow(ctx, q).Scan(&rows)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable:
		return false, fmt.Errorf("read whether %s holds rows: %w", qualifiedName(schema, table), ErrTableRowsLocked)
	case err != nil:
		return false, fmt.Errorf("read whether %s holds rows: %w", qualifiedName(schema, table), err)
	}
	return rows, nil
}
