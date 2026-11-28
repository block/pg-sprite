package migrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// The tests here send a row security change on orders through
// RunRowSecurity, the door that takes the table's lock before it reads the
// checkpoint row and runs the change. The desired file matches the live
// orders table and enables row security with one policy:
//
//	CREATE TABLE orders (
//		id   bigserial PRIMARY KEY,
//		qty  integer NOT NULL,
//		note text,
//		sku  text
//	);
//	CREATE INDEX orders_qty_idx ON orders (qty);
//	ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
//	CREATE POLICY readers ON orders FOR SELECT USING (true);

// lockHeldBound bounds a call that must not wait for a held table lock: a
// call that waited would end in the deadline rather than the held lock's
// error.
const lockHeldBound = 30 * time.Second

// ordersReadersRequest is the request for the desired file above.
func (f copySwapFixture) ordersReadersRequest(t *testing.T) RowSecurityRequest {
	t.Helper()
	desired, err := statement.ParseDesiredWithRowSecurity(`
		CREATE TABLE orders (
			id   bigserial PRIMARY KEY,
			qty  integer NOT NULL,
			note text,
			sku  text
		);
		CREATE INDEX orders_qty_idx ON orders (qty);
		ALTER TABLE orders ENABLE ROW LEVEL SECURITY;
		CREATE POLICY readers ON orders FOR SELECT USING (true);`)
	require.NoError(t, err)
	return RowSecurityRequest{Schema: f.schema, Desired: desired}
}

// rowSecurityOptions is the desired-state policy with the connection the
// table's lock is opened from.
func (f copySwapFixture) rowSecurityOptions() Options {
	opts := DefaultOptions()
	opts.Connection = f.cfg
	return opts
}

// ordersRowSecurity reads whether orders has row security enabled and the
// names of its policies.
func (f copySwapFixture) ordersRowSecurity(t *testing.T, pool *pgxpool.Pool) (bool, []string) {
	t.Helper()
	var enabled bool
	var policies []string
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT c.relrowsecurity,
		       COALESCE(ARRAY(
		           SELECT p.polname::text
		           FROM pg_catalog.pg_policy p
		           WHERE p.polrelid = c.oid
		           ORDER BY p.polname), '{}')
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relname = 'orders'`, f.schema).Scan(&enabled, &policies))
	return enabled, policies
}

// requireTableLockFree takes and lets go of orders' table lock, which fails
// while any session still holds it.
func (f copySwapFixture) requireTableLockFree(t *testing.T) {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err, "the table's lock was let go")
	require.NoError(t, lock.Release(t.Context()))
}

// Over the retype of qty stopped in verifying, the row security change
// would add a policy to the source the stopped run recorded, which its
// cutover gate then refuses, so the change is refused with
// copy-and-swap-run-stopped, read under the table's lock, and nothing runs.
func TestRunRowSecurityRefusesOverAStoppedCopySwapRun(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	f.stopRetypeAtVerifying(t)

	v, err := RunRowSecurity(t.Context(), f.pool, f.ordersReadersRequest(t), f.rowSecurityOptions())
	require.NoError(t, err, "a refusal is a verdict, not an error")
	f.requireStoppedRunRefusal(t, v)
	assert.Empty(t, v.ExecutedSQL)

	enabled, policies := f.ordersRowSecurity(t, f.pool)
	assert.False(t, enabled, "nothing was executed")
	assert.Empty(t, policies, "nothing was executed")
	assert.Equal(t, checkpoint.PhaseVerifying, f.inspectRow(t).Phase, "the stopped run is left as it was")
	f.requireTableLockFree(t)
}

// Another session holds orders' table lock, as a copy-and-swap run does
// from before it reads the checkpoint row until it ends. No run is stopped
// on the table, so the unlocked forecast admits the change, and a door that
// read the row and then ran the executor would add the policy under the
// run. RunRowSecurity takes the lock first and stops at once, without
// waiting for it: a failed verdict, the held lock's error naming the
// holder, and nothing executed.
func TestRunRowSecurityStopsAtOnceWhileTheTableLockIsHeld(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	holder, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, holder.Release(context.WithoutCancel(t.Context()))) })

	_, stopped, err := AdmitRowSecurityStoppedRun(t.Context(), f.pool, f.schema, "orders")
	require.NoError(t, err)
	require.False(t, stopped, "the row, read without the lock, shows no stopped run")

	ctx, cancel := context.WithTimeout(t.Context(), lockHeldBound)
	defer cancel()
	for name, run := range map[string]func() (verdict.Verdict, error){
		"RunRowSecurity": func() (verdict.Verdict, error) {
			return RunRowSecurity(ctx, f.pool, f.ordersReadersRequest(t), f.rowSecurityOptions())
		},
		"RunReviewedRowSecurity": func() (verdict.Verdict, error) {
			return RunReviewedRowSecurity(ctx, f.pool, f.ordersReadersRequest(t), nil, f.rowSecurityOptions())
		},
	} {
		v, err := run()
		held, ok := errors.AsType[*dbconn.TableLockHeldError](err)
		require.True(t, ok, "%s: want the held lock's error, got %v", name, err)
		assert.Equal(t, holder.BackendPID(), held.Holder.PID, name)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome, "%s: %s", name, v.Detail)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code, name)
		assert.Equal(t, f.schema+".orders", v.Table, name)
		assert.Empty(t, v.ExecutedSQL, name)
	}

	enabled, policies := f.ordersRowSecurity(t, f.pool)
	assert.False(t, enabled, "nothing was executed")
	assert.Empty(t, policies, "nothing was executed")
}

// With no run on orders, the change enables row security and adds the
// policy in one transaction, on a pool of one connection: the table's lock
// is held on a session of its own outside the pool, so the change's
// transaction has the pool's one connection to itself. The lock is let go
// once the change commits, and a reviewed run of the converged file, whose
// empty review authorizes only a no-op, runs nothing.
func TestRunRowSecurityAddsThePolicyOnAPoolOfOneConnection(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	pool := f.poolOfSize(t, 1)

	v, err := RunRowSecurity(t.Context(), pool, f.ordersReadersRequest(t), f.rowSecurityOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Equal(t, f.schema+".orders", v.Table)
	// The executor renders the whole row security from the desired
	// catalog: the setting, its FORCE counterpart, and each policy in full.
	target := pgx.Identifier{f.schema, "orders"}.Sanitize()
	assert.Equal(t, []string{
		"ALTER TABLE " + target + " ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + target + " NO FORCE ROW LEVEL SECURITY",
		`CREATE POLICY "readers" ON ` + target + `
    AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (true)`,
	}, v.ExecutedSQL)
	enabled, policies := f.ordersRowSecurity(t, pool)
	assert.True(t, enabled)
	assert.Equal(t, []string{"readers"}, policies)
	f.requireTableLockFree(t)

	v, err = RunReviewedRowSecurity(t.Context(), pool, f.ordersReadersRequest(t), []string{}, f.rowSecurityOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "%s: %s", v.Reason, v.Detail)
	assert.Empty(t, v.ExecutedSQL, "the file already matches")
	f.requireTableLockFree(t)
}
