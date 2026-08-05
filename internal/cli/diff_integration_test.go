package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
)

// newDiffCmd builds a DiffCmd with the flag defaults kong would apply,
// pointing at a desired-state file written for the test.
func newDiffCmd(t *testing.T, url, schema, desiredSQL string) *DiffCmd {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.sql")
	require.NoError(t, os.WriteFile(path, []byte(desiredSQL), 0o600))
	return &DiffCmd{
		DBFlags: DBFlags{
			URL:              url,
			LockTimeout:      3 * time.Second,
			StatementTimeout: 30 * time.Second,
		},
		Desired: path,
		Schema:  schema,
	}
}

func TestDiffPrintsOrderedPlanJSON(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint PRIMARY KEY, name varchar(20), legacy int)", schema))
	require.NoError(t, err)

	cmd := newDiffCmd(t, url, schema,
		"CREATE TABLE events (id bigint PRIMARY KEY, name varchar(50) NOT NULL);\n"+
			"CREATE INDEX events_name_idx ON events (name);")
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var report diffReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, schema, report.Schema)
	assert.Equal(t, "events", report.Table)
	assert.True(t, report.TableExists)

	var sqls []string
	var destructive []bool
	for _, ch := range report.Changes {
		sqls = append(sqls, ch.SQL)
		destructive = append(destructive, ch.Destructive)
	}
	assert.Equal(t, []string{
		fmt.Sprintf(`ALTER TABLE "%s"."events" DROP COLUMN "legacy"`, schema),
		fmt.Sprintf(`ALTER TABLE "%s"."events" ALTER COLUMN "name" TYPE character varying(50)`, schema),
		fmt.Sprintf(`ALTER TABLE "%s"."events" ALTER COLUMN "name" SET NOT NULL`, schema),
		fmt.Sprintf("CREATE INDEX events_name_idx ON %s.events USING btree (name)", schema),
	}, sqls)
	assert.Equal(t, []bool{true, false, false, false}, destructive)

	// Every derived statement is classified and routed: the widen is proven
	// binary-coercible by the live facts, SET NOT NULL and CREATE INDEX
	// carry their safer native sequences, and the whole plan would execute.
	assert.Equal(t, router.DispositionExecute, report.Disposition)
	routes := make([]planner.Route, 0, len(report.Changes))
	for _, ch := range report.Changes {
		routes = append(routes, ch.Route)
		assert.Equal(t, router.BackendNative, ch.Backend, ch.SQL)
		assert.Equal(t, router.DispositionExecute, ch.Disposition, ch.SQL)
		require.NotEmpty(t, ch.Decisions, ch.SQL)
	}
	assert.Equal(t, []planner.Route{
		planner.RouteNative, planner.RouteNative, planner.RouteNative, planner.RouteNative,
	}, routes)
	assert.Equal(t, planner.ReasonBinaryCoercible, report.Changes[1].Decisions[0].Reason,
		"live column types must feed the classifier")
	assert.Equal(t, planner.ReasonSaferIdiom, report.Changes[2].Decisions[0].Reason)
	assert.NotEqual(t, []string{report.Changes[2].SQL}, report.Changes[2].ExecSQL,
		"SET NOT NULL carries its safer native sequence")
	assert.Equal(t, planner.ReasonSaferIdiom, report.Changes[3].Decisions[0].Reason)
	require.Len(t, report.Changes[3].ExecSQL, 1)
	assert.NotEqual(t, report.Changes[3].SQL, report.Changes[3].ExecSQL[0],
		"CREATE INDEX carries its concurrent rewrite")
}

// A desired state that needs a table rewrite routes to the copy-and-swap
// backend, and the routed plan says that backend is unavailable in this
// build — the plan is honest about what execution would do.
func TestDiffRoutesRewriteToCopyAndSwap(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	cmd := newDiffCmd(t, url, schema, "CREATE TABLE events (id bigint PRIMARY KEY)")
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var report diffReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.Len(t, report.Changes, 1)
	ch := report.Changes[0]
	assert.Equal(t, planner.RouteCopyAndSwap, ch.Route)
	assert.Equal(t, router.BackendCopyAndSwap, ch.Backend)
	assert.Equal(t, router.DispositionUnavailable, ch.Disposition)
	assert.Empty(t, ch.ExecSQL)
	require.Len(t, ch.Decisions, 1)
	assert.Equal(t, planner.ReasonTypeRewrite, ch.Decisions[0].Reason)
}

// diff must never write: the live table is bit-identical before and after.
func TestDiffNeverWrites(t *testing.T) {
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

	cmd := newDiffCmd(t, url, schema, "CREATE TABLE events (id bigint PRIMARY KEY, name text NOT NULL)")
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var cols int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'events'`,
		schema).Scan(&cols))
	assert.Equal(t, 2, cols, "diff must not change the live table")
	var rows int
	require.NoError(t, pool.QueryRow(t.Context(),
		fmt.Sprintf("SELECT count(*) FROM %s.events", schema)).Scan(&rows))
	assert.Equal(t, 10, rows, "diff must not touch data")
}

func TestDiffNoChangesEmptyPlan(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint PRIMARY KEY, name text NOT NULL)", schema))
	require.NoError(t, err)

	cmd := newDiffCmd(t, url, schema, "CREATE TABLE events (id bigint PRIMARY KEY, name text NOT NULL)")
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var report diffReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.True(t, report.TableExists)
	assert.Empty(t, report.Changes)
}

func TestDiffMissingTableEmitsFullDesiredSchema(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	cmd := newDiffCmd(t, url, schema,
		"CREATE TABLE events (id bigint PRIMARY KEY);\nCREATE INDEX events_id_idx ON events (id);")
	cmd.JSON = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	var report diffReport
	require.NoError(t, json.Unmarshal([]byte(out.String()), &report))
	assert.False(t, report.TableExists)
	var sqls []string
	for _, ch := range report.Changes {
		sqls = append(sqls, ch.SQL)
	}
	assert.Equal(t, []string{
		fmt.Sprintf("CREATE TABLE %s.events (id bigint PRIMARY KEY)", schema),
		fmt.Sprintf("CREATE INDEX events_id_idx ON %s.events USING btree (id)", schema),
	}, sqls)
}

func TestDiffTextPlanIsExecutableSQL(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.events (id bigint PRIMARY KEY, legacy int)", schema))
	require.NoError(t, err)

	cmd := newDiffCmd(t, url, schema, "CREATE TABLE events (id bigint PRIMARY KEY, name text NOT NULL)")
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))

	// The text plan is an executable script: running it converges the table.
	_, err = pool.Exec(t.Context(), out.String())
	require.NoError(t, err, "text plan must be executable SQL: %s", out.String())

	cmd2 := newDiffCmd(t, url, schema, "CREATE TABLE events (id bigint PRIMARY KEY, name text NOT NULL)")
	cmd2.JSON = true
	var out2 strings.Builder
	require.NoError(t, cmd2.run(t.Context(), &out2))
	var report diffReport
	require.NoError(t, json.Unmarshal([]byte(out2.String()), &report))
	assert.Empty(t, report.Changes, "executing the text plan must converge the table")
}
