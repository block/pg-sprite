package executor_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

// The rename names an index, so the executor binds it to the preflighted
// table in the catalog, under that table's lock, rather than on the
// statement's text. Each case is a name the planner's facts could have
// vouched for that is no longer a plain index of the table when the lock is
// held: it renames nothing and fails with the permanent renamed-index-drift
// code. A plain index of the table renames.
func TestExecuteIndexRenameChecksTheIndexUnderTheTablesLock(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id     int PRIMARY KEY,
			email  text NOT NULL,
			handle text NOT NULL,
			CONSTRAINT users_handle_key UNIQUE (handle)
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email);
		CREATE TABLE %[1]s.orders (
			id int PRIMARY KEY,
			note text
		);
		CREATE INDEX orders_note_idx ON %[1]s.orders (note)`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "users")

	refused := []struct {
		name string
		sql  string
	}{
		// The index behind a unique constraint: the rename would rename
		// the constraint with it.
		{"index backs a constraint", "ALTER INDEX %[1]s.users_handle_key RENAME TO users_username_key"},
		// An index of another table: the preflight proof is for users.
		{"index of another table", "ALTER INDEX %[1]s.orders_note_idx RENAME TO orders_memo_idx"},
		// A table: ALTER INDEX ... RENAME would rename it.
		{"name is a table", "ALTER INDEX %[1]s.users RENAME TO accounts"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			st := mustParse(t, fmt.Sprintf(tc.sql, schema))
			err := executor.ExecuteIndexRename(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())
			require.ErrorIs(t, err, executor.ErrRenamedIndexDrift)
			assert.Equal(t, executor.CodeRenamedIndexDrift, executor.OutcomeCode(err))
		})
	}
	var names []string
	rows, err := pool.Query(t.Context(), `SELECT relname FROM pg_class WHERE relnamespace = $1::regnamespace ORDER BY relname`, schema)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"orders", "orders_note_idx", "orders_pkey", "users", "users_email_idx", "users_handle_key", "users_pkey"},
		names, "no refused rename may have renamed anything")

	t.Run("plain index of the table", func(t *testing.T) {
		st := mustParse(t, fmt.Sprintf("ALTER INDEX %s.users_email_idx RENAME TO users_contact_email_idx", schema))
		require.NoError(t, executor.ExecuteIndexRename(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy()))
		var n int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_class WHERE relnamespace = $1::regnamespace AND relname = 'users_contact_email_idx'`, schema).Scan(&n))
		assert.Equal(t, 1, n)
	})
}

// ALTER INDEX ... RENAME locks only the index it renames, never the index's
// table, so the table's lock does not hold the name the statement carries
// still between the check and the rename. Here another session renames the
// checked index away, so the rename waits on that index's lock; it then
// gives the freed name to another table's primary-key index and commits.
// The waiting rename resolves the name again and renames that index, and
// with it the constraint. The executor confirms, after the rename and
// before its commit, that the index carrying the new name is the one it
// checked, so the attempt rolls back with renamed-index-drift and the other
// table's constraint keeps the name the other session gave it.
func TestExecuteIndexRenameRefusesANameMovedToAnotherIndexMidRename(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id    int PRIMARY KEY,
			email text NOT NULL
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email);
		CREATE TABLE %[1]s.orders (
			id int PRIMARY KEY
		)`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "users")

	mover, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mover.Rollback(context.WithoutCancel(t.Context())) })
	_, err = mover.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.users_email_idx RENAME TO users_email_old_idx", schema))
	require.NoError(t, err)

	// A lock budget long enough that the rename is still waiting when the
	// other session commits, and one attempt, so a lock timeout surfaces
	// as itself instead of a retry whose check would refuse on its own.
	waitBudget := executor.Budget{LockTimeout: 30 * time.Second, StatementTimeout: time.Minute}
	oneAttempt := executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}
	st := mustParse(t, fmt.Sprintf("ALTER INDEX %s.users_email_idx RENAME TO users_contact_email_idx", schema))
	done := make(chan error, 1)
	go func() {
		done <- executor.ExecuteIndexRename(t.Context(), pool, pt, st, waitBudget, oneAttempt)
	}()
	waitForRenameLockWait(t, pool, "users_contact_email_idx")
	_, err = mover.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.orders_pkey RENAME TO users_email_idx", schema))
	require.NoError(t, err)
	require.NoError(t, mover.Commit(t.Context()))

	select {
	case err := <-done:
		require.ErrorIs(t, err, executor.ErrRenamedIndexDrift)
		assert.Equal(t, executor.CodeRenamedIndexDrift, executor.OutcomeCode(err))
	case <-time.After(30 * time.Second):
		t.Fatal("the waiting rename must return once the other session commits")
	}
	var conname string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT conname FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'p'`,
		schema+".orders").Scan(&conname))
	assert.Equal(t, "users_email_idx", conname, "the other table's constraint keeps the name the other session gave it")
	var indexName string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT c.relname FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		  WHERE i.indrelid = $1::regclass AND NOT i.indisprimary`,
		schema+".users").Scan(&indexName))
	assert.Equal(t, "users_email_old_idx", indexName, "the checked index keeps the name the other session gave it")
}

// An unqualified index name resolves on the attempt's search_path, which
// holds the proof's schema alone: an index's schema is always its table's,
// so no other schema can supply the name. Here another session renames the
// checked index to exactly the statement's new name while the rename waits
// on its lock, and public holds an index of another table under the old
// name. Were public on the path, the waiting rename would resolve the old
// name there and rename that index, and the index carrying the new name in
// the proof's schema would be the checked one, so the check after the
// rename could not tell. The name resolves nowhere instead: the rename
// fails and the public index keeps its name.
func TestExecuteIndexRenameResolvesAnUnqualifiedNameInTheProofsSchemaOnly(t *testing.T) {
	pool, schema := newPool(t)
	// public is shared by every test on the server, so its names carry
	// the throwaway schema's name to stay unique.
	oldName := schema + "_email_idx"
	newName := schema + "_contact_email_idx"
	otherTable := pgx.Identifier{"public", schema + "_orders"}.Sanitize()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (
			id    int PRIMARY KEY,
			email text NOT NULL
		);
		CREATE INDEX %[2]s ON %[1]s.users (email);
		CREATE TABLE %[3]s (
			id int,
			CONSTRAINT %[2]s PRIMARY KEY (id)
		)`, schema, oldName, otherTable))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP TABLE IF EXISTS "+otherTable)
		if err != nil {
			t.Logf("drop %s: %v", otherTable, err)
		}
	})
	pt := mustPreflight(t, pool, schema, "users")

	mover, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mover.Rollback(context.WithoutCancel(t.Context())) })
	_, err = mover.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.%s RENAME TO %s", schema, oldName, newName))
	require.NoError(t, err)

	waitBudget := executor.Budget{LockTimeout: 30 * time.Second, StatementTimeout: time.Minute}
	oneAttempt := executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}
	st := mustParse(t, fmt.Sprintf("ALTER INDEX %s RENAME TO %s", oldName, newName))
	done := make(chan error, 1)
	go func() {
		done <- executor.ExecuteIndexRename(t.Context(), pool, pt, st, waitBudget, oneAttempt)
	}()
	waitForRenameLockWait(t, pool, newName)
	require.NoError(t, mover.Commit(t.Context()))

	select {
	case err := <-done:
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42P01", pgErr.Code, "undefined_table: the old name resolves nowhere on the proof's schema")
	case <-time.After(30 * time.Second):
		t.Fatal("the waiting rename must return once the other session commits")
	}
	var conname string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT conname FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'p'`, otherTable).Scan(&conname))
	assert.Equal(t, oldName, conname, "the public table's constraint keeps its name")
}

// waitForRenameLockWait waits until a session running an ALTER INDEX whose
// text ends in renamedTo is waiting on a lock.
func waitForRenameLockWait(t *testing.T, pool *pgxpool.Pool, renamedTo string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			  WHERE query LIKE 'ALTER INDEX %' || $1 AND wait_event_type = 'Lock')`, renamedTo).Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, 25*time.Millisecond, "the rename must be waiting on the index's lock")
}
