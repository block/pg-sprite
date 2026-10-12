package schemachange_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The test here runs reconcile mode against a source that is being written
// the whole time: the application does not pause for a reconcile, so a
// recopied chunk a write lands in afterwards is settled by the reopened
// stream, not refused by the pass.

// sourceWriter writes the workload table on a fixed interval until stopped:
// each tick updates one seeded row, cycling through every chunk, and
// inserts one row beyond the seeded keys.
type sourceWriter struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	err    error
	writes int
}

// startWriter runs a sourceWriter over rows seeded keys. The caller stops it
// with stop; the fixture's cleanup stops it too.
func (f reconcileFixture) startWriter(t *testing.T, seeded int, interval time.Duration) *sourceWriter {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	w := &sourceWriter{cancel: cancel}
	update := fmt.Sprintf(`UPDATE %s SET amount = amount + 1 WHERE id = $1`, f.table.Qualified())
	insert := fmt.Sprintf(`INSERT INTO %s (uniq, amount, label, blob) VALUES ($1, 1, 'live', 'x')`, f.table.Qualified())
	w.wg.Go(func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if _, err := f.pool.Exec(ctx, update, int64(i%seeded)+1); err != nil {
				w.err = fmt.Errorf("live update: %w", err)
				return
			}
			if _, err := f.pool.Exec(ctx, insert, 100001+i); err != nil {
				w.err = fmt.Errorf("live insert: %w", err)
				return
			}
			w.writes++
		}
	})
	t.Cleanup(func() { cancel(); w.wg.Wait() })
	return w
}

// stop ends the writer and returns how many ticks wrote both statements; a
// writer that failed before being stopped fails the test.
func (w *sourceWriter) stop(t *testing.T) int {
	t.Helper()
	w.cancel()
	w.wg.Wait()
	require.NoError(t, w.err)
	return w.writes
}

// A reconcile under a steady write stream succeeds: the writer keeps
// updating rows in every chunk and inserting new ones while the recopy pass
// runs, so chunks the pass has recopied are written again before it ends.
// Under a reread policy that would be a repair error; under recopy the pass
// returns its repairs and the stream reopened at the new slot's consistent
// point carries every write that landed after it, and the shadow converges
// and verifies clean once the writer stops.
func TestReconcileUnderASteadyInsertStream(t *testing.T) {
	const seeded = 1500
	f := newReconcileFixture(t, seeded)
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, decode.DropSlot(context.WithoutCancel(t.Context()), f.cfg, f.pool, slot.Name()))
	})
	shadow := f.buildShadow(t)
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCopying, copier.Watermark{}, slot.Name(), slot.ConsistentPoint())))
	require.NoError(t, slot.Close(t.Context()))

	c := newCopier(t, f, shadow)
	first := f.startCapture(t, shadow, c, slot.ConsistentPoint())
	copyAll(t, f, c)
	status := first.awaitConfirmedPast(t, f.currentWALLSN(t))
	require.NoError(t, f.store.Save(t.Context(), f.lock, f.row(shadow, checkpoint.PhaseCatchingUp, c.Position().Watermark, slot.Name(), status.Confirmed)))

	// The failure, and a gap the pass has to repair.
	f.killWalsender(t, first, slot.Name())
	require.NoError(t, first.stream.Close(t.Context()))
	_, err = f.pool.Exec(t.Context(), `SELECT pg_catalog.pg_drop_replication_slot($1)`, slot.Name())
	require.NoError(t, err)
	f.exec(t, `UPDATE %s SET amount = 0 WHERE id BETWEEN 301 AND 310`)

	// The application keeps writing through the reconcile.
	writer := f.startWriter(t, seeded, 10*time.Millisecond)

	reconciler, err := schemachange.NewReconciler(f.cfg, f.lock, f.store, f.target, shadow, checksum.Options{
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	reconciled, err := reconciler.Reconcile(t.Context(), f.pool, f.load(t, shadow))
	require.NoError(t, err, "a write landing in a recopied chunk is the stream's to settle, not the pass's to refuse")
	t.Cleanup(func() { assert.NoError(t, reconciled.Slot.Close(context.WithoutCancel(t.Context()))) })
	assert.NotEmpty(t, reconciled.Outcome.Repairs, "the gap and the live writes left chunks behind")
	assert.Equal(t, checkpoint.PhaseCatchingUp, reconciled.Checkpoint.Phase)

	// Writes that landed during the pass are behind the new slot's
	// consistent point whether or not the pass recopied them: the reopened
	// stream carries them, and only then does the shadow converge.
	second := f.startCapture(t, shadow, c, reconciled.Slot.ConsistentPoint())
	writes := writer.stop(t)
	require.Positive(t, writes, "the writer ran while the reconcile did")
	second.awaitConfirmedPast(t, f.currentWALLSN(t))
	second.stop(t)
	f.assertConverged(t, shadow)

	verifier, err := checksum.NewVerifier(f.target, shadow, f.lock, checksum.Options{})
	require.NoError(t, err)
	report, err := verifier.Verify(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean())
}
