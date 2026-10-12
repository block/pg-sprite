package schemachange_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests below read checkpoint rows against slots of the derived name
// that are not the run's to resume from: a slot that is not this route's,
// and the route's own slot while another backend holds it.

// holdSlot opens a second replication connection and starts streaming from
// the slot, which is what makes a slot active: the walsender holds it until
// the connection closes. The returned closer releases it.
func (f reconcileFixture) holdSlot(t *testing.T, slot *decode.Slot) func() {
	t.Helper()
	holder, err := dbconn.ConnectReplication(t.Context(), f.cfg)
	require.NoError(t, err)
	publication := "'" + strings.ReplaceAll(slot.Name(), "'", "''") + "'"
	err = pglogrepl.StartReplication(t.Context(), holder, slot.Name(), pglogrepl.LSN(slot.ConsistentPoint()),
		pglogrepl.StartReplicationOptions{
			Mode:       pglogrepl.LogicalReplication,
			PluginArgs: []string{"proto_version '1'", "publication_names " + publication},
		})
	require.NoError(t, err)
	const slotHeld = 10 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
		if !assert.NoError(collect, err) || !assert.True(collect, found) {
			return
		}
		assert.True(collect, status.Active, "the streaming walsender must hold the slot")
	}, slotHeld, 50*time.Millisecond)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		assert.NoError(t, holder.Close(context.WithoutCancel(t.Context())))
	}
	t.Cleanup(release)
	return release
}

// awaitSlotReleased polls until the catalog shows nothing holding the slot,
// which is after the holder's walsender has exited.
func (f reconcileFixture) awaitSlotReleased(t *testing.T, name string) {
	t.Helper()
	const slotReleased = 10 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, found, err := decode.InspectSlot(t.Context(), f.pool, name)
		if !assert.NoError(collect, err) || !assert.True(collect, found) {
			return
		}
		assert.False(collect, status.Active, "the holder's walsender must have let go")
	}, slotReleased, 50*time.Millisecond)
}

// assertRefusesForeignSlot reads cp as a clean-phase row and as a row
// already reconciling, and expects both to refuse the slot under the name
// as foreign — neither a clean resume nor a reconcile may touch it — while
// the slot itself stays.
func assertRefusesForeignSlot(t *testing.T, f reconcileFixture, cp checkpoint.Checkpoint) {
	t.Helper()
	reconciling := cp
	reconciling.Phase = checkpoint.PhaseReconciling
	for _, row := range []checkpoint.Checkpoint{cp, reconciling} {
		_, err := schemachange.InspectResume(t.Context(), f.pool, f.target, row)
		assert.Equal(t, schemachange.CauseResumeSlotForeign, schemachange.RefusalCauseOf(err), "phase %s", row.Phase)
		assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)
		assert.ErrorIs(t, err, decode.ErrForeignDecodingState, "the refusal routes as the decode package's foreign state")
		var foreign *decode.ForeignStateError
		if assert.ErrorAs(t, err, &foreign) {
			assert.Equal(t, decode.ForeignObjectSlot, foreign.Object)
			assert.Equal(t, cp.SlotName, foreign.Name)
		}
	}
	status, found, err := decode.InspectSlot(t.Context(), f.pool, cp.SlotName)
	require.NoError(t, err)
	assert.True(t, found, "the foreign slot is left alone")
	assert.False(t, status.Own)
}

// A slot wearing the derived name that is physical, or that belongs to
// another database, is not the run's slot however the row reads: it is
// refused as resume-slot-foreign in every phase rather than read as clean
// or handed to a reconcile that would drop it, and it is left in place.
func TestInspectResumeRefusesAForeignSlotOfTheName(t *testing.T) {
	f := newReconcileFixture(t, 100)
	shadow := f.buildShadow(t)
	name := f.target.DecodingName()
	cp := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), name, 0)

	// A physical slot in the pool's own database.
	_, err := f.pool.Exec(t.Context(), `SELECT pg_create_physical_replication_slot($1)`, name)
	require.NoError(t, err)
	assertRefusesForeignSlot(t, f, cp)
	_, err = f.pool.Exec(t.Context(), `SELECT pg_drop_replication_slot($1)`, name)
	require.NoError(t, err)

	// A logical slot of the name in another database on the same cluster.
	other, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.NewDatabase(t, f.serverURL)})
	require.NoError(t, err)
	t.Cleanup(other.Close)
	_, err = other.Exec(t.Context(), `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := other.Exec(context.WithoutCancel(t.Context()), `SELECT pg_drop_replication_slot($1)`, name)
		assert.NoError(t, err)
	})
	assertRefusesForeignSlot(t, f, cp)
}

// The run's own slot, intact but held by another backend, is not readable
// now and is not lost: a clean-phase row is refused as resume-slot-held,
// naming the holder, and reads as clean once the holder has let go. A row
// already reconciling is not refused — the reconcile's drop waits the
// holder out — and reports the held slot for the caller to see.
func TestInspectResumeRefusesTheSlotWhileAnotherBackendHoldsIt(t *testing.T) {
	f := newReconcileFixture(t, 100)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	cp := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), slot.Name(), slot.ConsistentPoint())
	release := f.holdSlot(t, slot)

	_, err = schemachange.InspectResume(t.Context(), f.pool, f.target, cp)
	assert.Equal(t, schemachange.CauseResumeSlotHeld, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrInvariantViolation)

	reconciling := cp
	reconciling.Phase = checkpoint.PhaseReconciling
	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, reconciling)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	assert.True(t, resume.Found)
	assert.True(t, resume.Slot.Active, "the row decided; the held slot is reported, not refused")

	release()
	f.awaitSlotReleased(t, slot.Name())
	resume, err = schemachange.InspectResume(t.Context(), f.pool, f.target, cp)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeClean, resume.Mode)
	assert.False(t, resume.Slot.Active)
}

// A slot the server has given up on — its WAL removed past the slot's
// restart position — still exists, so "found" says nothing about whether
// it can be resumed from. InspectResume reads the server's own verdict and
// answers reconcile, carrying it, rather than clean. The cluster keeps one
// segment for slots; writing past it and checkpointing loses the slot.
func TestInspectResumeReadsASlotTheServerReportsLostAsReconcile(t *testing.T) {
	f := newReconcileFixtureOn(t, 100, []string{"max_slot_wal_keep_size=16MB", "min_wal_size=32MB", "max_wal_size=32MB"})
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	require.NoError(t, slot.Close(t.Context()))
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	cp := f.row(shadow, checkpoint.PhaseCatchingUp, copier.NewWatermark(math.MaxInt64), slot.Name(), slot.ConsistentPoint())

	f.exec(t, `INSERT INTO %s (uniq, amount, label, blob)
		SELECT g, 1, 'lost', repeat('x', 1000) FROM generate_series(1001, 80000) AS g`)
	for _, sql := range []string{`SELECT pg_switch_wal()`, `CHECKPOINT`, `SELECT pg_switch_wal()`, `CHECKPOINT`} {
		_, err := f.pool.Exec(t.Context(), sql)
		require.NoError(t, err, sql)
	}

	resume, err := schemachange.InspectResume(t.Context(), f.pool, f.target, cp)
	require.NoError(t, err)
	assert.Equal(t, schemachange.ResumeReconcile, resume.Mode)
	assert.True(t, resume.Found, "the slot still exists; the server has only given up on it")
	assert.Equal(t, decode.WALStatusLost, resume.Slot.WALStatus)
	require.NotNil(t, resume.Lost)
	assert.Equal(t, slot.Name(), resume.Lost.Slot)
	assert.True(t, resume.Lost.Found)
	assert.Equal(t, decode.WALStatusLost, resume.Lost.WALStatus)
	assert.Equal(t, resume.Slot.Invalidation, resume.Lost.Invalidation, "the server's cause, when it reports one, rides on the error")
}
