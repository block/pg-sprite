package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file that drops a live foreign key is refused as destructive
// without the caller's acknowledgement, and the key stays. A declared
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

	t.Run("refuses a changed foreign key as destructive and keeps the live one", func(t *testing.T) {
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

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, ordersCascade)}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Empty(t, res.Verdicts)
		assert.Equal(t, 1, liveForeignKeys(t, schema))
		var onDelete string
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT con.confdeltype::text
			FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = rel.relnamespace
			WHERE con.contype = 'f' AND n.nspname = $1 AND rel.relname = 'orders'`, schema).Scan(&onDelete))
		assert.Equal(t, "a", onDelete, "the live key keeps its NO ACTION delete rule")
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

// A foreign key the desired file no longer declares is dropped once the
// caller acknowledges the destructive change, as the revert of the change
// that added it plans. The key stops checking writes, and a plan derived
// afterwards is empty.
func TestRunDesiredDropsAForeignKeyTheFileNoLongerDeclares(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES %[1]s.users (id));
		INSERT INTO %[1]s.orders VALUES (10, 1)`, schema))
	require.NoError(t, err)
	req := reviewedDestructiveRequest(t, pool, schema, `CREATE TABLE orders (id bigint PRIMARY KEY, user_id bigint NOT NULL);`)

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
	require.Len(t, res.Plan.Statements, 1)
	assert.Equal(t, schemadiff.ChangeDropForeignKey, res.Plan.Statements[0].Kind)
	exists, _ := ordersForeignKeyValidated(t, pool, schema)
	assert.False(t, exists, "the run dropped the key")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.orders VALUES (11, 99)", schema))
	require.NoError(t, err, "the dropped key no longer checks writes")

	res, err = migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: req.Desired}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
}

// reviewedDestructiveRequest plans the desired orders file and returns the
// request a caller makes after reviewing that plan: pinned to its
// fingerprint, with the destructive change on orders acknowledged.
func reviewedDestructiveRequest(t *testing.T, pool *pgxpool.Pool, schema, desired string) migrate.DesiredRequest {
	t.Helper()
	ds := parseDesired(t, desired)
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
	require.NoError(t, err)
	return migrate.DesiredRequest{
		Schema:              schema,
		Desired:             ds,
		ExpectedFingerprint: report.Fingerprint,
		AllowDestructive:    schema + ".orders",
	}
}

// A file that moves the orders key to a new name and adds ON DELETE CASCADE
// runs as the add of the new key, NOT VALID then validated, and only then the
// drop of the old one, so every write is checked against a key throughout.
func TestRunDesiredChangesAForeignKeyUnderANewName(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES %[1]s.users (id));
		INSERT INTO %[1]s.orders VALUES (10, 1), (11, 2)`, schema))
	require.NoError(t, err)
	req := reviewedDestructiveRequest(t, pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL CONSTRAINT orders_user_id_cascade_fkey REFERENCES users (id) ON DELETE CASCADE
);`)

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
	require.Len(t, res.Plan.Statements, 2)
	assert.Equal(t, schemadiff.ChangeAddForeignKey, res.Plan.Statements[0].Kind)
	assert.Equal(t, schemadiff.ChangeDropForeignKey, res.Plan.Statements[1].Kind)
	exists, _ := ordersForeignKeyValidated(t, pool, schema)
	assert.False(t, exists, "the old key is gone")
	var onDelete string
	var validated bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT confdeltype, convalidated FROM pg_catalog.pg_constraint
		WHERE conrelid = ($1 || '.orders')::regclass AND conname = 'orders_user_id_cascade_fkey'`, schema).Scan(&onDelete, &validated))
	assert.Equal(t, "c", onDelete, "the new key cascades deletes")
	assert.True(t, validated, "the run validates the rows under the new key")
}

// A key moved to a new name whose rule the existing rows break stops at its
// validation with both keys in place: the old key still valid, and the new
// one NOT VALID but checking every new write. Once the rows are fixed, the
// next run validates the new key before it drops the old one.
func TestRunDesiredForeignKeyMovedToANewNameStopsAtValidationWithBothKeys(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.premium_users (id bigint PRIMARY KEY);
		INSERT INTO %[1]s.premium_users VALUES (1);
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES %[1]s.users (id));
		INSERT INTO %[1]s.orders VALUES (10, 1), (11, 2)`, schema))
	require.NoError(t, err)
	const desired = `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL CONSTRAINT orders_user_id_premium_fkey REFERENCES premium_users (id)
);`
	newKeyValidated := func() (exists, validated bool) {
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT count(*) > 0, coalesce(bool_and(convalidated), false) FROM pg_catalog.pg_constraint
			WHERE conrelid = ($1 || '.orders')::regclass AND conname = 'orders_user_id_premium_fkey'`, schema).Scan(&exists, &validated))
		return exists, validated
	}

	res, err := migrate.RunDesired(t.Context(), pool, reviewedDestructiveRequest(t, pool, schema, desired), runOptions())

	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "the server's own error is kept: %v", err)
	assert.Equal(t, "23503", pgErr.Code)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	oldExists, oldValidated := ordersForeignKeyValidated(t, pool, schema)
	assert.True(t, oldExists && oldValidated, "the old key still holds")
	newExists, newValidated := newKeyValidated()
	assert.True(t, newExists, "the new key's NOT VALID step committed")
	assert.False(t, newValidated, "user 2 keeps the new key from validating")
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.orders VALUES (12, 2)", schema))
	require.True(t, errors.As(err, &pgErr), "the new key checks new writes: %v", err)
	assert.Equal(t, "23503", pgErr.Code)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.premium_users VALUES (2)", schema))
	require.NoError(t, err)
	res, err = migrate.RunDesired(t.Context(), pool, reviewedDestructiveRequest(t, pool, schema, desired), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	require.Len(t, res.Plan.Statements, 2)
	assert.Equal(t, schemadiff.ChangeValidateForeignKey, res.Plan.Statements[0].Kind, "the new key is validated first")
	assert.Equal(t, schemadiff.ChangeDropForeignKey, res.Plan.Statements[1].Kind)
	oldExists, _ = ordersForeignKeyValidated(t, pool, schema)
	assert.False(t, oldExists, "the old key is gone")
	_, newValidated = newKeyValidated()
	assert.True(t, newValidated)
}

// A file that changes the orders key under the same name is refused before
// anything runs: the old key would be dropped before the new one could be
// added, and writes between the two would be checked against no key.
func TestRunDesiredRefusesAForeignKeyChangedUnderTheSameName(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES %[1]s.users (id));
		INSERT INTO %[1]s.orders VALUES (10, 1), (11, 2)`, schema))
	require.NoError(t, err)
	req := reviewedDestructiveRequest(t, pool, schema, `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE
);`)

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, res.Detail)
	assert.Equal(t, verdict.ReasonDeclarativeForeignKey, res.Reason)
	exists, validated := ordersForeignKeyValidated(t, pool, schema)
	assert.True(t, exists, "the live key stays")
	assert.True(t, validated)
	var onDelete string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT confdeltype FROM pg_catalog.pg_constraint
		WHERE conrelid = ($1 || '.orders')::regclass AND conname = 'orders_user_id_fkey'`, schema).Scan(&onDelete))
	assert.Equal(t, "a", onDelete, "the live key is unchanged")
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

// A new table's foreign key binds to the referenced table in the target
// schema, never to a table of the same name the session's search_path
// reaches elsewhere. When the target schema has no such table, nothing is
// created: the run fails before the create, and the table stays absent.
func TestRunDesiredCreateBindsForeignKeysToTheTargetSchema(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	elsewhere := "fk_elsewhere_" + schema
	_, err := pool.Exec(t.Context(), "CREATE TABLE public."+pgx.Identifier{elsewhere}.Sanitize()+" (id bigint PRIMARY KEY)")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP TABLE IF EXISTS public."+pgx.Identifier{elsewhere}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	desired := fmt.Sprintf(`
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES %s (id)
);`, pgx.Identifier{elsewhere}.Sanitize())
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desired)}

	_, err = migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.Error(t, err)
	assert.ErrorIs(t, err, schemadiff.ErrReferencedTableNotFound)
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", schema+".orders").Scan(&exists))
	assert.False(t, exists, "a key that would bind outside the target schema creates nothing")
}

// A desired file that adds a primary key and a foreign key referencing it on
// the same live table runs the key first, so the foreign key has the key it
// references when it is added.
func TestRunDesiredAddsAPrimaryKeyBeforeTheForeignKeyThatReferencesIt(t *testing.T) {
	pool, schema := foreignKeySchema(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.employees (id bigint NOT NULL, manager_id bigint);
		INSERT INTO %[1]s.employees VALUES (1, NULL), (2, 1)`, schema))
	require.NoError(t, err)
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE employees (
    id bigint NOT NULL,
    manager_id bigint,
    CONSTRAINT employees_pkey PRIMARY KEY (id),
    CONSTRAINT employees_manager_id_fkey FOREIGN KEY (manager_id) REFERENCES employees (id)
);`)}

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	var kinds []schemadiff.ChangeKind
	for _, st := range res.Plan.Statements {
		kinds = append(kinds, st.Kind)
	}
	assert.Equal(t, schemadiff.ChangeAddForeignKey, kinds[len(kinds)-1], "the foreign key comes after the key it references: %v", kinds)
	var validated bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT convalidated FROM pg_catalog.pg_constraint
		WHERE conrelid = ($1 || '.employees')::regclass AND conname = 'employees_manager_id_fkey'`, schema).Scan(&validated))
	assert.True(t, validated)
}

// A self-referencing foreign key and the key it rests on, a primary key or a
// unique constraint, both leave the desired file. The plan drops the foreign
// key first, since PostgreSQL refuses to drop the key while the foreign key
// still depends on it, and the run converges. A key that rests on a
// standalone unique index cannot be declared, so its drop is refused.
func TestRunDesiredDropsASelfReferencingForeignKeyBeforeItsKey(t *testing.T) {
	for _, tt := range []struct {
		name    string
		live    string
		desired string
		keyDrop string
	}{
		{
			name:    "primary key",
			live:    `CREATE TABLE %[1]s.orders (id bigint CONSTRAINT orders_pkey PRIMARY KEY, parent_id bigint CONSTRAINT orders_parent_fkey REFERENCES %[1]s.orders (id))`,
			desired: `CREATE TABLE orders (id bigint NOT NULL, parent_id bigint);`,
			keyDrop: `ALTER TABLE %s.orders DROP CONSTRAINT orders_pkey`,
		},
		{
			name:    "unique constraint",
			live:    `CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, code text CONSTRAINT orders_code_key UNIQUE, parent_code text CONSTRAINT orders_parent_fkey REFERENCES %[1]s.orders (code))`,
			desired: `CREATE TABLE orders (id bigint PRIMARY KEY, code text, parent_code text);`,
			keyDrop: `ALTER TABLE %s.orders DROP CONSTRAINT orders_code_key`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool, schema := foreignKeySchema(t)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tt.live, schema))
			require.NoError(t, err)
			req := reviewedDestructiveRequest(t, pool, schema, tt.desired)

			plan, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: req.Desired})
			require.NoError(t, err)
			var drops []string
			for _, st := range plan.Statements {
				if st.Drops != nil {
					drops = append(drops, st.SQL)
				}
			}
			require.Len(t, drops, 2, "the foreign key and its key are both dropped")
			assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.orders DROP CONSTRAINT orders_parent_fkey", schema), drops[0], "the foreign key drops first")
			assert.Equal(t, fmt.Sprintf(tt.keyDrop, schema), drops[1])

			res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
			assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
		})
	}
}

// ordersCascade declares orders with its user key changed to ON DELETE
// CASCADE.
const ordersCascade = `
CREATE TABLE orders (
    id bigint PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE
);`
