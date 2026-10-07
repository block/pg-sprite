package decode_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// Dropping removes both the slot and its publication, and dropping what is
// already gone is success, so a cleanup path can call it without first
// asking whether an earlier attempt got through.
func TestDropSlotRemovesSlotAndPublicationIdempotently(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	require.NoError(t, decode.DropSlot(t.Context(), f.cfg, f.pool, name), "nothing to drop is not an error")

	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))

	require.NoError(t, decode.DropSlot(t.Context(), f.cfg, f.pool, name))
	_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.False(t, found)
	assert.False(t, f.publicationExists(t, name))
	require.NoError(t, decode.DropSlot(t.Context(), f.cfg, f.pool, name), "a second drop finds nothing and succeeds")
}

// A walsender still streaming from the slot holds it; the drop waits for the
// holder to let go instead of failing, then completes.
func TestDropSlotWaitsForTheWALSenderToReleaseTheSlot(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	require.NoError(t, slot.Close(t.Context()))
	release := f.holdSlot(t, slot)

	const dropDeadline = 30 * time.Second
	dropCtx, cancel := context.WithTimeout(t.Context(), dropDeadline)
	defer cancel()
	dropped := make(chan error, 1)
	go func() { dropped <- decode.DropSlot(dropCtx, f.cfg, f.pool, slot.Name()) }()

	// The drop must still be waiting while the holder streams: the slot
	// is there, active, and the call has not returned.
	const heldFor = 500 * time.Millisecond
	select {
	case err := <-dropped:
		t.Fatalf("drop returned while the walsender held the slot: %v", err)
	case <-time.After(heldFor):
	}
	status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, status.Active)

	release()
	select {
	case err := <-dropped:
		require.NoError(t, err)
	case <-dropCtx.Done():
		t.Fatalf("drop did not complete after the holder released the slot: %v", dropCtx.Err())
	}
	_, found, err = decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	assert.False(t, found)
	assert.False(t, f.publicationExists(t, slot.Name()))
}

// A wait the caller's context ends is reported as that context's error and
// never as a drop: the slot and its publication are still there for the
// next attempt.
func TestDropSlotEndedByItsContextIsNotADrop(t *testing.T) {
	f := newSlotFixture(t)
	slot := f.createSlot(t)
	require.NoError(t, slot.Close(t.Context()))
	release := f.holdSlot(t, slot)

	const waitBudget = 500 * time.Millisecond
	dropCtx, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()
	err := decode.DropSlot(dropCtx, f.cfg, f.pool, slot.Name())
	require.ErrorIs(t, err, context.DeadlineExceeded)

	_, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
	require.NoError(t, err)
	assert.True(t, found, "the slot is not dropped while its holder streams")
	assert.True(t, f.publicationExists(t, slot.Name()))
	release()
}

// A slot wearing the derived name that belongs to another database, or that
// is physical, is never this route's to drop: the drop refuses and the slot
// stays. Only the pool's own logical slot is dropped.
func TestDropSlotRefusesAForeignSlotOfTheName(t *testing.T) {
	f := newSlotFixture(t)
	name := f.target.DecodingName()
	other, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)})
	require.NoError(t, err)
	t.Cleanup(other.Close)

	_, err = other.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
	require.NoError(t, err)
	err = decode.DropSlot(t.Context(), f.cfg, f.pool, name)
	require.ErrorIs(t, err, decode.ErrForeignDecodingState)
	_, found, err := decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.True(t, found, "another database's slot is left alone")
	_, err = other.Exec(t.Context(), `SELECT pg_drop_replication_slot($1)`, name)
	require.NoError(t, err)

	_, err = f.pool.Exec(t.Context(), `SELECT pg_create_physical_replication_slot($1)`, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `SELECT pg_drop_replication_slot($1)`, name)
		assert.NoError(t, err)
	})
	err = decode.DropSlot(t.Context(), f.cfg, f.pool, name)
	require.ErrorIs(t, err, decode.ErrForeignDecodingState)
	_, found, err = decode.InspectSlot(t.Context(), f.pool, name)
	require.NoError(t, err)
	assert.True(t, found, "a physical slot is left alone")
}
