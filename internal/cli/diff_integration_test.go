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
