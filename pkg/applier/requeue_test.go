package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

// A requeued batch is the buffer's again, entry for entry: the next Drain
// finds the same keys, OldestPending covers the entries' first events, and
// the moved-from index knows the moved image once more, so a later event
// that reuses its old key flags it.
func TestRequeuePutsTheBatchBack(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 5, col("label", "a"))))
	require.NoError(t, b.Add(keyMove(20, 7, 8, marker("blob"))))
	require.NoError(t, b.Add(del(30, 9)))
	batch := b.Drain(position(t, 100))
	require.Equal(t, []int64{5, 7, 8, 9}, keys(batch.Entries))
	require.Equal(t, 0, b.Len())

	require.NoError(t, b.Requeue(batch))

	assert.Equal(t, 4, b.Len())
	oldest, ok := b.OldestPending()
	require.True(t, ok)
	assert.Equal(t, decode.LSN(10), oldest)
	require.NoError(t, b.Add(insert(40, 7, col("label", "other"), col("blob", "x"))))
	assert.True(t, entry(t, b, 8).OldKeyReused, "the requeued image is indexed under its old key again")

	again := b.Drain(position(t, 100))
	assert.Equal(t, []int64{5, 7, 8, 9}, keys(again.Entries))
	assert.Equal(t, []decode.Column{{Name: "label", Value: "a", Present: true}}, again.Entries[0].Columns)
}

// The stream owner adds nothing between a Drain and its Requeue, so a
// buffered entry under a batch key is a protocol breach: the requeue is
// refused whole and the buffer is as it was.
func TestRequeueRefusesAHeldKey(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 5, col("label", "a"))))
	require.NoError(t, b.Add(update(11, 6, col("label", "b"))))
	batch := b.Drain(position(t, 100))
	require.NoError(t, b.Add(update(12, 6, col("label", "c"))))

	err := b.Requeue(batch)

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(CO-5): requeue key 6")
	assert.Equal(t, 1, b.Len(), "key 5 was not put back either")
	assert.Equal(t, []decode.Column{{Name: "label", Value: "c", Present: true}}, entry(t, b, 6).Columns)
}

func TestRequeueOfAnEmptyBatchIsANoOp(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Requeue(Batch{}))
	assert.Equal(t, 0, b.Len())
	_, ok := b.OldestPending()
	assert.False(t, ok)
}
