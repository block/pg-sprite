package migrate_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file that drops a live foreign key is refused as destructive
// before the foreign-key check is reached, and the key stays. A declared
// foreign key that matches the live one plans no change, so the rest of the
// table's convergence runs.
func TestRunDesiredKeepsLiveForeignKeys(t *testing.T) {
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

	t.Run("runs a column add beside an unchanged foreign key", func(t *testing.T) {
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
    user_id bigint NOT NULL REFERENCES users (id),
    note text
);`)}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		require.Len(t, res.Plan.Statements, 1)
		assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders ADD COLUMN note text", schema), res.Plan.Statements[0].SQL)
		require.Len(t, res.Verdicts, 1)
		assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)

		var noteExists bool
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'orders' AND column_name = 'note'
			)`, schema).Scan(&noteExists))
		assert.True(t, noteExists, "the column add ran")
		assert.Equal(t, 1, liveForeignKeys(t, schema), "the live foreign key is untouched")
	})
}

const desiredOrdersWithForeignKey = `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id)
);`

// foreignKeySchema returns a pool and a fresh schema holding a users table
// with ids 1 and 2.
func foreignKeySchema(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id bigint PRIMARY KEY);
		INSERT INTO %[1]s.users VALUES (1), (2)`, schema))
	require.NoError(t, err)
	return pool, schema
}

// ordersForeignKeyValidated reads whether orders_user_id_fkey exists and has
// been validated.
func ordersForeignKeyValidated(t *testing.T, pool *pgxpool.Pool, schema string) (exists, validated bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `
		SELECT count(*) > 0, coalesce(bool_and(convalidated), false)
		FROM pg_catalog.pg_constraint
		WHERE conrelid = ($1 || '.orders')::regclass AND conname = 'orders_user_id_fkey' AND contype = 'f'`,
		schema).Scan(&exists, &validated)
	require.NoError(t, err)
	return exists, validated
}

// A foreign key the desired file adds to a live table with rows is applied:
// the run adds it NOT VALID, validates the existing rows, and a plan derived
// afterwards is empty, so a second run does nothing.
func TestRunDesiredAddsAForeignKeyToALiveTable(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL);
		INSERT INTO %[1]s.orders VALUES (10, 1), (11, 2)`, schema))
	require.NoError(t, err)
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredOrdersWithForeignKey)}

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
	require.Len(t, res.Plan.Statements, 1)
	assert.Equal(t, schemadiff.ChangeAddForeignKey, res.Plan.Statements[0].Kind)
	exists, validated := ordersForeignKeyValidated(t, pool, schema)
	assert.True(t, exists)
	assert.True(t, validated, "the run validates the rows it added the key over")

	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
}

// A row that breaks the new foreign key fails the validation, and nothing
// pretends otherwise: the key stays NOT VALID, so new writes are checked, and
// the run fails naming the step that committed before the validation. Once the row is fixed, the next
// run plans only the validation and completes it.
func TestRunDesiredForeignKeyOverAnOrphanRowStopsAtValidation(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL);
		INSERT INTO %[1]s.orders VALUES (10, 1), (11, 99)`, schema))
	require.NoError(t, err)
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredOrdersWithForeignKey)}

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sequence step 2 of 2 (validate-constraint) failed; steps before it committed")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "the server's own error is kept: %v", err)
	assert.Equal(t, "23503", pgErr.Code)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	exists, validated := ordersForeignKeyValidated(t, pool, schema)
	assert.True(t, exists, "the NOT VALID step committed")
	assert.False(t, validated, "the orphan row keeps the key from validating")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.orders VALUES (12, 98)", schema))
	require.True(t, errors.As(err, &pgErr), "a new orphan row is refused: %v", err)
	assert.Equal(t, "23503", pgErr.Code)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("UPDATE %s.orders SET user_id = 2 WHERE id = 11", schema))
	require.NoError(t, err)
	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	require.Len(t, res.Plan.Statements, 1)
	assert.Equal(t, schemadiff.ChangeValidateForeignKey, res.Plan.Statements[0].Kind)
	_, validated = ordersForeignKeyValidated(t, pool, schema)
	assert.True(t, validated)
}

// A new table that declares a foreign key is created with it, so the key
// guards the table's first row.
func TestRunDesiredCreatesATableWithAForeignKey(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredOrdersWithForeignKey)}

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	require.NotNil(t, res.Plan.TableExists)
	assert.False(t, *res.Plan.TableExists)
	exists, validated := ordersForeignKeyValidated(t, pool, schema)
	assert.True(t, exists)
	assert.True(t, validated)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.orders VALUES (10, 99)", schema))
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "an orphan row is refused: %v", err)
	assert.Equal(t, "23503", pgErr.Code)

	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	assert.Empty(t, res.Plan.Statements, "the created table plans no statements")
}
