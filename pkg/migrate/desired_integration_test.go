package migrate_test

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/router"
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
	serverURL := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
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

	t.Run("creates the greenfield table and re-runs as a no-op", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)

		req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}
		res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		require.NotNil(t, res.Plan.TableExists)
		assert.False(t, *res.Plan.TableExists, "the plan must record that the table was absent")
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
		assert.Empty(t, res.Plan.Statements, "the created table plans no statements")
		assert.Empty(t, res.Verdicts)
	})

	t.Run("creates from an index-first desired file and re-runs as a no-op", func(t *testing.T) {
		// The index precedes its table in the file. Run 1 must still create
		// table-first, and run 2 — where the desired file replays on the
		// scratch schema — must converge rather than error on the index
		// referencing a table that does not exist yet.
		schema := testutil.NewSchema(t, pool)

		req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t,
			"CREATE INDEX t_v_idx ON t (v);\nCREATE TABLE t (id int PRIMARY KEY, v text);")}
		res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		require.Len(t, res.Verdicts, len(res.Plan.Statements))
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
		assert.Empty(t, res.Plan.Statements, "the created table plans no statements")
		assert.Empty(t, res.Verdicts)
	})

	t.Run("refuses a create when a standalone type occupies the name", func(t *testing.T) {
		// A standalone type is not a table, so the plan is greenfield —
		// but the create would collide with the type's own composite name.
		// The absence check turns that into a typed whole-plan refusal.
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TYPE %s.t AS ENUM ('a')", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, runOptions())
		require.NoError(t, err, "an occupied name is a refusal, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonCreateCollision, res.Reason)
		assert.Empty(t, res.Verdicts, "nothing was attempted")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.False(t, exists, "the refused plan must not create the table")
	})

	t.Run("refuses a greenfield create without schema CREATE", func(t *testing.T) {
		// The role precedes the schema so LIFO cleanup drops the schema —
		// and with it the grant the role depends on — before the role.
		const password = "desired-create-password"
		role := testutil.NewRole(t, pool, "LOGIN PASSWORD '"+password+"'")
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s",
			schema, pgx.Identifier{role}.Sanitize()))
		require.NoError(t, err)
		u, err := url.Parse(serverURL)
		require.NoError(t, err)
		u.User = url.UserPassword(role, password)
		engine, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
		require.NoError(t, err)
		defer engine.Close()

		res, err := migrate.RunDesired(t.Context(), engine,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, runOptions())
		require.NoError(t, err, "a missing grant is a refusal, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonInsufficientPrivileges, res.Reason)
		assert.Contains(t, res.Detail, "GRANT CREATE ON SCHEMA",
			"the refusal names the exact provisioning statement")
		assert.Empty(t, res.Verdicts, "nothing was attempted")
	})

	t.Run("refuses a desired PARTITION OF before anything runs", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(
			"CREATE TABLE %s.events (id int, at date, PRIMARY KEY (id, at)) PARTITION BY RANGE (at)", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema: schema,
			Desired: parseDesired(t,
				"CREATE TABLE t PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')"),
		}, runOptions())
		require.NoError(t, err, "a create-path admission refusal is a result, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
		assert.Equal(t, router.DispositionRefuse, res.Plan.Disposition,
			"the refusal is decided on the plan, before the create path is entered")
		assert.Contains(t, res.Detail, "is refused by the create path: "+executor.ErrPartitionOfUnsupported.Error(),
			"the detail carries the create path's typed cause, not the echoed SQL alone")
		assert.Empty(t, res.Verdicts, "nothing was attempted")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.False(t, exists, "the refused plan must not create the partition")
	})

	t.Run("refuses a desired set that claims one name twice before anything runs", func(t *testing.T) {
		// The plan SQL says nothing about why statement 2 is refused — the
		// cause is the collision with the implicit primary-key index name,
		// and the detail carries the create path's typed cause for it.
		schema := testutil.NewSchema(t, pool)

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema:  schema,
			Desired: parseDesired(t, "CREATE TABLE t (id int PRIMARY KEY); CREATE INDEX t_pkey ON t (id)"),
		}, runOptions())
		require.NoError(t, err, "a create-path admission refusal is a result, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
		assert.Contains(t, res.Detail, "planned statement 2 (")
		assert.Contains(t, res.Detail, "is refused by the create path: "+executor.CreateShapeDuplicateName.Description())
		assert.Empty(t, res.Verdicts, "nothing was attempted")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.False(t, exists, "the refused plan must not create the table")
	})

	t.Run("refuses an occupied constraint-index name and preserves convergence", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other (v int)", schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE INDEX t_pkey ON %s.other (v)", schema))
		require.NoError(t, err)

		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, runOptions())
		require.NoError(t, err, "a catalog collision is a refusal, not an execution failure")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonCreateCollision, res.Reason)
		assert.Empty(t, res.Verdicts, "no create step ran")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.False(t, exists, "the refused set must not create the table")

		clean := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t,
			"CREATE TABLE clean (id int PRIMARY KEY, v text)")}
		res, err = migrate.RunDesired(t.Context(), pool, clean, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		res, err = migrate.RunDesired(t.Context(), pool, clean, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Empty(t, res.Plan.Statements, "a clean greenfield create converges on its second run")
	})

	t.Run("a failed index build keeps the created table and discloses the prefix", func(t *testing.T) {
		// An index on a column the table does not have passes admission
		// (shape and target) and the claimed-name probe (the name is free),
		// so the create path commits the CREATE TABLE and fails on the
		// index step — committed-prefix semantics, disclosed by the
		// verdicts: the executed create, then the failed build with the
		// failing plan statement named.
		schema := testutil.NewSchema(t, pool)
		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
				CREATE TABLE t (id int PRIMARY KEY, v text);
				CREATE INDEX t_missing_idx ON t (missing);`)}, runOptions())
		require.Error(t, err, "a mid-plan execution failure returns the failed result with the error")
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		require.Len(t, res.Verdicts, 2, "the committed create and the failed index build")
		assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)
		assert.Contains(t, res.Verdicts[0].Statement, "CREATE TABLE")
		assert.Equal(t, verdict.OutcomeFailed, res.Verdicts[1].Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), res.Verdicts[1].Code)
		assert.Contains(t, res.Verdicts[1].Statement, "t_missing_idx",
			"the failed verdict names the plan statement that failed, not the one before it")
		assert.Contains(t, res.Detail, "planned statement 2 of 2 failed",
			"the disclosure counts the committed prefix")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.True(t, exists, "the committed CREATE TABLE stays committed")
	})

	t.Run("a first-choice name taken inside the probe window fails step 1 and leaves the table", func(t *testing.T) {
		// The claimed-name probe passes because the name is free when it
		// looks; an event trigger then takes it as the CREATE TABLE begins,
		// so the server suffixes the constraint index. The CREATE TABLE has
		// committed by the time the executor can see that, so the verdict
		// is a failure at step 1 that says so — not a collision refusal
		// claiming nothing ran — and the table stays for the operator. A
		// rerun re-diffs the live table and refuses the constraint rename
		// as destructive; the operator's rename converges it.
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other (v int)", schema))
		require.NoError(t, err)
		testutil.RunDuringDDL(t, pool, testutil.DDLCommandStart, "CREATE TABLE", schema, "t",
			fmt.Sprintf("CREATE INDEX t_pkey ON %s.other (v)", schema))

		req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}
		res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.Error(t, err)
		assert.ErrorIs(t, err, executor.ErrCreateNameMismatch)
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		require.Len(t, res.Verdicts, 1, "the failed create is the only verdict; nothing before it committed")
		assert.Equal(t, verdict.OutcomeFailed, res.Verdicts[0].Outcome)
		assert.Equal(t, string(executor.CodeCreateNameMismatch), res.Verdicts[0].Code)
		assert.Contains(t, res.Verdicts[0].Statement, "CREATE TABLE")
		assert.Equal(t, 1, res.Verdicts[0].FailedStep, "a sequence stopped at its first step, not a run that never started one")
		assert.Contains(t, res.Verdicts[0].FailedStepSQL, "CREATE TABLE")
		assert.Contains(t, res.Verdicts[0].Detail, "committed and was not rolled back",
			"the verdict must not describe the step as rolled back")
		assert.Contains(t, res.Verdicts[0].Detail, `"t_pkey1"`, "the suffixed replacement is named for the operator")
		assert.Contains(t, res.Detail, "planned statement 1 of",
			"the disclosure places the failure at the first planned statement")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't')`, schema).Scan(&exists))
		assert.True(t, exists, "the executor never drops a table it has just created")

		// Retrying unchanged does not re-enter the create path: the live
		// table diffs against the desired file, and converging the
		// suffixed constraint onto its claimed name is a drop the planner
		// refuses as destructive, so nothing runs.
		res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Empty(t, res.Verdicts, "the destructive guard refuses before any statement runs")

		// The operator follows the code's remedy — free the first-choice
		// name and rename the suffixed relation onto it — and the front
		// door converges the remainder, then re-runs as a no-op.
		_, err = pool.Exec(t.Context(), fmt.Sprintf("DROP INDEX %s.t_pkey", schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf("ALTER INDEX %s.t_pkey1 RENAME TO t_pkey", schema))
		require.NoError(t, err)

		res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		require.Len(t, res.Plan.Statements, 1, "only the index the failed run never reached remains to converge")
		assert.Contains(t, res.Plan.Statements[0].SQL, "t_v_idx")

		res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Empty(t, res.Plan.Statements, "the repaired table plans no statements")
		assert.Empty(t, res.Verdicts)
	})

	t.Run("a table that cannot be read at its name after the create fails step 1 as unverified", func(t *testing.T) {
		// The CREATE TABLE commits, but an event trigger renamed the table
		// inside that same transaction, so the read of the names it owns
		// finds nothing at the claimed name. An unproven name set is not a
		// passing one: the verdict fails at step 1 under its own code, with
		// the read's cause in the chain, and the table stays where it was
		// moved for the operator.
		schema := testutil.NewSchema(t, pool)
		testutil.RunDuringDDL(t, pool, testutil.DDLCommandEnd, "CREATE TABLE", schema, "t",
			fmt.Sprintf("ALTER TABLE %s.t RENAME TO t_moved", schema))

		res, err := migrate.RunDesired(t.Context(), pool,
			migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desiredSQL)}, runOptions())
		require.Error(t, err)
		assert.ErrorIs(t, err, executor.ErrCreateNamesUnverified)
		assert.NotErrorIs(t, err, executor.ErrCreateNameMismatch, "nothing was compared, so nothing mismatched")
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		require.Len(t, res.Verdicts, 1, "the failed create is the only verdict; nothing before it committed")
		assert.Equal(t, verdict.OutcomeFailed, res.Verdicts[0].Outcome)
		assert.Equal(t, string(executor.CodeCreateNamesUnverified), res.Verdicts[0].Code)
		assert.Equal(t, 1, res.Verdicts[0].FailedStep)
		assert.Contains(t, res.Verdicts[0].FailedStepSQL, "CREATE TABLE")
		assert.Contains(t, res.Verdicts[0].Detail, "committed and was not rolled back",
			"the verdict must not describe the step as rolled back")
		assert.Contains(t, res.Verdicts[0].Detail, "unproven",
			"the verdict says the claims were not compared, not that they mismatched")

		var moved bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = $1 AND table_name = 't_moved')`, schema).Scan(&moved))
		assert.True(t, moved, "the committed table stands under the name it was moved to")
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
