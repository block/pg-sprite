package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func parseDesired(t *testing.T, sql string) statement.DesiredSchema {
	t.Helper()
	ds, err := statement.ParseDesired(sql)
	require.NoError(t, err)
	return ds
}

// RunDesired is the declarative execution loop: these tests drive the full
// plan-then-execute flow against a live database — convergence and its
// no-op re-run, the plan-time admission refusals, the fingerprint pin, and
// the committed-prefix shapes when execution stops partway.
func TestRunDesired(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	desiredSQL := `CREATE TABLE t (id int PRIMARY KEY, v text);
CREATE INDEX t_v_idx ON t (v);`

	t.Run("converges the live table and re-runs as a no-op", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}
		res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		require.Len(t, res.Verdicts, len(res.Plan.Statements),
			"every planned statement carries a verdict")
		for _, v := range res.Verdicts {
			assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		}

		var typ string
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'v'`, schema).Scan(&typ))
		assert.Equal(t, "text", typ)
		var indexValid bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT i.indisvalid FROM pg_index i
			 WHERE i.indexrelid = ($1 || '.t_v_idx')::regclass`, schema).Scan(&indexValid))
		assert.True(t, indexValid, "the index build must have completed and validated")

		// The convergence oracle: a second run derives an empty plan and
		// runs nothing.
		res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
		assert.Empty(t, res.Verdicts)
	})

	t.Run("refuses a greenfield plan and creates nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)

		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, runOptions())
		require.NoError(t, err, "a plan-time refusal is a result, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
		assert.Empty(t, res.Verdicts, "nothing was attempted")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.False(t, exists, "the refused plan must not create the table")
	})

	t.Run("refuses a destructive plan and drops nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE TABLE %s.t (id int PRIMARY KEY, v text, extra int)", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Empty(t, res.Verdicts, "a destructive plan refuses before any statement runs")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'extra')`, schema).Scan(&exists))
		assert.True(t, exists, "the live column the desired schema lacks must survive")
	})

	t.Run("refuses a plan that drops NOT NULL and keeps the guarantee", func(t *testing.T) {
		// Dropping NOT NULL discards the same guarantee as dropping the
		// equivalent constraint: the destructive guard must stop the plan
		// before anything runs, and the live guarantee must survive.
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE TABLE %s.t (id int PRIMARY KEY, v text NOT NULL)", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema:  schema,
			Desired: parseDesired(t, "CREATE TABLE t (id int PRIMARY KEY, v text)"),
		}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Empty(t, res.Verdicts, "the guard refuses before any statement runs")

		var notNull bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT is_nullable = 'NO' FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'v'`, schema).Scan(&notNull))
		assert.True(t, notNull, "the NOT NULL the desired schema dropped must survive")
	})

	t.Run("refuses a pinned fingerprint mismatch and runs nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema:              schema,
			Desired:             parseDesired(t, desiredSQL),
			ExpectedFingerprint: "not-the-reviewed-plan",
		}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
		assert.Empty(t, res.Verdicts)

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'v')`, schema).Scan(&exists))
		assert.False(t, exists, "a fingerprint mismatch must execute nothing")
	})

	t.Run("executes under a matching pinned fingerprint", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		desired := parseDesired(t, desiredSQL)
		reviewed, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: desired})
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema:              schema,
			Desired:             desired,
			ExpectedFingerprint: reviewed.Fingerprint,
		}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Equal(t, reviewed.Fingerprint, res.Plan.Fingerprint,
			"the executed plan is the reviewed plan")

		// A retry of the same pinned request after convergence is a
		// no-op, not a fingerprint mismatch: the empty plan resolves
		// before the pin is checked, so an idempotent re-run of an
		// approved plan stays safe to issue.
		res, err = migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema:              schema,
			Desired:             desired,
			ExpectedFingerprint: reviewed.Fingerprint,
		}, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome,
			"a pinned re-run of a converged table is a no-op, not a refusal")
		assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
		assert.Empty(t, res.Verdicts)
	})

	t.Run("stops at an execution-time refusal", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		// The plan admits at plan time (diffplan applies no size guard),
		// but Run size-guards the blind ADD COLUMN attempt: with the
		// threshold below one heap page the first statement refuses at
		// execution time and everything after it is never attempted.
		opts := runOptions()
		opts.MaxTableSizeBytes = 1
		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, opts)
		require.NoError(t, err, "an execution-time refusal is a result, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonTableTooLarge, res.Reason,
			"the aggregate carries the refusing statement's reason")
		require.NotEmpty(t, res.Verdicts)
		last := res.Verdicts[len(res.Verdicts)-1]
		assert.Equal(t, verdict.OutcomeRefused, last.Outcome)
		assert.Less(t, len(res.Verdicts), len(res.Plan.Statements),
			"the statements after the refusal were never attempted")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_indexes
			 WHERE schemaname = $1 AND indexname = 't_v_idx')`, schema).Scan(&exists))
		assert.False(t, exists, "the planned index after the refusal must not exist")
	})

	t.Run("returns the failed result together with the operational error", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
		require.NoError(t, err)
		// The NULL row makes the substituted safer sequence's VALIDATE
		// CONSTRAINT step fail after the scaffold CHECK ... NOT VALID
		// committed.
		_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (1, NULL)", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema:  schema,
			Desired: parseDesired(t, "CREATE TABLE t (id int PRIMARY KEY, v text NOT NULL)"),
		}, runOptions())
		require.Error(t, err, "an execution failure is an operational error")
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		require.Len(t, res.Verdicts, 1)
		assert.Equal(t, verdict.OutcomeFailed, res.Verdicts[0].Outcome,
			"the failed statement's verdict is the error's machine-readable twin")
		assert.NotEmpty(t, res.Verdicts[0].ExecutedSQL,
			"the failed verdict discloses the committed prefix inside the statement")
	})
}
