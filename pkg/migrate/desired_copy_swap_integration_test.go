package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
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
		Enabled:       true,
		Connection:    dbconn.Config{URL: serverURL},
		FreeDiskBytes: testutil.UnlimitedDisk,
		BlockWrites:   table,
	}
	return opts
}

// desiredCopySwapOptions are copySwapRunOptions without the block-writes
// acknowledgement, which desired-state execution rejects: the route takes
// the live path, so a plan that reaches a copy needs logical decoding.
func desiredCopySwapOptions(serverURL string) migrate.Options {
	return copySwapRunOptions(serverURL, "")
}

// A data fault on the declarative front door is abandoned the same way as
// on the imperative one, and the failed statement's verdict says so: the
// plan retypes body to varchar(2) while every row holds eight characters,
// so the copy fails with 22001 and the Runner discards the run. The
// aggregate result carries neither the code nor the field; an importer
// reads them on the failed entry of Verdicts.
func TestRunDesiredReportsAnAbandonedRunOnItsVerdict(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL, MaxConnsFloor: migrate.CopySwapMinConns(copier.Options{})})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id   int PRIMARY KEY,
			body text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.t (id, body)
		SELECT g, repeat('x', 8) FROM generate_series(1, 50) AS g`, schema))
	require.NoError(t, err)

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema,
		Desired: parseDesired(t, `CREATE TABLE t (
			id   int PRIMARY KEY,
			body varchar(2)
		)`),
	}, desiredCopySwapOptions(serverURL))
	require.Error(t, err)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	require.Len(t, res.Verdicts, 1)
	assert.Equal(t, "data-fault", res.Verdicts[0].Code)
	assert.True(t, res.Verdicts[0].Abandoned, "the Runner discarded the run, so the corrected plan runs fresh")
}

// Desired-state execution runs a plan through the copy-and-swap backend as
// a unit: a plan whose preflight would refuse is refused whole before the
// native statements ahead of the retype commit, and a plan that retypes two
// columns of one table runs both — the second run replaces the done row the
// first left behind — and converges.
func TestRunDesiredCopyAndSwap(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL, MaxConnsFloor: migrate.CopySwapMinConns(copier.Options{})})
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
		opts := desiredCopySwapOptions(serverURL)
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
		assert.Equal(t, verdict.Cause(preflight.CopySwapCauseDiskHeadroom), res.Cause, "the result carries the preflight's cause, as the statement verdict would")
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

	t.Run("a plan under the closed route is refused whole with the gate's cause", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.t (
				id       int PRIMARY KEY,
				owner_id int NOT NULL
			)`, schema))
		require.NoError(t, err)

		// The same plan — a native ADD COLUMN, then a retype routed to
		// copy-and-swap — with free disk declared and logical decoding
		// available, so only the route being closed stands in its way.
		// The gate refuses the whole plan at admission, before the
		// preflight reads anything, and the native statement planned ahead
		// of the retype does not commit.
		opts := desiredCopySwapOptions(serverURL)
		opts.CopySwap.Enabled = false
		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
			Schema: schema,
			Desired: parseDesired(t, `CREATE TABLE t (
				id       int PRIMARY KEY,
				owner_id bigint NOT NULL,
				note     text
			)`),
		}, opts)
		require.NoError(t, err, "a closed route is a refusal, not an error")
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
		assert.Equal(t, verdict.ClassCapabilityBoundary, res.Class)
		assert.Equal(t, verdict.CauseCopyAndSwapNotEnabled, res.Cause)
		assert.Empty(t, res.Verdicts, "no statement was attempted")
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
		res, err := migrate.RunDesired(t.Context(), pool, req, desiredCopySwapOptions(serverURL))
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
		res, err = migrate.RunDesired(t.Context(), pool, req, desiredCopySwapOptions(serverURL))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
		assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
	})
}

// Desired-state execution takes no block-writes acknowledgement, so a plan
// that reaches a copy always needs the live path. On a server without
// logical decoding (wal_level = replica, on a server of the test's own: a
// shared PG_DSN server may have logical decoding on) the plan below, a
// native ADD COLUMN ahead of a retype routed to copy-and-swap, is refused
// whole at admission: the route's preflight refuses the server as an
// environmental condition, and neither statement runs.
func TestRunDesiredRefusesACopyOnAServerWithoutLogicalDecoding(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=replica")
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL, MaxConnsFloor: migrate.CopySwapMinConns(copier.Options{})})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id       int PRIMARY KEY,
			owner_id int NOT NULL
		)`, schema))
	require.NoError(t, err)

	res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{
		Schema: schema,
		Desired: parseDesired(t, `CREATE TABLE t (
			id       int PRIMARY KEY,
			owner_id bigint NOT NULL,
			note     text
		)`),
	}, desiredCopySwapOptions(serverURL))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
	assert.Equal(t, verdict.ClassEnvironmental, res.Class, "logical decoding is a server setting, not a capability boundary")
	assert.Empty(t, res.Verdicts, "no statement was attempted")

	var columns int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 't'`, schema).Scan(&columns))
	assert.Equal(t, 2, columns, "the native ADD COLUMN did not commit")
}
