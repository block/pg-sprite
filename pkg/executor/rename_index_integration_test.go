package executor_test

import (
	"fmt"
	"testing"

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
