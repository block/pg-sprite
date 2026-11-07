package preflight

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
// it finds. Rows a row-level security policy hides from the session's
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
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = 1"); err != nil {
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
