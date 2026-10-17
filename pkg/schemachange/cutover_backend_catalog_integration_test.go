package schemachange_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// shadowedSwapPool is a pool on the fixture's server for the swap: every
// session lists the fixture's schema ahead of pg_catalog on search_path, so
// an impostor relation or function created there answers an unqualified
// catalog name, and its connections stop hearing from the server once lose
// is set. It is a raw pgx pool on purpose — pools from pkg/dbconn remove a
// shadowed pg_catalog entry on connect, and the swap's first statements run
// on whatever pool the caller built, before the swap sets a path of its own.
func (f shadowFixture) shadowedSwapPool(t *testing.T, lose *atomic.Bool) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	pc.ConnConfig.RuntimeParams["search_path"] = f.schema + ", pg_catalog"
	dialer := &net.Dialer{}
	pc.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &unreachableConn{Conn: c, lose: lose, eofOnCommit: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// The two catalog reads the swap makes on the caller's own pool — the
// transaction's record of which backend it runs on, and the poll that
// follows a lost attempt's backend to its exit — must resolve to the real
// pg_catalog even when the pool's sessions list another schema ahead of it
// (CO-9). Each subtest plants an impostor in the schema that is first on the
// pool's search_path and requires the swap to answer as it does without one.
func TestCutoverBackendReadsResolveToTheRealCatalog(t *testing.T) {
	// The swap records its backend with pg_backend_pid(). An impostor
	// returning a pid no backend has would make the swap's own row
	// unreadable, so a swap that would have committed is refused instead.
	t.Run("an impostor pg_backend_pid does not misidentify the swap's backend", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		f.exec(t, `
			CREATE FUNCTION %s.pg_backend_pid() RETURNS integer
			LANGUAGE sql AS 'SELECT 0'`)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
		ready, err := f.gate(t, s)
		require.NoError(t, err)
		var lose atomic.Bool
		pool := f.shadowedSwapPool(t, &lose)

		swapped, err := schemachange.Cutover(t.Context(), pool, s.lock, ready, nil, schemachange.CutoverOptions{})

		require.NoError(t, err, "the swap records the backend it really runs on")
		assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the shadow is the live table")
		assert.Equal(t, 1, swapped.Attempts())
	})

	// The poll after a lost attempt asks pg_stat_activity whether the
	// attempt's backend is still there. An impostor view that shows a
	// session only its own row answers "gone" at once while the server is
	// still inside the attempt's COMMIT, so the swap would read the outcome
	// before the attempt decided it (LK-4). The real wait follows the
	// backend to its exit and reports the commit the client never heard of.
	t.Run("an impostor pg_stat_activity does not hide the lost attempt's backend", func(t *testing.T) {
		f := newShadowFixture(t)
		f.createOrders(t)
		f.exec(t, `
			CREATE VIEW %s.pg_stat_activity AS
			SELECT * FROM pg_catalog.pg_stat_activity
			WHERE pid = pg_catalog.pg_backend_pid()`)
		write := f.slowCommit(t, 2*time.Second)
		s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
		ready, err := f.gate(t, s)
		require.NoError(t, err)
		var lose atomic.Bool
		pool := f.shadowedSwapPool(t, &lose)

		drain := func(ctx context.Context, tx pgx.Tx) error {
			lose.Store(true)
			return write(ctx, tx)
		}
		swapped, err := schemachange.Cutover(t.Context(), pool, s.lock, ready, drain, schemachange.CutoverOptions{})
		f.waitForCommitToFinish(t)

		require.NoError(t, err, "the server committed, so the swap is reported as a success")
		assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the shadow is the live table")
		assert.Equal(t, 1, swapped.Attempts())
	})
}
