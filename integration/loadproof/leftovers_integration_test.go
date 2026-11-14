package loadproof_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// heldSlot is a logical replication slot a walsender is streaming from, as
// a run's slot is while the binary decodes it.
type heldSlot struct {
	pool    *pgxpool.Pool
	name    string
	release func()
}

// slotHeldDeadline bounds the wait for the walsender to take the slot once
// streaming starts, which a healthy server does at once.
const slotHeldDeadline = 10 * time.Second

// newHeldSlot starts a server that decodes WAL, creates a logical slot on a
// database of its own, and starts streaming from it on a replication
// connection: the walsender serving that connection holds the slot until
// the connection closes. release closes it, once however often it is
// called, and runs again at cleanup.
func newHeldSlot(t *testing.T) heldSlot {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical")
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	const name = "loadproof_leftover"
	_, err = pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'test_decoding')`, name)
	require.NoError(t, err)
	holder, err := dbconn.ConnectReplication(t.Context(), cfg)
	require.NoError(t, err)
	var once sync.Once
	release := func() {
		once.Do(func() {
			assert.NoError(t, holder.Close(context.WithoutCancel(t.Context())))
		})
	}
	t.Cleanup(release)
	require.NoError(t, pglogrepl.StartReplication(t.Context(), holder, name, 0,
		pglogrepl.StartReplicationOptions{Mode: pglogrepl.LogicalReplication}))
	require.Eventually(t, func() bool {
		active, found := slotActive(t, pool, name)
		return found && active
	}, slotHeldDeadline, slotReleasePollInterval, "the streaming walsender must hold the slot")
	return heldSlot{pool: pool, name: name, release: release}
}

// slotActive reads whether the slot exists and whether a backend holds it.
func slotActive(t *testing.T, pool *pgxpool.Pool, name string) (active, found bool) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `SELECT active FROM pg_replication_slots WHERE slot_name = $1`, name).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	require.NoError(t, err)
	return active, true
}

// A slot whose holder lets go while the cleanup is already looking at it is
// dropped, not refused: the binary's connection is closed before the
// cleanup runs, but the server notices that and releases the slot a moment
// later, so the cleanup waits for the release instead of leaving the slot
// pinning WAL.
func TestDropSlotIfLeftWaitsForTheHolderToLetGo(t *testing.T) {
	slot := newHeldSlot(t)

	// The holder lets go a short while after the drop starts, from its own
	// goroutine, which the test waits for before it returns.
	const heldFor = 500 * time.Millisecond
	var wg sync.WaitGroup
	wg.Go(func() {
		timer := time.NewTimer(heldFor)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
		}
		slot.release()
	})
	dropped, err := dropSlotIfLeft(t.Context(), slot.pool, slot.name)
	wg.Wait()

	require.NoError(t, err)
	assert.True(t, dropped, "the slot was there and is reported dropped")
	_, found := slotActive(t, slot.pool, slot.name)
	assert.False(t, found, "the slot no longer exists")
}

// A slot still held when the wait runs out belongs to a process that is
// still running, and the cleanup says so rather than drop it from under
// that process; once the holder lets go the same cleanup drops it.
func TestDropSlotIfLeftRefusesASlotStillHeldAtTheDeadline(t *testing.T) {
	slot := newHeldSlot(t)

	const deadline = 300 * time.Millisecond
	dropped, err := dropSlotWhenReleased(t.Context(), slot.pool, slot.name, deadline)
	require.ErrorIs(t, err, errSlotStillHeld)
	assert.False(t, dropped)
	active, found := slotActive(t, slot.pool, slot.name)
	assert.True(t, found, "the held slot is left in place")
	assert.True(t, active, "the holder still streams from it")

	slot.release()
	dropped, err = dropSlotIfLeft(t.Context(), slot.pool, slot.name)
	require.NoError(t, err)
	assert.True(t, dropped, "the slot is dropped once nothing holds it")
}
