package decode_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

const (
	// standbySlotDeadline bounds creating a logical slot on a standby,
	// which waits for the primary to log the running transactions the
	// slot's snapshot starts from.
	standbySlotDeadline = 30 * time.Second
	standbySlotPoll     = 100 * time.Millisecond
)

// A server in recovery has no write position, so a slot on it has no end
// to measure its retained or unconfirmed WAL to: both read as unknown,
// never as nothing, while the rest of the row is read as on a primary, and
// the lag watch refuses to call such a slot within the ceiling. The slot is
// a logical one held on the standby itself, since a standby lists only its
// own slots and only a logical slot has a confirmed position that would
// make the unconfirmed measure readable on a primary; a standby can hold
// one from PostgreSQL 16.
func TestInspectSlotOnAStandbyReadsBothMeasuresAsUnknown(t *testing.T) {
	major, err := strconv.Atoi(testutil.PGVersion())
	require.NoError(t, err, "PG_VERSION is a major version number")
	if major < 16 {
		t.Skipf("a standby can hold a logical slot from PostgreSQL 16; running %d", major)
	}
	pair := testutil.StartPrimaryWithStandby(t, "wal_level=logical", "hot_standby_feedback=on")
	standby, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: pair.StandbyURL})
	require.NoError(t, err)
	t.Cleanup(standby.Close)

	const name = "pg_sprite_standby_slot"
	createSlotOnStandby(t, pair, name)

	status, found, err := decode.InspectSlot(t.Context(), standby, name)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, status.Logical)
	assert.True(t, status.Own, "a logical slot of the pool's database")
	assert.Equal(t, decode.WALStatusReserved, status.WALStatus, "the slot is readable, not lost")
	assert.NotZero(t, status.RestartLSN, "the slot has a position to measure from")
	assert.NotZero(t, status.ConfirmedFlushLSN, "the slot has a confirmed position to measure from")
	assert.Equal(t, decode.RetainedWAL{}, status.Retained, "no write position to measure the retained WAL to")
	assert.Equal(t, decode.RetainedWAL{}, status.Unconfirmed, "no write position to measure the unconfirmed WAL to")

	watch, err := decode.NewSlotLagWatch(decode.DefaultSlotLagCeiling, decode.RestartReleaseInterval)
	require.NoError(t, err)
	assert.ErrorIs(t, watch.Judge(status, time.Now()), decode.ErrSlotLagUnknown)
}

// createSlotOnStandby creates a logical slot of the standby's database on
// the standby. The creation waits for a record of the primary's running
// transactions to be replayed, which the primary otherwise logs only on
// its background writer's interval, so the primary is asked to log one
// until the creation returns.
func createSlotOnStandby(t *testing.T, pair testutil.PrimaryStandby, name string) {
	t.Helper()
	primary, err := pgx.Connect(t.Context(), pair.PrimaryURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, primary.Close(context.WithoutCancel(t.Context())))
	})
	standby, err := pgx.Connect(t.Context(), pair.StandbyURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, standby.Close(context.WithoutCancel(t.Context())))
	})

	// The creation's goroutine is waited for on every return, after the
	// deadline's context is cancelled, so it never outlives the standby
	// connection it uses.
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithTimeout(t.Context(), standbySlotDeadline)
	defer cancel()
	created := make(chan error, 1)
	wg.Go(func() {
		_, err := standby.Exec(ctx, `SELECT pg_catalog.pg_create_logical_replication_slot($1, 'pgoutput')`, name)
		created <- err
	})
	ticker := time.NewTicker(standbySlotPoll)
	defer ticker.Stop()
	for {
		select {
		case err := <-created:
			require.NoError(t, err, "create logical slot %s on the standby", name)
			return
		case <-ticker.C:
			_, err := primary.Exec(ctx, `SELECT pg_catalog.pg_log_standby_snapshot()`)
			require.NoError(t, err, "log the primary's running transactions")
		}
	}
}
