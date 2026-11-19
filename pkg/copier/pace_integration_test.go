package copier_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// A paced copy stops between chunks while its backlog is over the mark and
// carries on once it falls. One worker copies 100-row chunks, so the
// backlog is read once before each claim: the first three readings are
// within the mark and three chunks land, the next two are over it and the
// worker waits twice, and every reading after that is within it. While the
// worker is held the copy has nothing in flight and exactly the three
// landed chunks' rows in the shadow, so a held-back copy leaves no chunk
// open for the applier to wait behind. The held copy then finishes and
// converges, and reports the one wait and the two polls it took.
func TestCopierHoldsBetweenChunksWhileTheBacklogIsOverTheMark(t *testing.T) {
	f := newCopierFixture(t)
	const (
		rows       = 1000
		chunkRows  = 100
		mark       = 64
		poll       = 20 * time.Millisecond
		landedHeld = 3 * chunkRows
	)
	target, lock, shadow := f.prepare(t, rows)

	// readings is the backlog each successive read returns; every read
	// after the last one is within the mark.
	readings := []uint64{0, mark, mark - 1, mark + 1, 4 * mark}
	reads := 0
	backlog := func() uint64 {
		reads++
		if reads > len(readings) {
			return 0
		}
		return readings[reads-1]
	}

	var c *copier.Copier
	var held []copier.Position
	sleep := func(_ context.Context, d time.Duration) error {
		assert.Equal(t, poll, d, "each wait is one poll")
		held = append(held, c.Position())
		return nil
	}

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 1,
		Chunker: copier.ChunkerOptions{InitialRows: chunkRows, MaxRows: chunkRows},
		Pace:    copier.Pace{Backlog: backlog, HighWater: mark, Poll: poll, Sleep: sleep},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))

	require.Len(t, held, 2, "the fourth and fifth readings are over the mark; the sixth is not")
	for _, pos := range held {
		assert.Empty(t, pos.InFlight, "a held worker has claimed no chunk")
		assert.Equal(t, int64(landedHeld), pos.RowsInserted, "three chunks landed before the backlog went over the mark")
		assert.Equal(t, pos.Cut, pos.Watermark, "every chunk cut so far has landed")
	}
	assert.Equal(t, copier.Paced{Waits: 1, Waited: 2 * poll}, c.Paced())

	f.assertConverged(t, shadow)
	pos := c.Position()
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), pos.Watermark, "the held copy still covers the whole key space")
	assert.Equal(t, int64(rows), pos.RowsInserted)
}
