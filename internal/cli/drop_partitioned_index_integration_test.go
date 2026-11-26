package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/verdict"
)

// runMigrate runs one migrate command and decodes the verdict it printed.
func runMigrate(t *testing.T, cmd *MigrateCmd) (verdict.Verdict, error) {
	t.Helper()
	cmd.JSON = true
	var out strings.Builder
	err := cmd.run(t.Context(), &out)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v), "output: %s", out.String())
	return v, err
}

// cliIndexExists reports whether the named index is present in the schema.
func cliIndexExists(t *testing.T, pool *pgxpool.Pool, schema, index string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = $2)`,
		schema, index).Scan(&exists))
	return exists
}

// A single-index DROP INDEX is refused on apply, and which refusal depends
// on the index: a partitioned table's index has no concurrent drop, and a
// partition's index attached under it cannot be dropped alone. The command
// dials to read the index's kind, and its dry run reads the same catalog,
// so the two print the same refusal and exit with the refusal code. A plain
// table's index keeps the refusal it always had.
func TestMigrateDropIndexOfPartitionedTable(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	// PostgreSQL makes ev_name_idx a partitioned index and builds
	// ev_1_name_idx on the partition, attached under it.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.ev (
			id bigint NOT NULL,
			name text,
			day date NOT NULL
		) PARTITION BY RANGE (day);
		CREATE TABLE %[1]s.ev_1 PARTITION OF %[1]s.ev
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
		CREATE INDEX ev_name_idx ON %[1]s.ev (name);

		CREATE TABLE %[1]s.users (
			id int PRIMARY KEY,
			email text
		);
		CREATE INDEX users_email_idx ON %[1]s.users (email)`, schema))
	require.NoError(t, err)

	cases := []struct {
		name       string
		sql        string
		reason     verdict.Reason
		cause      verdict.Cause
		saferIdiom string
		table      string
	}{
		{
			name:   "plain drop of a partitioned index names the bounded plain drop, not CONCURRENTLY",
			sql:    fmt.Sprintf("DROP INDEX %s.ev_name_idx", schema),
			reason: verdict.ReasonUnsupportedPartitionedParent,
			cause:  verdict.CauseParentConcurrentIndexDrop,
			table:  schema + ".ev",
		},
		{
			name:   "concurrent drop of a partitioned index is refused, not run directly",
			sql:    fmt.Sprintf("DROP INDEX CONCURRENTLY %s.ev_name_idx", schema),
			reason: verdict.ReasonUnsupportedPartitionedParent,
			cause:  verdict.CauseParentConcurrentIndexDrop,
			table:  schema + ".ev",
		},
		{
			name:   "plain drop of an attached partition index names the parent index",
			sql:    fmt.Sprintf("DROP INDEX %s.ev_1_name_idx", schema),
			reason: verdict.ReasonUnsupportedStatement,
			table:  schema + ".ev_1",
		},
		{
			name:   "concurrent drop of an attached partition index names the parent index",
			sql:    fmt.Sprintf("DROP INDEX CONCURRENTLY %s.ev_1_name_idx", schema),
			reason: verdict.ReasonUnsupportedStatement,
			table:  schema + ".ev_1",
		},
		{
			name:       "plain drop of a plain table's index keeps the concurrent idiom",
			sql:        fmt.Sprintf("DROP INDEX %s.users_email_idx", schema),
			reason:     verdict.ReasonIndexStatement,
			saferIdiom: "DROP INDEX CONCURRENTLY",
		},
		{
			name:   "concurrent drop of a plain table's index is run directly",
			sql:    fmt.Sprintf("DROP INDEX CONCURRENTLY %s.users_email_idx", schema),
			reason: verdict.ReasonIndexStatement,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied, err := runMigrate(t, newMigrateCmd(url, tc.sql))
			require.ErrorIs(t, err, verdict.ErrRefused)
			assert.Equal(t, verdict.OutcomeRefused, applied.Outcome)
			assert.Equal(t, tc.reason, applied.Reason)
			assert.Equal(t, tc.cause, applied.Cause)
			assert.Equal(t, tc.saferIdiom, applied.SaferIdiom)
			assert.Equal(t, tc.table, applied.Table)

			dry := newMigrateCmd(url, tc.sql)
			dry.DryRun = true
			planned, err := runMigrate(t, dry)
			require.ErrorIs(t, err, verdict.ErrRefused)
			assert.Equal(t, applied, planned, "the dry run must end in the apply's refusal")
		})
	}
	assert.True(t, cliIndexExists(t, pool, schema, "ev_name_idx"), "a refusal executes nothing")
	assert.True(t, cliIndexExists(t, pool, schema, "users_email_idx"), "a refusal executes nothing")

	// The concurrent forms stay refused under the acknowledgements: the
	// passthrough runs the submitted statement inside a transaction, which
	// no concurrent drop can run in.
	for _, sql := range []string{
		fmt.Sprintf("DROP INDEX CONCURRENTLY %s.ev_name_idx", schema),
		fmt.Sprintf("DROP INDEX CONCURRENTLY %s.ev_1_name_idx", schema),
		fmt.Sprintf("DROP INDEX %s.ev_1_name_idx", schema),
	} {
		t.Run("acknowledged "+sql, func(t *testing.T) {
			cmd := newMigrateCmd(url, sql)
			cmd.AcceptBlocking = schema + ".ev"
			cmd.AllowDestructive = schema + ".ev"
			v, err := runMigrate(t, cmd)
			require.ErrorIs(t, err, verdict.ErrRefused)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.False(t, v.BlockingPassthrough)
		})
	}

	// The plain drop of the partitioned index is the one PostgreSQL
	// accepts: under both acknowledgements it runs, exits with the
	// accepted-blocking code (3), and drops the partition's index with it.
	t.Run("plain drop of a partitioned index runs under both acknowledgements", func(t *testing.T) {
		cmd := newMigrateCmd(url, fmt.Sprintf("DROP INDEX %s.ev_name_idx", schema))
		cmd.AcceptBlocking = schema + ".ev"
		cmd.AllowDestructive = schema + ".ev"
		v, err := runMigrate(t, cmd)
		require.ErrorIs(t, err, verdict.ErrAcceptedBlocking)
		assert.Equal(t, verdict.OutcomeExecutedWithoutOnlineSafety, v.Outcome)
		assert.True(t, v.BlockingPassthrough)
		assert.True(t, v.AllowedDestructive)
		assert.Equal(t, verdict.ReasonUnsupportedPartitionedParent, v.Reason)
		assert.Equal(t, verdict.CauseParentConcurrentIndexDrop, v.Cause)
		assert.Contains(t, v.Detail, "every partition of it, and every partition's index were locked")
		assert.False(t, cliIndexExists(t, pool, schema, "ev_name_idx"))
		assert.False(t, cliIndexExists(t, pool, schema, "ev_1_name_idx"))
	})
}
