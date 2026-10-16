package checkpoint_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// slotLockTimeout bounds the second claim on a slot's lock: a lock still
// held is reported as the server's lock timeout well inside the test's
// own deadline.
const slotLockTimeout = 2 * time.Second

// newSlotLockStore is a store over a pool of exactly one connection, with
// a short lock_timeout, on a fresh database: the smallest pool that can
// show the lock holding a seat the callback needs.
func newSlotLockStore(t *testing.T) (*checkpoint.Store, dbconn.Config) {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, testutil.StartPostgres(t)), MaxConns: 1, LockTimeout: slotLockTimeout}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	return store, cfg
}

// The lock takes no seat in the store's pool: a callback on a pool of one
// connection still gets that connection while the lock is held, so a claim
// never waits on the pool for a seat its own lock occupies (LK-2 bounds
// the advisory wait, and nothing else is waited on).
func TestSlotLockLeavesThePoolToTheCallback(t *testing.T) {
	store, cfg := newSlotLockStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := store.WithSlotLock(ctx, cfg, "pgsprite_0badf00d", func(ctx context.Context) error {
		return store.Ensure(ctx)
	})

	require.NoError(t, err, "the callback's own work on the pool finishes under the lock")
}

// A callback that panics, with the panic recovered above, does not leave
// the slot's lock held: the next claim on the name gets it.
func TestSlotLockIsLetGoWhenTheCallbackPanics(t *testing.T) {
	store, cfg := newSlotLockStore(t)
	const name = "pgsprite_0badf00d"
	func() {
		defer func() { _ = recover() }()
		_ = store.WithSlotLock(t.Context(), cfg, name, func(context.Context) error { panic("boom") })
	}()

	err := store.WithSlotLock(t.Context(), cfg, name, func(context.Context) error { return nil })

	assert.NoError(t, err, "the lock is free again after the panic")
}

// While one holder has the lock, a second claim on the same name waits and
// is bounded by lock_timeout rather than by the first holder's goodwill;
// once the first lets go, a claim gets the lock at once.
func TestSlotLockIsExclusivePerName(t *testing.T) {
	store, cfg := newSlotLockStore(t)
	const name = "pgsprite_0badf00d"
	var second error
	err := store.WithSlotLock(t.Context(), cfg, name, func(ctx context.Context) error {
		second = store.WithSlotLock(ctx, cfg, name, func(context.Context) error { return nil })
		return nil
	})
	require.NoError(t, err)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, second, &pgErr, "the second claim on a held name is refused by the server")
	assert.Equal(t, "55P03", pgErr.Code, "the refusal is the server's lock timeout")

	err = store.WithSlotLock(t.Context(), cfg, name, func(context.Context) error { return nil })
	assert.NoError(t, err, "the lock is free once its holder has let go")
}

// A cfg that dials another database than the store's pool is refused
// before the lock is taken: the advisory key is per database, so a lock
// there would not exclude a claim or a reap judged through the store
// (ST-3). The callback never runs.
func TestSlotLockRefusesAConnectionOnAnotherDatabase(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, serverURL)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	elsewhere := dbconn.Config{URL: testutil.NewDatabase(t, serverURL)}

	ran := false
	err = store.WithSlotLock(t.Context(), elsewhere, "pgsprite_0badf00d", func(context.Context) error {
		ran = true
		return nil
	})

	require.ErrorIs(t, err, checkpoint.ErrInvariantViolation)
	assert.False(t, ran, "nothing runs under a lock in another database")
}
