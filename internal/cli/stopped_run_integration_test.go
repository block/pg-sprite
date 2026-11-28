package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here stop a copy-and-swap run of
//
//	ALTER TABLE orders ALTER COLUMN qty TYPE bigint
//
// in verifying, with its shadow, its write fence, and its row standing, and
// then send another change on orders through the front doors a run takes
// apart from the imperative pipeline: the dry runs, which must end in the
// refusal the run would, and the desired file's row security change, which
// runs in a transaction of its own.

// errStopped is what the checkpoint hook stops the copy-and-swap run with.
var errStopped = errors.New("stopped in verifying")

// stoppedOrders creates orders, with row security enabled and one policy so
// a row security file can describe it, and stops the retype of qty in
// verifying. It returns the server URL, the schema, and a pool on it.
func stoppedOrders(t *testing.T) (string, string, *pgxpool.Pool) {
	t.Helper()
	url := testutil.StartPostgres(t)
	cfg := dbconn.Config{URL: url, MaxConnsFloor: migrate.CopySwapMinConns(copier.Options{})}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text
		);
		ALTER TABLE %[1]s.orders ENABLE ROW LEVEL SECURITY;
		CREATE POLICY readers ON %[1]s.orders FOR SELECT USING (true);
		INSERT INTO %[1]s.orders (qty, note)
		SELECT g, 'note ' || g
		FROM generate_series(1, 500) g;`, schema))
	require.NoError(t, err)

	retype, err := statement.ParseOne(fmt.Sprintf(`
		ALTER TABLE %s.orders
			ALTER COLUMN qty TYPE bigint`, schema))
	require.NoError(t, err)
	opts := migrate.DefaultOptions()
	opts.CopySwap = migrate.CopySwapOptions{
		Enabled:       true,
		Connection:    cfg,
		FreeDiskBytes: testutil.UnlimitedDisk,
		BlockWrites:   schema + ".orders",
		OnCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
			if cp.Phase == checkpoint.PhaseVerifying {
				return errStopped
			}
			return nil
		},
	}
	_, err = migrate.Run(t.Context(), pool, retype, opts)
	require.ErrorIs(t, err, errStopped)
	return url, schema, pool
}

// stoppedRunResult is the part of a verdict or a desired-state result the
// tests read.
type stoppedRunResult struct {
	Outcome verdict.Outcome `json:"outcome"`
	Reason  verdict.Reason  `json:"reason"`
	Cause   verdict.Cause   `json:"cause"`
	Detail  string          `json:"detail"`
}

// requireStoppedRunRefusal checks that out is the refusal of a change over
// the stopped run, with the refusal exit code.
func requireStoppedRunRefusal(t *testing.T, err error, out string) {
	t.Helper()
	require.ErrorIs(t, err, verdict.ErrRefused, out)
	var res stoppedRunResult
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome, res.Detail)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapRunStopped, res.Cause)
}

// The apply of
//
//	ALTER TABLE orders DROP COLUMN note
//
// is refused over the stopped retype, so its dry run is too, rather than
// print a plan the apply will not run. The dry run of the stopped retype
// itself is not refused over the stopped run: with --copy-and-swap its apply
// resumes the run, and without it the apply refuses the closed route, and
// so does the dry run.
func TestMigrateDryRunRefusesAnotherStatementOverAStoppedCopySwapRun(t *testing.T) {
	url, schema, _ := stoppedOrders(t)

	drop := newMigrateCmd(url, fmt.Sprintf(`
		ALTER TABLE %s.orders
			DROP COLUMN note`, schema))
	drop.DryRun = true
	drop.JSON = true
	drop.AllowDestructive = schema + ".orders"
	var out strings.Builder
	requireStoppedRunRefusal(t, drop.run(t.Context(), &out), out.String())

	retypeSQL := fmt.Sprintf(`
		ALTER TABLE %s.orders
			ALTER COLUMN qty TYPE bigint`, schema)
	closed := newMigrateCmd(url, retypeSQL)
	closed.DryRun = true
	closed.JSON = true
	var refusal strings.Builder
	require.ErrorIs(t, closed.run(t.Context(), &refusal), verdict.ErrRefused, refusal.String())
	var res stoppedRunResult
	require.NoError(t, json.Unmarshal([]byte(refusal.String()), &res), refusal.String())
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, res.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapNotEnabled, res.Cause, "the resume needs the route opened, as the apply does")

	retype := newMigrateCmd(url, retypeSQL)
	retype.DryRun = true
	retype.CopyAndSwap = true
	retype.JSON = true
	var plan strings.Builder
	require.NoError(t, retype.run(t.Context(), &plan), plan.String())
}

// A desired file that matches the source — which a run stopped before its
// swap has not changed — plans nothing, and the run refuses that empty plan
// over the stopped retype rather than report the table converged. Its dry
// run gives the same refusal.
func TestMigrateDesiredDryRunRefusesAnEmptyPlanOverAStoppedCopySwapRun(t *testing.T) {
	url, schema, _ := stoppedOrders(t)
	cmd := desiredCommand(t, url, schema, `
		CREATE TABLE orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text
		);`, "--dry-run")
	var out strings.Builder
	requireStoppedRunRefusal(t, cmd.run(t.Context(), &out), out.String())
}

// A desired file that adds a column plans
//
//	ALTER TABLE orders ADD COLUMN sku text
//
// whose run refuses it over the stopped retype when it reaches the
// statement. Its dry run gives the same refusal instead of the plan.
func TestMigrateDesiredDryRunRefusesAPlanOverAStoppedCopySwapRun(t *testing.T) {
	url, schema, _ := stoppedOrders(t)
	cmd := desiredCommand(t, url, schema, `
		CREATE TABLE orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text,
			sku  text
		);`, "--dry-run")
	var out strings.Builder
	requireStoppedRunRefusal(t, cmd.run(t.Context(), &out), out.String())
}

// rowSecurityChange is a desired file for orders whose table definition
// matches the source and whose row security adds a policy, a change the
// row security executor runs on the table in one transaction.
const rowSecurityChange = `
	CREATE TABLE orders (
		id   bigserial PRIMARY KEY,
		qty  integer NOT NULL,
		note text
	);
	ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
	CREATE POLICY readers ON orders FOR SELECT USING (true);
	CREATE POLICY writers ON orders FOR INSERT WITH CHECK (true);`

// The row security change would add a policy to the source under the
// stopped run, whose cutover gate compares the source with the shape the
// run recorded, so it is refused before it locks the table, and the policy
// is not added.
func TestMigrateDesiredRowSecurityChangeIsRefusedOverAStoppedCopySwapRun(t *testing.T) {
	url, schema, pool := stoppedOrders(t)
	cmd := desiredCommand(t, url, schema, rowSecurityChange)
	var out strings.Builder
	requireStoppedRunRefusal(t, cmd.run(t.Context(), &out), out.String())

	var policies int
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT count(*)
		FROM pg_catalog.pg_policies
		WHERE schemaname = $1
		  AND tablename = 'orders'`, schema).Scan(&policies))
	assert.Equal(t, 1, policies, "nothing was executed")
}

// The dry run of the same row security change gives the refusal the run
// would, in place of the review of the policy it adds.
func TestMigrateDesiredRowSecurityDryRunRefusesOverAStoppedCopySwapRun(t *testing.T) {
	url, schema, _ := stoppedOrders(t)
	cmd := desiredCommand(t, url, schema, rowSecurityChange, "--dry-run")
	var out strings.Builder
	requireStoppedRunRefusal(t, cmd.run(t.Context(), &out), out.String())
}
