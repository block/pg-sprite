package migrate

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// poolOfSize opens a pool on the fixture's database whose URL carries
// pool_max_conns=size, the operator's explicit choice, with the floor the
// CLI sets: the floor raises only a default size, so the pool holds size.
func (f copySwapFixture) poolOfSize(t *testing.T, size int) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(f.cfg.URL)
	require.NoError(t, err)
	q := u.Query()
	q.Set("pool_max_conns", fmt.Sprint(size))
	u.RawQuery = q.Encode()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:           u.String(),
		MaxConnsFloor: CopySwapMinConns(copier.Options{}),
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.Equal(t, int32(size), pool.Config().MaxConns)
	return pool
}

// relations lists the relations in the fixture's schema by name.
func (f copySwapFixture) relations(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT c.relname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		ORDER BY c.relname`, f.schema)
	require.NoError(t, err)
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

// assertNoCheckpointRow asserts that the route saved no checkpoint row for
// table in the fixture's schema. It reads the row, not the engine schema:
// the suite can run against one shared database, where other tests have
// already created the checkpoint table. A run that blocks writes takes no
// replication slot, so the row is the only engine state it could leave.
func (f copySwapFixture) assertNoCheckpointRow(t *testing.T, table string) {
	t.Helper()
	store, err := checkpoint.NewStore(f.pool, checkpoint.Options{})
	require.NoError(t, err)
	_, err = store.Inspect(t.Context(), f.schema, table)
	assert.True(t, errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing),
		"no checkpoint row was saved for %s.%s: %v", f.schema, table, err)
}

// The copier's four default workers each hold a pool connection while they
// copy a chunk, and the route keeps two more: one for the catch-up's
// flushes, one for checkpoint saves and progress reads. A pool of five
// would leave the catch-up queued behind the copy, so Run refuses it
// before anything is read or written: a failed verdict with the
// pool-too-small code, an error matching executor.ErrPoolTooSmall, and no
// shadow or checkpoint row left behind.
func TestCopyAndSwapRefusesAPoolSmallerThanItsWorkersAndTwo(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	small := f.poolOfSize(t, 5)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)

	v, err := Run(t.Context(), small, st, f.options())
	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodePoolTooSmall), v.Code)
	assert.Equal(t, f.schema+".orders", v.Table)

	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "the source is untouched")
	assert.Equal(t, []string{"orders", "orders_id_seq", "orders_pkey", "orders_qty_idx"}, f.relations(t),
		"no shadow table was built")
	f.assertNoCheckpointRow(t, "orders")
}

// Six connections hold the four workers and the two reserved sessions, so
// the same statement on a pool of six is admitted and runs to the swap.
func TestCopyAndSwapAdmitsAPoolOfItsWorkersAndTwo(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	pool := f.poolOfSize(t, 6)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)

	v, err := Run(t.Context(), pool, st, f.options())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
	assert.Equal(t, "bigint", f.columnType(t, "orders", "qty"))
	assert.Equal(t, int64(2500), f.rowCount(t, "orders"))
}

// Desired-state admission reads the pool size before the plan's first
// statement. The plan adds a column natively and then retypes owner_id
// through copy-and-swap; on a pool of five it stops as a failed result with
// no verdict, and the native ADD COLUMN ahead of the retype never runs.
func TestRunDesiredRefusesAPoolSmallerThanCopyAndSwapNeeds(t *testing.T) {
	f := newCopySwapFixture(t)
	f.exec(t, `
		CREATE TABLE {schema}.t (
			id       int PRIMARY KEY,
			owner_id int NOT NULL
		)`)
	small := f.poolOfSize(t, 5)
	desired, err := statement.ParseDesired(`
		CREATE TABLE t (
			id       int PRIMARY KEY,
			owner_id bigint NOT NULL,
			note     text
		)`)
	require.NoError(t, err)

	opts := f.options()
	opts.CopySwap.BlockWrites = f.schema + ".t"
	res, err := RunDesired(t.Context(), small, DesiredRequest{Schema: f.schema, Desired: desired}, opts)
	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
	assert.Empty(t, res.Verdicts, "no statement was attempted")

	assert.False(t, f.hasColumn(t, "t", "note"), "the native statement ahead of the retype did not run")
	assert.Equal(t, "integer", f.columnType(t, "t", "owner_id"), "the source is untouched")
	assert.Equal(t, []string{"t", "t_pkey"}, f.relations(t), "no shadow table was built")
	f.assertNoCheckpointRow(t, "t")
}
