package migrate_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// runOptions is a runnable policy for direct [migrate.Run] calls: the
// sanctioned embedding starting point, with the long-phase bounds tightened
// the way a caller tunes them — here so a hung test fails in a minute, not
// thirty.
func runOptions() migrate.Options {
	opts := migrate.DefaultOptions()
	opts.Budget.Concurrent.Overall = time.Minute
	opts.Budget.Validate.Overall = time.Minute
	return opts
}

func parseOne(t *testing.T, sql string) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	return st
}

// Run is the library front door: these tests drive its top-level dispatch —
// gate, force acknowledgement, and each router disposition — directly,
// without the CLI adapter, so a non-CLI embedding caller has the same
// coverage the CLI's integration tests give the flag surface.
func TestRunDispatch(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	t.Run("executes an instant change to one verdict", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		assert.Equal(t, schema+".t", v.Table)

		var typ string
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'age'`, schema).Scan(&typ))
		assert.Equal(t, "integer", typ)
	})

	t.Run("substitutes the safer native sequence", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"INSERT INTO %s.t SELECT g, 'x' FROM generate_series(1, 100) g", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		assert.Len(t, v.ExecutedSQL, 4, "the four-step SET NOT NULL sequence must be reported")

		var notNull bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT attnotnull FROM pg_attribute
			 WHERE attrelid = ($1 || '.t')::regclass AND attname = 'v'`, schema).Scan(&notNull))
		assert.True(t, notNull)
	})

	t.Run("re-checks the gate for callers that skipped it", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("DROP TABLE %s.t", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT to_regclass($1 || '.t') IS NOT NULL", schema).Scan(&exists))
		assert.True(t, exists, "a gated statement must never execute")
	})

	t.Run("refuses a copy-and-swap route as backend-unavailable", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"INSERT INTO %s.t SELECT g, 'x' FROM generate_series(1, 100) g", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN id TYPE bigint", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)

		var typ string
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'id'`, schema).Scan(&typ))
		assert.Equal(t, "integer", typ, "the refused change must not touch the schema")
	})

	t.Run("refuses an unconstructible rewrite as rewrite-required", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN nickname text UNIQUE", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonRewriteRequired, v.Reason)

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'nickname')`, schema).Scan(&exists))
		assert.False(t, exists, "the refused change must not touch the schema")
	})

	t.Run("runs the acknowledged submitted form as-is", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"INSERT INTO %s.t SELECT g, 'x' FROM generate_series(1, 100) g", schema))
		require.NoError(t, err)

		opts := runOptions()
		opts.Force = schema + ".t"
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema)), opts)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
		assert.True(t, v.Forced, "the verdict must record the override")
		assert.Empty(t, v.ExecutedSQL, "the submitted form ran as-is; no substitution to report")

		var notNull bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT attnotnull FROM pg_attribute
			 WHERE attrelid = ($1 || '.t')::regclass AND attname = 'v'`, schema).Scan(&notNull))
		assert.True(t, notNull)
	})

	t.Run("returns the failed verdict together with the operational error", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
		require.NoError(t, err)
		// The NULL row makes the substituted sequence's VALIDATE CONSTRAINT
		// step fail after the scaffold CHECK ... NOT VALID has committed.
		_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (1, NULL)", schema))
		require.NoError(t, err)

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema)), runOptions())
		require.Error(t, err, "an execution failure is an operational error")
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome,
			"the failed verdict is the error's machine-readable twin — both return together")
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Len(t, v.ExecutedSQL, 1, "exactly the scaffold step committed before the failure")

		var scaffolds int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_constraint con
			   JOIN pg_class c ON c.oid = con.conrelid
			   JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = $1 AND c.relname = 't' AND con.contype = 'c'`, schema).Scan(&scaffolds))
		assert.Equal(t, 1, scaffolds, "the committed prefix must describe real surviving state")
	})

	t.Run("rejects a mismatched force acknowledgement before a verdict", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)

		opts := runOptions()
		opts.Force = schema + ".wrong"
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema)), opts)
		require.Error(t, err)
		assert.Equal(t, verdict.Verdict{}, v, "an error before a verdict carries a zero verdict")

		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = 'age')`, schema).Scan(&exists))
		assert.False(t, exists, "nothing may execute on an acknowledgement mismatch")
	})
}
