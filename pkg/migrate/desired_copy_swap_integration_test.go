package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// copySwapRunOptions are runOptions with the copy-and-swap route wired the
// way the CLI wires it: the route's own connection, free disk the shadow
// will fit in, and the block-writes acknowledgement for table (SCHEMA.TABLE) so
// the test server needs no logical decoding.
func copySwapRunOptions(serverURL, table string) migrate.Options {
	opts := runOptions()
	opts.CopySwap = migrate.CopySwapOptions{
		Connection:    dbconn.Config{URL: serverURL},
		FreeDiskBytes: testutil.UnlimitedDisk,
		BlockWrites:   table,
	}
	return opts
}

// Desired-state execution runs a plan through the copy-and-swap backend as
// a unit: a plan whose preflight would refuse is refused whole before the
// native statements ahead of the retype commit, and a plan that retypes two
// columns of one table runs both — the second run replaces the done row the
// first left behind — and converges.
func TestRunDesiredCopyAndSwap(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	defer pool.Close()

	columnType := func(t *testing.T, schema, column string) string {
		t.Helper()
		var typ string
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT data_type FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't' AND column_name = $2`, schema, column).Scan(&typ))
		return typ
	}

	t.Run("a plan the route's preflight refuses runs nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.t (
				id       int PRIMARY KEY,
				owner_id int NOT NULL
			)`, schema))
		require.NoError(t, err)

		// The plan is an ADD COLUMN the native backend would commit in an
		// instant, then a retype the router sends through copy-and-swap.
		// With no free disk declared the route's preflight refuses the
		// retype; admission reads that before the ADD COLUMN runs.
		opts := copySwapRunOptions(serverURL, schema+".t")
		opts.CopySwap.FreeDiskBytes = 0
		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema: schema,
			Desired: parseDesired(t, `CREATE TABLE t (
				id       int PRIMARY KEY,
				owner_id bigint NOT NULL,
				note     text
			)`),
		}, opts)
		require.NoError(t, err, "a preflight refusal is a result, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
		assert.Equal(t, verdict.ClassEnvironmental, res.Class)
		assert.Empty(t, res.Verdicts, "no statement was attempted")
		assert.Contains(t, res.Detail, string(preflight.CopySwapCauseDiskHeadroom))
		assert.Contains(t, res.Detail, "nothing was executed")
		require.Len(t, res.Plan.Statements, 2, "the plan carried the native statement and the retype")
		assert.Equal(t, router.BackendCopyAndSwap, res.Plan.Statements[1].Backend)

		var columns int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't'`, schema).Scan(&columns))
		assert.Equal(t, 2, columns, "the native ADD COLUMN ahead of the refused retype did not commit")
		assert.Equal(t, "integer", columnType(t, schema, "owner_id"))
	})

	t.Run("a block-writes acknowledgement naming another table runs nothing", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.t (
				id       int PRIMARY KEY,
				owner_id int NOT NULL
			)`, schema))
		require.NoError(t, err)

		// The same plan as above, a native ADD COLUMN ahead of a retype the
		// router sends through copy-and-swap, with every gate open except
		// the acknowledgement, which names another table. Each planned
		// statement checks it before anything runs, so the first one, the
		// native ADD COLUMN, already stops the plan: neither statement runs
		// and no table's writes are blocked.
		opts := copySwapRunOptions(serverURL, schema+".other")
		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema: schema,
			Desired: parseDesired(t, `CREATE TABLE t (
				id       int PRIMARY KEY,
				owner_id bigint NOT NULL,
				note     text
			)`),
		}, opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "block-writes acknowledgement")
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		assert.Empty(t, res.Verdicts, "no statement reached a verdict")
		require.Len(t, res.Plan.Statements, 2, "the plan carried the native statement and the retype")
		assert.Equal(t, router.BackendCopyAndSwap, res.Plan.Statements[1].Backend)

		var columns int
		require.NoError(t, pool.QueryRow(t.Context(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = 't'`, schema).Scan(&columns))
		assert.Equal(t, 2, columns, "the native ADD COLUMN did not commit")
		assert.Equal(t, "integer", columnType(t, schema, "owner_id"), "the retype did not run")
		_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.t (id, owner_id) VALUES (1, 1)`, schema))
		assert.NoError(t, err, "the table still takes writes")
	})

	t.Run("two retypes of one table converge and re-run as a no-op", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.t (
				id       int PRIMARY KEY,
				owner_id int NOT NULL,
				body     text
			)`, schema))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), fmt.Sprintf(
			"INSERT INTO %s.t SELECT g, g, repeat('x', 8) FROM generate_series(1, 50) g", schema))
		require.NoError(t, err)

		// Each retyped column is its own planned statement and its own
		// copy-and-swap run on the same table. The first leaves its done
		// row; the second replaces it inside its own lock session.
		req := migrate.DesiredRequest{
			Schema: schema,
			Desired: parseDesired(t, `CREATE TABLE t (
				id       int PRIMARY KEY,
				owner_id bigint NOT NULL,
				body     varchar(10)
			)`),
		}
		res, err := migrate.RunDesired(t.Context(), pool, req, copySwapRunOptions(serverURL, schema+".t"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		require.Len(t, res.Plan.Statements, 2, "one retype per column")
		require.Len(t, res.Verdicts, 2, "every planned statement carries a verdict")
		for _, v := range res.Verdicts {
			assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		}
		assert.Equal(t, "bigint", columnType(t, schema, "owner_id"))
		assert.Equal(t, "character varying", columnType(t, schema, "body"))
		var rows int
		require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.t", schema)).Scan(&rows))
		assert.Equal(t, 50, rows, "every row is carried across both swaps")

		// The convergence oracle: a second run derives an empty plan and
		// runs nothing.
		res, err = migrate.RunDesired(t.Context(), pool, req, copySwapRunOptions(serverURL, schema+".t"))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
	})
}
