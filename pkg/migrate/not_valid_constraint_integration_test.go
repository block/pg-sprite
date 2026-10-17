package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired check constraint is added online as NOT VALID and then
// validated. When a row violates it, the validation fails after the NOT
// VALID constraint committed, which leaves the live table carrying the
// declared constraint unvalidated. Once the operator fixes the row, the next
// run converges by validating that constraint: one non-destructive
// statement, no drop of the constraint the first run added, and a third run
// plans nothing.
func TestRunDesiredValidatesTheConstraintAFailedValidationLeftBehind(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	table := pgx.Identifier{schema, "t"}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s (id int PRIMARY KEY, qty int);
		INSERT INTO %[1]s VALUES (1, -1)`, table))
	require.NoError(t, err)

	req := migrate.DesiredRequest{
		Schema:  schema,
		Desired: parseDesired(t, "CREATE TABLE t (id int PRIMARY KEY, qty int, CONSTRAINT t_qty_check CHECK (qty > 0))"),
	}
	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.Error(t, err, "the violating row fails the validation")
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	require.Equal(t, []bool{false}, constraintValidity(t, pool, schema, "t_qty_check"),
		"the constraint committed NOT VALID before its validation failed")

	_, err = pool.Exec(t.Context(), fmt.Sprintf("UPDATE %s SET qty = 1", table))
	require.NoError(t, err)

	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	require.Len(t, res.Plan.Statements, 1)
	st := res.Plan.Statements[0]
	assert.Equal(t, schemadiff.ChangeValidateConstraint, st.Kind)
	assert.Equal(t, fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT t_qty_check", schema), st.SQL)
	assert.False(t, st.Destructive)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)
	assert.Equal(t, []bool{true}, constraintValidity(t, pool, schema, "t_qty_check"),
		"the same constraint is validated, not dropped and re-added")

	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
}

// constraintValidity lists convalidated for every constraint of the name on
// schema.t, so a test sees both whether the constraint exists and whether
// the server has validated it.
func constraintValidity(t *testing.T, pool *pgxpool.Pool, schema, name string) []bool {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT c.convalidated FROM pg_constraint c
		WHERE c.conrelid = ($1 || '.t')::regclass AND c.conname = $2`,
		pgx.Identifier{schema}.Sanitize(), name)
	require.NoError(t, err)
	validity, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	require.NoError(t, err)
	return validity
}
