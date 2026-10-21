package applier_test

import (
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
)

// gatedPosition is a finished copy's position that the catch-up cannot read
// until the test opens the gate, so the test decides which source changes have
// committed before the catch-up's first flush.
type gatedPosition struct {
	open chan struct{}
	once sync.Once
}

func newGatedPosition() *gatedPosition { return &gatedPosition{open: make(chan struct{})} }

func (g *gatedPosition) Position() copier.Position {
	<-g.open
	return allLanded()
}

func (g *gatedPosition) release() { g.once.Do(func() { close(g.open) }) }

// Row 2 is updated without touching its out-of-line blob, then deleted, both
// before the catch-up's first flush; the chunk that covered row 2 read it
// after the delete, so the shadow holds no row 2. The catch-up delivers one
// change per cycle, so the UPDATE reaches the flush before the DELETE: the
// flush finds no row to keep the blob from, holds the image instead of
// aborting, the next cycle's DELETE replaces it, and the shadow converges
// with the source (CO-4, CO-8, D13).
func TestCatchupHoldsAMarkerImageForARowTheChunkNeverCopied(t *testing.T) {
	f := newCatchupFixture(t, 20)
	gate := newGatedPosition()
	run := startCatchupWith(t, f, catchupSetup{
		opts:      applier.CatchupOptions{MaxChanges: 1},
		positions: gate,
	})
	// The cleanup that ends the run waits for it, and a run blocked on the gate
	// cannot end: this one is registered later, so it runs first.
	t.Cleanup(gate.release)

	q := f.table.Qualified()
	shadow := pgx.Identifier{f.table.Schema, run.shadow.ShadowTable()}.Sanitize()
	_, err := f.pool.Exec(t.Context(), `DELETE FROM `+shadow+` WHERE id = 2`)
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), `UPDATE `+q+` SET amount = 99 WHERE id = 2`)
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), `DELETE FROM `+q+` WHERE id = 2`)
	require.NoError(t, err)
	gate.release()

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "confirm past the delete with nothing buffered", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	run.stop(t)
	f.assertConverged(t, run.shadow)
}
