package schemachange_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// repeatableReadPool is a pool on the fixture's server whose every session
// defaults to REPEATABLE READ, as a connection parameter: the setting
// reaches no other pool and nothing is left behind on the server.
func (f shadowFixture) repeatableReadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := f.cfg
	cfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		cc.RuntimeParams["default_transaction_isolation"] = "repeatable read"
		return nil
	}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var isolation string
	require.NoError(t, pool.QueryRow(t.Context(), `SHOW default_transaction_isolation`).Scan(&isolation))
	require.Equal(t, "repeatable read", isolation, "every session of the pool defaults to REPEATABLE READ")
	return pool
}

// An index created on the source while the swap waits for its ACCESS
// EXCLUSIVE lock is drift the swap refuses, even on a server whose default
// isolation is REPEATABLE READ. The swap's first statement runs before the
// lock wait; were the transaction to keep the snapshot that statement took,
// the checks after the lock would read the catalog from before the index
// committed, pass, and swap — leaving the new index behind on the retained
// table and the live table without it. The swap runs at READ COMMITTED, so
// the checks after the lock see what the lock froze (ST-5).
func TestCutoverUnderARepeatableReadDefaultRefusesAnIndexCreatedWhileItWaitedForTheLock(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	repeatableRead := f.repeatableReadPool(t)

	// The index build holds SHARE on orders until it commits, so the swap's
	// ACCESS EXCLUSIVE waits behind it after the swap's first statement ran.
	indexer, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the indexer commits below, so this
		// returns the guaranteed ErrTxClosed.
		_ = indexer.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = indexer.Exec(t.Context(), `CREATE INDEX orders_sku_idx ON `+pgx.Identifier{f.schema, "orders"}.Sanitize()+` (sku)`)
	require.NoError(t, err)

	swapDone := make(chan error, 1)
	go func() {
		// One attempt with a lock wait long enough to outlast the index
		// commit, so no retry opens a second transaction after it.
		_, err := schemachange.Cutover(t.Context(), repeatableRead, s.lock, ready, nil, schemachange.CutoverOptions{
			Options:      schemachange.Options{LockTimeout: 20 * time.Second},
			LockAttempts: 1,
		})
		swapDone <- err
	}()
	const swapWaits = 10 * time.Second
	require.Eventually(t, func() bool { return f.backendWaitingOnLock(t, "LOCK TABLE %") }, swapWaits, 20*time.Millisecond)
	require.NoError(t, indexer.Commit(t.Context()))
	err = <-swapDone

	assert.Equal(t, schemachange.CauseSchemaDrift, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrCutoverRolledBack, "a refusal inside the transaction leaves the source live")
	assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "the source is still live")
	assert.Contains(t, f.indexNames(t, "orders"), "orders_sku_idx", "the new index stays on the live table")
	assert.False(t, f.relationExists(t, schemachange.OldName(f.schema, "orders")), "nothing was retained under the _old name")
}
