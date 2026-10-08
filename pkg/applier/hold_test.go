package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// held drains a moved, marker-bearing image out of the buffer and hands it
// back as a flush would: completed with value, read at readLSN.
func held(t *testing.T, b *Buffer, readLSN decode.LSN, value string) HeldImage {
	t.Helper()
	batch := b.Drain(position(t, 100))
	require.Len(t, batch.CompleteFirst(), 1, "exactly one image to complete")
	return HeldImage{
		Entry:     batch.CompleteFirst()[0],
		Completed: []decode.Column{col("blob", value)},
		ReadLSN:   readLSN,
	}
}

// A held image is the buffer's again with its completion pending: Drain
// keeps it whatever the copier's position, counts it as Held, and
// OldestPending still covers its first event; Release at the read position
// lets the completion in, fills the marker, and the next Drain flushes the
// image whole.
func TestHoldKeepsTheImageUntilTheStreamPassesTheRead(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 7, 8, col("label", "a"), marker("blob"))))
	h := held(t, b, 500, "from the row")
	require.Equal(t, 0, b.Len())

	require.NoError(t, b.Hold([]HeldImage{h}))

	assert.Equal(t, 1, b.Len())
	assert.Equal(t, 1, b.Held())
	oldest, ok := b.OldestPending()
	require.True(t, ok)
	assert.Equal(t, decode.LSN(10), oldest, "the confirmed position may not pass the held image")
	batch := b.Drain(position(t, 100))
	assert.Empty(t, batch.Entries, "a held image is not flushed")
	assert.Equal(t, 1, batch.Held)
	assert.Equal(t, 0, batch.Deferred)
	assert.Equal(t, 1, b.Len())

	assert.Equal(t, 0, b.Release(499), "the stream has not passed the read")
	assert.True(t, entry(t, b, 8).HasMarker())
	assert.Equal(t, 1, b.Release(500))
	assert.Equal(t, 0, b.Held())
	batch = b.Drain(position(t, 100))
	require.Equal(t, []int64{8}, keys(batch.Entries))
	assert.Equal(t, 0, batch.Held)
	assert.Equal(t, []decode.Column{
		{Name: "label", Value: "a", Present: true},
		{Name: "blob", Value: "from the row", Present: true},
	}, batch.Entries[0].Columns)
	assert.Empty(t, batch.CompleteFirst(), "the image is whole; nothing to read before writing")
}

// Release takes each completion on its own read position, so a batch whose
// reads straddle the stream's position releases only the ones it has
// passed.
func TestReleaseTakesOnlyTheCompletionsTheStreamHasPassed(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 1, 2, marker("blob"))))
	require.NoError(t, b.Add(keyMove(20, 3, 4, marker("blob"))))
	batch := b.Drain(position(t, 100))
	first := batch.CompleteFirst()
	require.Len(t, first, 2)
	require.NoError(t, b.Hold([]HeldImage{
		{Entry: first[0], Completed: []decode.Column{col("blob", "two")}, ReadLSN: 300},
		{Entry: first[1], Completed: []decode.Column{col("blob", "four")}, ReadLSN: 400},
	}))

	assert.Equal(t, 1, b.Release(350))

	assert.Equal(t, 1, b.Held())
	assert.False(t, entry(t, b, 2).HasMarker())
	assert.True(t, entry(t, b, 4).HasMarker())
	batch = b.Drain(position(t, 100))
	assert.Equal(t, []int64{2}, keys(batch.Entries))
	assert.Equal(t, 1, batch.Held)
}

// The stream owner holds before it adds, so a buffered entry under a held
// key is a protocol breach: the hold is refused whole and the buffer is as
// it was.
func TestHoldRefusesAKeyTheBufferHolds(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 7, 8, marker("blob"))))
	h := held(t, b, 500, "from the row")
	require.NoError(t, b.Add(update(20, 8, col("label", "newer"))))

	err := b.Hold([]HeldImage{h})

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(CO-5): hold key 8")
	assert.Equal(t, 1, b.Len())
	assert.Equal(t, 0, b.Held())
	assert.Equal(t, []decode.Column{{Name: "label", Value: "newer", Present: true}}, entry(t, b, 8).Columns)
}

// A plain UPDATE of the held key keeps the completion pending and its own
// values win: the completion fills only the columns the image still carries
// as markers when it is taken.
func TestReleaseFillsOnlyTheMarkersAnUpdateLeft(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 7, 8, col("label", "a"), marker("blob"), marker("doc"))))
	batch := b.Drain(position(t, 100))
	require.NoError(t, b.Hold([]HeldImage{{
		Entry:     batch.CompleteFirst()[0],
		Completed: []decode.Column{col("blob", "read blob"), col("doc", "read doc")},
		ReadLSN:   500,
	}}))
	require.NoError(t, b.Add(update(600, 8, col("label", "b"), col("blob", "newer blob"), marker("doc"))))

	assert.Equal(t, 1, b.Held(), "an update of the key keeps the completion pending")
	assert.Equal(t, 1, b.Release(500))
	assert.Equal(t, []decode.Column{
		{Name: "label", Value: "b", Present: true},
		{Name: "blob", Value: "newer blob", Present: true},
		{Name: "doc", Value: "read doc", Present: true},
	}, entry(t, b, 8).Columns)
}

// An event that replaces the held key's entry — a delete, or the row moving
// on — drops the completion: the read may have seen that event's row. The
// image that moved on inherits the old key and is completed afresh.
func TestAnEventThatReplacesTheHeldKeyDropsTheCompletion(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 7, 8, marker("blob"))))
		require.NoError(t, b.Hold([]HeldImage{held(t, b, 500, "from the row")}))

		require.NoError(t, b.Add(del(600, 8)))

		assert.Equal(t, 0, b.Held())
		assert.Equal(t, 0, b.Release(500))
		assert.Equal(t, DeleteMarker, entry(t, b, 8).Kind)
	})
	t.Run("move on", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 7, 8, marker("blob"))))
		require.NoError(t, b.Hold([]HeldImage{held(t, b, 500, "from the row")}))

		require.NoError(t, b.Add(keyMove(600, 8, 9, marker("blob"))))

		assert.Equal(t, 0, b.Held())
		assert.Equal(t, 0, b.Release(500))
		moved := entry(t, b, 9)
		assert.True(t, moved.HasMarker(), "the dropped completion is not carried on")
		require.NotNil(t, moved.OldKey)
		assert.Equal(t, int64(7), *moved.OldKey, "the row's shadow row is still under the key it started at")
		batch := b.Drain(position(t, 100))
		assert.Equal(t, []int64{8, 9}, keys(batch.Entries))
		assert.Equal(t, []int64{9}, keys(batch.CompleteFirst()))
	})
}

// A completion read from the old key's shadow row is dropped when a later
// event lands another row on that key: the copier may have copied that
// row, and the read may have seen it. The image is flagged reused, so the
// next flush completes it from the source.
func TestReuseOfTheOldKeyDropsAShadowCompletion(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 7, 8, marker("blob"))))
	require.NoError(t, b.Hold([]HeldImage{held(t, b, 500, "maybe another row's")}))

	require.NoError(t, b.Add(insert(600, 7, col("label", "other"), col("blob", "x"))))

	assert.Equal(t, 0, b.Held())
	assert.Equal(t, 0, b.Release(500))
	moved := entry(t, b, 8)
	assert.True(t, moved.OldKeyReused)
	assert.True(t, moved.HasMarker())
	batch := b.Drain(position(t, 100))
	assert.Equal(t, []int64{7, 8}, keys(batch.Entries))
	assert.Equal(t, []int64{8}, keys(batch.CompleteFirst()))
}

// A hold keeps the batch's copy of the image and its completion, so a
// later write to either slice by the caller changes nothing buffered.
func TestHoldCopiesTheImageAndItsCompletion(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 7, 8, col("label", "a"), marker("blob"))))
	h := held(t, b, 500, "from the row")
	require.NoError(t, b.Hold([]HeldImage{h}))

	h.Entry.Columns[0].Value = "changed by the caller"
	h.Completed[0].Value = "changed by the caller"
	require.Equal(t, 1, b.Release(500))

	assert.Equal(t, []decode.Column{
		{Name: "label", Value: "a", Present: true},
		{Name: "blob", Value: "from the row", Present: true},
	}, entry(t, b, 8).Columns)
}

func TestHoldOfNothingIsANoOp(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Hold(nil))
	assert.Equal(t, 0, b.Len())
	assert.Equal(t, 0, b.Held())
	assert.Equal(t, 0, b.Release(decode.LSN(1)))
}
