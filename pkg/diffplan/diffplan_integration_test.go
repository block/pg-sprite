package diffplan_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
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

// A desired state that needs a table rewrite routes to the copy-and-swap
// backend, and the routed plan says that backend is unavailable in this
// build — a library caller sees the same honest refusal the CLI prints.
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

	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Statements, 1)
	ch := report.Statements[0]
	assert.Equal(t, planner.RouteCopyAndSwap, ch.Route)
	assert.Equal(t, router.BackendCopyAndSwap, ch.Backend)
	assert.Equal(t, router.DispositionUnavailable, ch.Disposition)
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
}
