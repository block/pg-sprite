package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// Desired-state execution plans foreign-key changes but never runs them. A
// desired file that adds a foreign key, next to an otherwise executable
// column add, is refused whole with the declarative foreign-key reason, and
// the live table keeps its shape. A desired file that drops a live foreign
// key is refused as destructive before the foreign-key check is reached.
func TestRunDesiredRefusesForeignKeyChanges(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	liveForeignKeys := func(t *testing.T, schema string) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT count(*)
			FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = rel.relnamespace
			WHERE con.contype = 'f' AND n.nspname = $1 AND rel.relname = 'orders'`, schema).Scan(&n))
		return n
	}

	t.Run("refuses an added foreign key and runs nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.users (
				id bigint PRIMARY KEY
			);
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				user_id bigint NOT NULL
			)`, schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id),
    note text
);`)}, runOptions())
		require.NoError(t, err, "a foreign-key change is a refusal, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDeclarativeForeignKey, res.Reason)
		assert.Equal(t, verdict.ClassCapabilityBoundary, res.Class)
		assert.Contains(t, res.Detail, "changes a foreign key, which desired-state execution does not run yet")
		assert.Empty(t, res.Verdicts, "nothing was attempted")

		assert.Zero(t, liveForeignKeys(t, schema))
		var noteExists bool
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'note'
			)`, schema).Scan(&noteExists))
		assert.False(t, noteExists, "admission is all-or-nothing: the column add did not run either")
	})

	t.Run("refuses a dropped foreign key as destructive", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.users (
				id bigint PRIMARY KEY
			);
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				user_id bigint NOT NULL REFERENCES %[1]s.users (id)
			)`, schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL
);`)}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Empty(t, res.Verdicts)
		assert.Equal(t, 1, liveForeignKeys(t, schema))
	})

	t.Run("converges nothing when the declared foreign key matches", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.users (
				id bigint PRIMARY KEY
			);
			CREATE TABLE %[1]s.orders (
				id bigint PRIMARY KEY,
				user_id bigint NOT NULL REFERENCES %[1]s.users (id)
			)`, schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id)
);`)}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Empty(t, res.Plan.Statements)
		assert.Equal(t, 1, liveForeignKeys(t, schema))
	})
}
