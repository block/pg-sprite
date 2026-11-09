package schemachange_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// poolOf opens a pool of exactly size connections on the fixture's
// database, the pool the run under test is handed.
func (f reconcileFixture) poolOf(t *testing.T, size int32) *pgxpool.Pool {
	t.Helper()
	cfg := f.cfg
	cfg.MaxConns = size
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.Equal(t, size, pool.Config().MaxConns)
	return pool
}

// A pool of exactly MinPoolConns for the run's copy options — two copy
// workers, one connection for the catch-up's flushes, one for the
// checkpoint saves — is admitted, and the live run reaches done on it.
func TestRunnerRunsOnAPoolAtTheFloor(t *testing.T) {
	f := newDecodingFixture(t, 100)
	opts := liveRunOptions(schemachange.RunOptions{})
	require.Equal(t, int32(4), schemachange.MinPoolConns(opts.Copier), "two workers and the two the run reserves")
	pool := f.poolOf(t, schemachange.MinPoolConns(opts.Copier))
	runner, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)

	result, err := runner.Run(t.Context(), pool, f.target, f.liveStatement(t))

	require.NoError(t, err)
	assert.Equal(t, checkpoint.PhaseDone, result.Checkpoint.Phase)
}

// A pool one connection short of MinPoolConns is refused with the same
// executor.ErrPoolTooSmall the front doors give, before the run takes the
// table lock or writes anything: no checkpoint row, no shadow, no slot,
// and no publication. With a connection fewer than the floor, the copy
// workers could take every connection the catch-up's flushes and the
// checkpoint saves need, so the run is not started on it at all.
func TestRunnerRefusesAPoolBelowTheFloorBeforeCreatingAnything(t *testing.T) {
	f := newDecodingFixture(t, 100)
	opts := liveRunOptions(schemachange.RunOptions{})
	pool := f.poolOf(t, schemachange.MinPoolConns(opts.Copier)-1)
	runner, err := schemachange.NewRunner(f.cfg, f.store, opts)
	require.NoError(t, err)

	_, err = runner.Run(t.Context(), pool, f.target, f.liveStatement(t))

	require.ErrorIs(t, err, executor.ErrPoolTooSmall)
	_, err = f.store.Inspect(t.Context(), f.table.Schema, f.table.Table)
	assert.ErrorIs(t, err, checkpoint.ErrNotFound, "no checkpoint row was written")
	var shadowed, published bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT pg_catalog.to_regclass($1) IS NOT NULL`,
		pgx.Identifier{f.table.Schema, schemachange.ShadowName(f.table.Schema, f.table.Table)}.Sanitize()).Scan(&shadowed))
	assert.False(t, shadowed, "no shadow was built")
	f.assertNoSlot(t)
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = $1)`, f.target.DecodingName()).Scan(&published))
	assert.False(t, published, "no publication was made")
}
