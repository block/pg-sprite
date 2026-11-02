package diffplan_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// parseDesired parses a desired-state schema the way a library caller would
// before handing it to Plan.
func parseDesired(t *testing.T, sql string) statement.DesiredSchema {
	t.Helper()
	ds, err := statement.ParseDesired(sql)
	require.NoError(t, err)
	return ds
}

// The library front door derives the same ordered, classified, routed plan
// the CLI diff command prints: live facts feed the classifier, safer native
// sequences ride along, and the report is stamped with server version and
// fingerprint.
func TestPlanDerivesOrderedRoutedPlan(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint PRIMARY KEY, name varchar(20), legacy int)", schema))
	require.NoError(t, err)

	ds := parseDesired(t,
		"CREATE TABLE events (id bigint PRIMARY KEY, name varchar(50) NOT NULL);\n"+
			"CREATE INDEX events_name_idx ON events (name);")
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
	require.NoError(t, err)

	assert.Equal(t, plan.FormatVersion, report.FormatVersion)
	assert.Equal(t, plan.SourceDiff, report.Source)
	assert.Equal(t, schema, report.Schema)
	assert.Equal(t, "events", report.Table)
	assert.NotEmpty(t, report.ServerVersion)
	require.NotNil(t, report.TableExists)
	assert.True(t, *report.TableExists)

	var sqls []string
	var kinds []schemadiff.ChangeKind
	for _, ch := range report.Statements {
		sqls = append(sqls, ch.SQL)
		kinds = append(kinds, ch.Kind)
	}
	assert.Equal(t, []string{
		fmt.Sprintf("ALTER TABLE %s.events DROP legacy", schema),
		fmt.Sprintf("ALTER TABLE %s.events ALTER COLUMN name TYPE varchar(50)", schema),
		fmt.Sprintf("ALTER TABLE %s.events ALTER COLUMN name SET NOT NULL", schema),
		fmt.Sprintf("CREATE INDEX events_name_idx ON %s.events USING btree (name)", schema),
	}, sqls)
	assert.Equal(t, []schemadiff.ChangeKind{
		schemadiff.ChangeDropColumn,
		schemadiff.ChangeAlterType,
		schemadiff.ChangeSetNotNull,
		schemadiff.ChangeCreateIndex,
	}, kinds)

	assert.Equal(t, router.DispositionExecute, report.Disposition)
	for _, ch := range report.Statements {
		assert.Equal(t, planner.RouteNative, ch.Route, ch.SQL)
		assert.Equal(t, router.BackendNative, ch.Backend, ch.SQL)
		require.NotEmpty(t, ch.Decisions, ch.SQL)
	}
	assert.Equal(t, planner.ReasonBinaryCoercible, report.Statements[1].Decisions[0].Reason,
		"live column types must feed the classifier")

	require.NotEmpty(t, report.Fingerprint)
	again, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
	require.NoError(t, err)
	assert.Equal(t, report.Fingerprint, again.Fingerprint,
		"the same desired state against the same live table plans deterministically")
}

func TestPlanRefusesPartitionedParentIndexChange(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint, name text) PARTITION BY RANGE (id); "+
			"CREATE TABLE %s.events_1 PARTITION OF %s.events FOR VALUES FROM (0) TO (100)", schema, schema, schema))
	require.NoError(t, err)
	ds := parseDesired(t,
		"CREATE TABLE events (id bigint, name text) PARTITION BY RANGE (id);\n"+
			"CREATE INDEX events_name_idx ON events (name);")
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
	require.NoError(t, err)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, report.Reason)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, report.Statements[0].Reason)
	assert.Empty(t, report.Statements[0].ExecSQL)
	for _, decision := range report.Statements[0].Decisions {
		assert.Empty(t, decision.SaferSQL)
		assert.Empty(t, decision.SaferSQLExecution)
	}
}

// A stored generated column whose desired expression differs from the live
// one plans a single refused SET EXPRESSION statement. The plan shows the
// drift instead of failing, and the index built on the column is left alone:
// nothing drops and re-adds the column.
func TestPlanRefusesGeneratedExpressionChange(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.notes (id bigint PRIMARY KEY, kind int NOT NULL, body text NOT NULL, "+
			"search tsvector GENERATED ALWAYS AS (CASE WHEN kind = 1 THEN NULL::tsvector ELSE to_tsvector('simple', body) END) STORED); "+
			"CREATE INDEX notes_search_idx ON %s.notes USING gin (search)", schema, schema))
	require.NoError(t, err)

	ds := parseDesired(t,
		"CREATE TABLE notes (id bigint PRIMARY KEY, kind int NOT NULL, body text NOT NULL, "+
			"search tsvector GENERATED ALWAYS AS (CASE WHEN kind IN (1, 2) THEN NULL::tsvector ELSE to_tsvector('simple', body) END) STORED);\n"+
			"CREATE INDEX notes_search_idx ON notes USING gin (search);")
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: ds})
	require.NoError(t, err)

	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	require.Len(t, report.Statements, 1)
	ch := report.Statements[0]
	assert.Equal(t, schemadiff.ChangeSetGeneratedExpression, ch.Kind)
	assert.Contains(t, ch.SQL, `ALTER COLUMN search SET EXPRESSION AS (`)
	assert.Contains(t, ch.SQL, `ARRAY[1, 2]`)
	assert.Equal(t, planner.RouteRefuse, ch.Route)
	assert.Equal(t, router.DispositionRefuse, ch.Disposition)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, ch.Reason)
	assert.Equal(t, verdict.ClassCapabilityBoundary, ch.Class)
	assert.Empty(t, ch.ExecSQL)
}

// A desired state that needs a table rewrite routes to the copy-and-swap
// backend, and the routed plan says execution would run it there — a
// library caller sees the same routing the CLI prints.
func TestPlanRoutesRewriteToCopyAndSwap(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema:  schema,
		Desired: parseDesired(t, "CREATE TABLE events (id bigint PRIMARY KEY)"),
	})
	require.NoError(t, err)

	assert.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	ch := report.Statements[0]
	assert.Equal(t, planner.RouteCopyAndSwap, ch.Route)
	assert.Equal(t, router.BackendCopyAndSwap, ch.Backend)
	assert.Equal(t, router.DispositionExecute, ch.Disposition)
	assert.Empty(t, ch.ExecSQL, "the copy-and-swap backend lists no native SQL")
	require.Len(t, ch.Decisions, 1)
	assert.Equal(t, planner.ReasonTypeRewrite, ch.Decisions[0].Reason)
}

// Plan must never write: the live table is bit-identical before and after.
func TestPlanNeverWrites(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint PRIMARY KEY, legacy int)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"INSERT INTO %s.events SELECT g, g FROM generate_series(1, 10) g", schema))
	require.NoError(t, err)

	_, err = diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema:  schema,
		Desired: parseDesired(t, "CREATE TABLE events (id bigint PRIMARY KEY, name text NOT NULL)"),
	})
	require.NoError(t, err)

	var cols int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'events'`,
		schema).Scan(&cols))
	assert.Equal(t, 2, cols, "plan must not change the live table")
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.events", schema)).Scan(&rows))
	assert.Equal(t, 10, rows, "plan must not touch data")
}

func TestPlanNoChangesEmptyPlan(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint PRIMARY KEY, name text NOT NULL)", schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema:  schema,
		Desired: parseDesired(t, "CREATE TABLE events (id bigint PRIMARY KEY, name text NOT NULL)"),
	})
	require.NoError(t, err)

	require.NotNil(t, report.TableExists)
	assert.True(t, *report.TableExists)
	assert.Empty(t, report.Statements)
}

// seedAbandonedUniqueBuild creates schema.events with two rows sharing a
// name and runs a unique concurrent build over them. The build fails on the
// duplicates after creating its catalog entry, which leaves
// events_name_key behind as an invalid index — the leftover of an
// unfinished concurrent build.
func seedAbandonedUniqueBuild(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
CREATE TABLE %[1]s.events (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);
INSERT INTO %[1]s.events VALUES (1, 'dup'), (2, 'dup');
`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
CREATE UNIQUE INDEX CONCURRENTLY events_name_key ON %s.events (name)
`, schema))
	require.Error(t, err, "a unique build over duplicates must fail after creating its catalog entry")
}

// A desired schema that no longer names the leftover of a failed concurrent
// build plans zero statements — the diff never drops an unfinished build —
// and one invalid-index-undeclared advisory, so the empty plan does not
// read as a clean table. The advisory is explanatory: the fingerprint is
// the empty plan's.
func TestPlanUndeclaredInvalidIndexReportsAdvisory(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	seedAbandonedUniqueBuild(t, pool, schema)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema: schema,
		Desired: parseDesired(t, `
CREATE TABLE events (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);
`),
	})
	require.NoError(t, err)
	assert.Empty(t, report.Statements, "the diff never drops an unfinished build")
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	assert.Equal(t, []schemadiff.Advisory{
		{Kind: schemadiff.AdvisoryInvalidIndexUndeclared, Index: "events_name_key"},
	}, report.Advisories)
	assert.Equal(t, plan.Fingerprint(nil), report.Fingerprint, "an advisory is not part of the plan's identity")
}

// A desired schema that names the leftover of a failed concurrent build
// plans its non-destructive create-index and an invalid-index-rebuild
// advisory: the create cannot run as-is against the occupied name, and the
// plan completes only through the proven-removal path. The advisory changes
// neither the destructive gate nor the fingerprint.
func TestPlanDeclaredInvalidIndexReportsRebuildAdvisory(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	seedAbandonedUniqueBuild(t, pool, schema)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema: schema,
		Desired: parseDesired(t, `
CREATE TABLE events (
    id   bigint PRIMARY KEY,
    name text NOT NULL
);

CREATE UNIQUE INDEX events_name_key ON events (name);
`),
	})
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	assert.Equal(t, schemadiff.ChangeCreateIndex, report.Statements[0].Kind)
	assert.False(t, report.Statements[0].Destructive, "the advisory does not turn the create destructive")
	assert.Equal(t, []schemadiff.Advisory{
		{Kind: schemadiff.AdvisoryInvalidIndexRebuild, Index: "events_name_key"},
	}, report.Advisories)
	assert.Equal(t, plan.Fingerprint(report.Statements), report.Fingerprint)
}

// A table that does not exist yet plans as the full desired schema,
// qualified onto the target schema.
func TestPlanMissingTableEmitsFullDesiredSchema(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema: schema,
		Desired: parseDesired(t,
			"CREATE TABLE events (id bigint PRIMARY KEY);\nCREATE INDEX events_id_idx ON events (id);"),
	})
	require.NoError(t, err)

	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
	var sqls []string
	var kinds []schemadiff.ChangeKind
	for _, ch := range report.Statements {
		sqls = append(sqls, ch.SQL)
		kinds = append(kinds, ch.Kind)
	}
	assert.Equal(t, []string{
		fmt.Sprintf("CREATE TABLE %s.events (id bigint PRIMARY KEY)", schema),
		fmt.Sprintf("CREATE INDEX events_id_idx ON %s.events USING btree (id)", schema),
	}, sqls)
	assert.Equal(t, []schemadiff.ChangeKind{
		schemadiff.ChangeCreateTable,
		schemadiff.ChangeCreateIndex,
	}, kinds)
	assertGreenfieldIndexExecution(t, report.Statements[1])
}

func TestPlanMissingTableRefusesCreateShapes(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.parent (id int) PARTITION BY RANGE (id)", schema))
	require.NoError(t, err)

	tests := []struct {
		name    string
		sql     string
		refused []int
		cause   executor.CreateShapeCause
	}{
		{name: "partition of", sql: "CREATE TABLE child PARTITION OF parent FOR VALUES FROM (1) TO (2)", refused: []int{0}, cause: executor.CreateShapePartitionOf},
		{name: "if not exists", sql: "CREATE TABLE IF NOT EXISTS t_if (id int)", refused: []int{0}, cause: executor.CreateShapeIfNotExists},
		{name: "duplicate implicit index", sql: "CREATE TABLE t_dup (id int PRIMARY KEY); CREATE INDEX t_dup_pkey ON t_dup (id)", refused: []int{1}, cause: executor.CreateShapeDuplicateName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, tc.sql)})
			require.NoError(t, err)
			assert.Equal(t, router.DispositionRefuse, report.Disposition)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, report.Reason)
			for i, st := range report.Statements {
				want := router.DispositionExecute
				var wantCause executor.CreateShapeCause
				if slices.Contains(tc.refused, i) {
					want = router.DispositionRefuse
					wantCause = tc.cause
					assert.Equal(t, verdict.ReasonUnsupportedStatement, st.Reason)
				}
				assert.Equal(t, want, st.Disposition, "statement %d", i+1)
				assert.Equal(t, wantCause, st.Cause, "statement %d carries the create path's cause exactly when it refused it", i+1)
			}
		})
	}
}

// A greenfield plan must state execution order even when the desired file
// lists an index before its table: the CREATE TABLE is planned first, so a
// plan statement's verdict is the verdict of the create-path step at the
// same position.
func TestPlanMissingTableOrdersCreateTableFirst(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
		Schema: schema,
		Desired: parseDesired(t,
			"CREATE INDEX events_id_idx ON events (id);\nCREATE TABLE events (id bigint PRIMARY KEY);"),
	})
	require.NoError(t, err)

	require.NotNil(t, report.TableExists)
	assert.False(t, *report.TableExists)
	var sqls []string
	var kinds []schemadiff.ChangeKind
	for _, ch := range report.Statements {
		sqls = append(sqls, ch.SQL)
		kinds = append(kinds, ch.Kind)
	}
	assert.Equal(t, []string{
		fmt.Sprintf("CREATE TABLE %s.events (id bigint PRIMARY KEY)", schema),
		fmt.Sprintf("CREATE INDEX events_id_idx ON %s.events USING btree (id)", schema),
	}, sqls)
	assert.Equal(t, []schemadiff.ChangeKind{
		schemadiff.ChangeCreateTable,
		schemadiff.ChangeCreateIndex,
	}, kinds)
	assertGreenfieldIndexExecution(t, report.Statements[1])
}

func assertGreenfieldIndexExecution(t *testing.T, got plan.Statement) {
	t.Helper()
	require.Equal(t, []string{got.SQL}, got.ExecSQL)
	assert.NotContains(t, got.ExecSQL[0], "CONCURRENTLY")
	assert.Equal(t, planner.ExecutionAutocommit, got.Execution)
	require.NotEmpty(t, got.Decisions)
	for _, decision := range got.Decisions {
		assert.Equal(t, planner.ReasonMetadataOnly, decision.Reason)
		assert.Empty(t, decision.SaferSQL)
		assert.Empty(t, decision.SaferSQLExecution)
	}
}
