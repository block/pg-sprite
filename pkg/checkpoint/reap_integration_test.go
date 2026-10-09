package checkpoint_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// reapDatabase is one database on a cluster that decodes WAL, with a store
// over it and the means to mint slots for tables of its own: every slot a
// test here creates belongs to one of these, so the reaper's judgement of
// "this database's slots" is observable.
type reapDatabase struct {
	cfg   dbconn.Config
	pool  *pgxpool.Pool
	store *checkpoint.Store
	// schema holds the tables slots are minted for.
	schema string
}

// newReapServer starts the cluster every database of the test shares.
func newReapServer(t *testing.T) string {
	t.Helper()
	return testutil.StartPostgresWithSettings(t, "wal_level=logical")
}

func newReapDatabase(t *testing.T, serverURL string) reapDatabase {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.NewDatabase(t, serverURL)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	return reapDatabase{cfg: cfg, pool: pool, store: store, schema: testutil.NewSchema(t, pool)}
}

// slotFor creates a table of the given name, mints its copy-and-swap proof
// for a run that decodes WAL, and creates its slot, which is closed at once
// so nothing holds it; the slot is dropped after the test if the reaper did
// not get to it.
func (d reapDatabase) slotFor(t *testing.T, table string) (preflight.CopySwapTarget, *decode.Slot) {
	t.Helper()
	_, err := d.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s (
			id bigint PRIMARY KEY,
			note text
		)`, pgx.Identifier{d.schema, table}.Sanitize()))
	require.NoError(t, err)
	target, err := preflight.CheckCopySwap(t.Context(), d.pool, d.schema, table,
		preflight.CopySwapEnvironment{LogicalDecoding: true, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	slot, err := decode.CreateSlot(t.Context(), d.cfg, d.pool, target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), d.cfg, d.pool, slot.Name()))
	})
	return target, slot
}

// claim saves a checkpoint row in phase that names the slot, under the
// table's lock session, the way a run claims its slot before creating it;
// the lock is let go once the row is down so a later claim can take it.
func (d reapDatabase) claim(t *testing.T, table, slotName string, phase checkpoint.Phase) checkpoint.Checkpoint {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), d.cfg, d.schema, table)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release(t.Context())) }()
	cp := checkpoint.Checkpoint{
		Schema:            d.schema,
		Table:             table,
		ShadowTable:       "_pgsprite_" + table + "_new",
		SlotName:          slotName,
		PublicationName:   slotName,
		LastAppliedLSN:    decode.LSN(1),
		SourceFingerprint: "src-" + table,
		TargetFingerprint: "tgt-" + table,
		Phase:             phase,
	}
	require.NoError(t, d.store.Save(t.Context(), lock, cp))
	return cp
}

// slotExists asks the cluster, not the database: a slot the reaper must not
// touch is one in another database.
func slotExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots WHERE slot_name = $1)`, name).Scan(&exists))
	return exists
}

// Two runs created slots; one wrote its row and is mid copy, the other
// left no row behind. The reaper drops only the orphan, publication and
// all, and names the live slot as kept. Once the live run's row turns
// terminal its slot is an orphan too and the next pass drops it (ST-3).
func TestReapOrphanSlotsDropsOrphansAndKeepsSlotsALiveRowNames(t *testing.T) {
	d := newReapDatabase(t, newReapServer(t))
	require.NoError(t, d.store.Ensure(t.Context()))
	_, live := d.slotFor(t, "orders")
	_, orphan := d.slotFor(t, "ledger")
	d.claim(t, "orders", live.Name(), checkpoint.PhaseCopying)

	reaped, err := d.store.ReapOrphanSlots(t.Context(), d.cfg)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{Dropped: []string{orphan.Name()}, Live: []string{live.Name()}}, reaped)
	assert.False(t, slotExists(t, d.pool, orphan.Name()))
	assert.True(t, slotExists(t, d.pool, live.Name()))
	var publications int
	require.NoError(t, d.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_catalog.pg_publication WHERE pubname = $1`, orphan.Name()).Scan(&publications))
	assert.Zero(t, publications, "the orphan's publication goes with its slot")

	d.claim(t, "orders", live.Name(), checkpoint.PhaseDone)
	reaped, err = d.store.ReapOrphanSlots(t.Context(), d.cfg)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{Dropped: []string{live.Name()}}, reaped)
	assert.False(t, slotExists(t, d.pool, live.Name()))

	reaped, err = d.store.ReapOrphanSlots(t.Context(), d.cfg)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{}, reaped, "a clean database has nothing to reap")
}

// A slot a stream holds is somebody's right now, row or no row: the reaper
// reports it as active and leaves it, rather than wait out the holder and
// drop a slot it may be about to claim. A slot of another database on the
// same cluster is never read at all, so it is in no list and stays.
func TestReapOrphanSlotsLeavesHeldSlotsAndOtherDatabasesAlone(t *testing.T) {
	serverURL := newReapServer(t)
	d := newReapDatabase(t, serverURL)
	require.NoError(t, d.store.Ensure(t.Context()))
	target, held := d.slotFor(t, "orders")
	stream, err := decode.OpenStream(t.Context(), d.cfg, d.pool, target, held.ConsistentPoint())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, stream.Close(context.WithoutCancel(t.Context()))) })
	const activeDeadline = 10 * time.Second
	const activePoll = 20 * time.Millisecond
	require.Eventually(t, func() bool {
		status, found, err := decode.InspectSlot(t.Context(), d.pool, held.Name())
		require.NoError(t, err)
		return found && status.Active
	}, activeDeadline, activePoll, "the stream's walsender should hold the slot")

	other := newReapDatabase(t, serverURL)
	_, elsewhere := other.slotFor(t, "orders")
	require.NotEqual(t, held.Name(), elsewhere.Name(), "the same table name in another database derives another slot")

	reaped, err := d.store.ReapOrphanSlots(t.Context(), d.cfg)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{Active: []string{held.Name()}}, reaped)
	assert.True(t, slotExists(t, d.pool, held.Name()))
	assert.True(t, slotExists(t, d.pool, elsewhere.Name()), "another database's orphan is not this database's to drop")
}

// A slot that wears the engine's prefix without its shape is reported as
// foreign and left alone: the reaper drops only what the engine creates.
func TestReapOrphanSlotsReportsAPrefixWearingForeignSlot(t *testing.T) {
	d := newReapDatabase(t, newReapServer(t))
	require.NoError(t, d.store.Ensure(t.Context()))
	const foreign = "pgsprite_backup"
	_, err := d.pool.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, foreign)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := d.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_drop_replication_slot($1)`, foreign)
		assert.NoError(t, err)
	})

	reaped, err := d.store.ReapOrphanSlots(t.Context(), d.cfg)
	require.NoError(t, err)
	assert.Equal(t, checkpoint.Reaped{Foreign: []string{foreign}}, reaped)
	assert.True(t, slotExists(t, d.pool, foreign))
}

// Without the checkpoint table no row can speak for any slot, so the pass
// is refused rather than read every slot as an orphan.
func TestReapOrphanSlotsRefusesWithoutTheCheckpointTable(t *testing.T) {
	d := newReapDatabase(t, newReapServer(t))
	_, orphan := d.slotFor(t, "orders")
	_, err := d.store.ReapOrphanSlots(t.Context(), d.cfg)
	require.ErrorIs(t, err, checkpoint.ErrTableMissing)
	assert.True(t, slotExists(t, d.pool, orphan.Name()), "a refused pass drops nothing")
}
