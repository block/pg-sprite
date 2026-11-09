package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// documentsWithRowSecurity is a desired file for t with the row security the
// rowSecurityTable fixture creates and the table definition columns names.
const documentsWithRowSecurity = `CREATE TABLE t (
  id int PRIMARY KEY,
  owner_id int NOT NULL%s
);%s
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON t FOR SELECT USING (owner_id = 7);`

// rowSecurityTable returns a pool and a schema holding t with row security
// enabled and one policy, the table documentsWithRowSecurity declares.
func rowSecurityTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %[1]s.t (id int PRIMARY KEY, owner_id int NOT NULL);
ALTER TABLE %[1]s.t ENABLE ROW LEVEL SECURITY;
CREATE POLICY readers ON %[1]s.t FOR SELECT USING (owner_id = 7);`, schema))
	require.NoError(t, err)
	return pool, schema
}

func parseRowSecurity(t *testing.T, sql string) statement.DesiredWithRowSecurity {
	t.Helper()
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	return desired
}

// An operator adds a column and an index to a table whose desired file also
// declares its row security, which already matches the live table. The plan
// is the table definition's own plan, the run converges the table without
// touching a policy, and a re-run plans nothing.
func TestRunDesiredWithRowSecurityChangesOnlyTheTable(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	desired := parseRowSecurity(t, fmt.Sprintf(documentsWithRowSecurity, ",\n  title text", "\nCREATE INDEX t_title_idx ON t (title);"))

	planned, err := diffplan.PlanWithRowSecurity(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	tableOnly, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: desired.TableDefinition()})
	require.NoError(t, err)
	assert.Equal(t, tableOnly, planned, "the plan is the table definition's plan")
	require.Len(t, planned.Statements, 2)
	assert.Equal(t, "ALTER TABLE "+schema+".t ADD COLUMN title text", planned.Statements[0].SQL)
	assert.Equal(t, "CREATE INDEX t_title_idx ON "+schema+".t USING btree (title)", planned.Statements[1].SQL)

	req := migrate.RowSecurityRequest{Schema: schema, Desired: desired, ExpectedFingerprint: planned.Fingerprint}
	res, err := migrate.RunDesiredWithRowSecurity(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
	assert.Equal(t, "converged: all 2 planned statements committed, a fresh plan is empty, and the row security still matches the file", res.Detail)
	require.Len(t, res.Verdicts, 2)
	for _, v := range res.Verdicts {
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	}
	assert.True(t, hasColumn(t, pool, schema, "title"))
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	assert.Equal(t, before.RowSecurity, after.RowSecurity, "the row security is unchanged")

	again, err := migrate.RunDesiredWithRowSecurity(t.Context(), pool, migrate.RowSecurityRequest{Schema: schema, Desired: desired}, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, again.Outcome)
	assert.Empty(t, again.Plan.Statements)
	assert.Empty(t, again.Verdicts)
	assert.Equal(t, "already converged: the live table and its row security match the desired file; nothing to run", again.Detail)
}

// This path changes only the table, so a file whose row security differs from
// the live table is refused before anything runs, with or without table
// changes, and so is a file for a table that does not exist: the row security
// executor creates it with its row security in one transaction.
func TestRunDesiredWithRowSecurityRefusesWhatItDoesNotChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sql    string
		reason verdict.Reason
		class  verdict.Class
	}{
		{
			name:   "the row security differs",
			sql:    `CREATE TABLE t (id int PRIMARY KEY, owner_id int NOT NULL, title text); ALTER TABLE t ENABLE ROW LEVEL SECURITY;`,
			reason: verdict.ReasonUnsupportedStatement,
			class:  verdict.ClassCapabilityBoundary,
		},
		{
			name:   "the table does not exist",
			sql:    `CREATE TABLE missing (id int PRIMARY KEY); ALTER TABLE missing ENABLE ROW LEVEL SECURITY;`,
			reason: verdict.ReasonUnsupportedStatement,
			class:  verdict.ClassEnvironmental,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := rowSecurityTable(t)
			before, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
			require.NoError(t, err)
			res, err := migrate.RunDesiredWithRowSecurity(t.Context(), pool, migrate.RowSecurityRequest{Schema: schema, Desired: parseRowSecurity(t, tc.sql)}, runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
			assert.Equal(t, tc.reason, res.Reason)
			assert.Equal(t, tc.class, res.Class)
			assert.Contains(t, res.Detail, "executor.ExecuteDesiredRowSecurity")
			assert.Empty(t, res.Verdicts)
			after, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
			require.NoError(t, err)
			assert.Equal(t, before, after, "nothing ran")
		})
	}
}

// A table change the table's own plan refuses, here a serial column, is
// refused in the terms a file without row security gets, not reported as a
// row security difference: the row security matches, so the error names the
// column, and nothing runs.
func TestRunDesiredWithRowSecurityReportsTheTablesOwnRefusal(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	desired := parseRowSecurity(t, fmt.Sprintf(documentsWithRowSecurity, ",\n  seq serial", ""))

	_, err = diffplan.PlanWithRowSecurity(t.Context(), pool, schema, desired)
	require.ErrorIs(t, err, schemadiff.ErrOnlyTableDiffers)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	var review *diffplan.RowSecurityReviewRequired
	assert.NotErrorAs(t, err, &review, "the row security matches")
	_, tableErr := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: desired.TableDefinition()})
	require.Error(t, tableErr)
	assert.ErrorContains(t, err, tableErr.Error(), "the error is the table definition's own refusal")

	_, err = migrate.RunDesiredWithRowSecurity(t.Context(), pool, migrate.RowSecurityRequest{Schema: schema, Desired: desired}, runOptions())
	require.ErrorIs(t, err, schemadiff.ErrOnlyTableDiffers)
	assert.ErrorContains(t, err, tableErr.Error())
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	assert.Equal(t, before, after, "nothing ran")
}

// Another session can change the policies while the table plan runs. Here an
// event trigger replaces the policy once the run's ADD COLUMN commits. The
// run does not claim convergence: it fails with ErrNotConverged, says the row
// security no longer matches, and reports the column as committed.
func TestRunDesiredWithRowSecurityFailsWhenThePoliciesChangeDuringTheRun(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	testutil.InstallEventTrigger(t, pool, testutil.DDLCommandEnd, schema, "widen_policy", fmt.Sprintf(`
		IF TG_TAG = 'ALTER TABLE'
			AND EXISTS (SELECT FROM pg_catalog.pg_event_trigger_ddl_commands() WHERE object_identity = '%[1]s.t')
			AND EXISTS (SELECT FROM pg_catalog.pg_policy p
				JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
				JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = '%[1]s' AND c.relname = 't' AND p.polname = 'readers') THEN
			DROP POLICY readers ON %[1]s.t;
			CREATE POLICY everyone ON %[1]s.t FOR SELECT USING (true);
		END IF;`, schema))

	desired := parseRowSecurity(t, fmt.Sprintf(documentsWithRowSecurity, ",\n  title text", ""))
	res, err := migrate.RunDesiredWithRowSecurity(t.Context(), pool, migrate.RowSecurityRequest{Schema: schema, Desired: desired}, runOptions())
	require.ErrorIs(t, err, migrate.ErrNotConverged)
	assert.ErrorContains(t, err, "the row security on "+schema+".t no longer matches the file")
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceUnverifiable}, res.Convergence)
	assert.Contains(t, res.Detail, "all 1 planned statements committed and remain in effect")
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, verdict.OutcomeExecuted, res.Verdicts[0].Outcome)
	assert.True(t, hasColumn(t, pool, schema, "title"), "the planned statement stays committed")
}
