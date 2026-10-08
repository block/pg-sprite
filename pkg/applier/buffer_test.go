package applier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/decode"
)

func col(name string, value any) decode.Column {
	return decode.Column{Name: name, Value: value, Present: true}
}

// marker is pgoutput's unchanged-TOAST column: present in the tuple, no value.
func marker(name string) decode.Column { return decode.Column{Name: name, Present: false} }

func insert(lsn decode.LSN, key int64, cols ...decode.Column) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Insert, LSN: lsn, Delivered: lsn, Key: key, Columns: cols}
}

func update(lsn decode.LSN, key int64, cols ...decode.Column) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Update, LSN: lsn, Delivered: lsn, Key: key, Columns: cols}
}

func keyMove(lsn decode.LSN, from, to int64, cols ...decode.Column) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Update, LSN: lsn, Delivered: lsn, Key: to, OldKey: &from, Columns: cols}
}

func del(lsn decode.LSN, key int64) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Delete, LSN: lsn, Delivered: lsn, Key: key}
}

func entry(t *testing.T, b *Buffer, key int64) Entry {
	t.Helper()
	e, ok := b.entries[key]
	require.True(t, ok, "key %d is buffered", key)
	return *e
}

// A newer UPDATE overlays only the columns it carries: the earlier value of a
// column the later event omitted survives, and a column no event ever supplied
// stays a marker for the flush to complete (CO-5, CO-8).
func TestBufferUpdateOverlaysPresentColumnsOnly(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 1, col("label", "a"), col("blob", "big"), marker("doc"))))
	require.NoError(t, b.Add(update(20, 1, col("label", "b"), marker("blob"), marker("doc"))))

	got := entry(t, b, 1)
	assert.Equal(t, Image, got.Kind)
	assert.Equal(t, []decode.Column{col("label", "b"), col("blob", "big"), marker("doc")}, got.Columns)
	assert.True(t, got.HasMarker())
	assert.Equal(t, decode.LSN(10), got.FirstLSN, "the entry still owes the stream its first event")
	assert.Equal(t, 1, b.Len())
}

// Column order follows the first image; a name the image never held is
// appended rather than dropped.
func TestBufferUpdateAppendsUnknownColumn(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 1, col("label", "a"))))
	require.NoError(t, b.Add(update(20, 1, col("extra", 7))))
	assert.Equal(t, []decode.Column{col("label", "a"), col("extra", 7)}, entry(t, b, 1).Columns)
}

// A delete replaces the image outright, and an INSERT after it replaces the
// marker with the complete new row (CO-5).
func TestBufferDeleteThenInsertReplaces(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 1, col("label", "a"), marker("doc"))))
	require.NoError(t, b.Add(del(20, 1)))

	got := entry(t, b, 1)
	assert.Equal(t, DeleteMarker, got.Kind)
	assert.Nil(t, got.Columns)
	assert.Equal(t, decode.LSN(10), got.FirstLSN)

	require.NoError(t, b.Add(insert(30, 1, col("label", "z"), col("doc", "new"))))
	got = entry(t, b, 1)
	assert.Equal(t, Image, got.Kind)
	assert.Equal(t, []decode.Column{col("label", "z"), col("doc", "new")}, got.Columns)
	assert.False(t, got.HasMarker())
	assert.Equal(t, decode.LSN(10), got.FirstLSN, "the key has been pending since the first unflushed event")
	assert.Equal(t, 1, b.Len())
}

// Add stores its own copy of the event's columns: a caller reusing its slice
// cannot reach into the buffer.
func TestBufferCopiesEventColumns(t *testing.T) {
	b := NewBuffer()
	cols := []decode.Column{col("label", "a")}
	require.NoError(t, b.Add(insert(10, 1, cols...)))
	cols[0].Value = "mutated"
	assert.Equal(t, "a", entry(t, b, 1).Columns[0].Value)
}

// A key-moving UPDATE is two entries: a delete marker at the old key and an
// image at the new key that remembers the old one (CO-5, D4).
func TestBufferKeyMoveEntersTwoEntries(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 5, 5000, col("label", "a"), marker("doc"))))

	old := entry(t, b, 5)
	assert.Equal(t, DeleteMarker, old.Kind)
	assert.Nil(t, old.OldKey)

	moved := entry(t, b, 5000)
	assert.Equal(t, Image, moved.Kind)
	require.NotNil(t, moved.OldKey)
	assert.Equal(t, int64(5), *moved.OldKey)
	assert.Equal(t, []decode.Column{col("label", "a"), marker("doc")}, moved.Columns)
	assert.Equal(t, 2, b.Len())
}

// When the old key already holds a buffered image, the moved image starts from
// it: a value an earlier event supplied at the old key fills a column the move
// omitted, and the pending window starts at that earlier event.
func TestBufferKeyMoveCarriesOldKeyImageForward(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 5, col("label", "a"), col("doc", "filled"))))
	require.NoError(t, b.Add(keyMove(20, 5, 5000, col("label", "b"), marker("doc"))))

	moved := entry(t, b, 5000)
	assert.Equal(t, []decode.Column{col("label", "b"), col("doc", "filled")}, moved.Columns)
	assert.False(t, moved.HasMarker())
	assert.Equal(t, decode.LSN(10), moved.FirstLSN)
	assert.Equal(t, decode.LSN(10), entry(t, b, 5).FirstLSN)
}

// A row that moves twice inside one flush window still has its pre-buffer
// version under the key it started at, so the final image links there. Key 2
// held a different row, X, whose deletion is buffered and whose shadow row is
// still present, so completing the moved image's marker from key 2 would copy
// X's value into it; the middle key is left a marker of its own.
func TestBufferKeyMoveChainCompletesFromOrigin(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(del(10, 2)))
	require.NoError(t, b.Add(keyMove(20, 1, 2, col("label", "r"), marker("doc"))))
	require.NoError(t, b.Add(keyMove(30, 2, 3, col("label", "r"), marker("doc"))))

	assert.Equal(t, DeleteMarker, entry(t, b, 1).Kind)
	assert.Equal(t, DeleteMarker, entry(t, b, 2).Kind)
	last := entry(t, b, 3)
	assert.True(t, last.HasMarker())
	require.NotNil(t, last.OldKey)
	assert.Equal(t, int64(1), *last.OldKey, "doc's value lives in key 1's shadow row; key 2's shadow row is X's")
	assert.Equal(t, decode.LSN(10), last.FirstLSN, "the window the image continues opened with X's deletion at key 2")
	assert.Equal(t, 3, b.Len())
}

// R moves off key 50 before the copier has read it, and the source then puts
// another row, S, at 50. Whenever the copier reads 50's chunk it copies S, so
// key 50's shadow row is never R's pre-buffer version and the moved image's
// doc marker must not be completed from it. The same holds when the reuse
// happens after the old key's own entry was discarded, and through a chain,
// and when the reuse is seen as an UPDATE of a key the buffer does not hold.
func TestBufferReusedOldKeyIsNotACompletionSource(t *testing.T) {
	t.Run("single move", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 25, col("label", "r"), marker("doc"))))
		require.NoError(t, b.Add(insert(20, 50, col("label", "s"), col("doc", "S-doc"))))
		got := entry(t, b, 25)
		require.True(t, got.HasMarker())
		assert.True(t, got.OldKeyReused, "key 50's shadow row is S's once the copier reads it")
	})
	t.Run("chain, after the old key's marker was discarded", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 15, col("label", "r"), marker("doc"))))
		_ = b.Drain(position(t, 30, [2]int64{11, 20}))
		require.NoError(t, b.Add(insert(20, 50, col("label", "s"), col("doc", "S-doc"))))
		require.NoError(t, b.Add(keyMove(30, 15, 25, col("label", "r"), marker("doc"))))
		got := entry(t, b, 25)
		require.NotNil(t, got.OldKey)
		assert.Equal(t, int64(50), *got.OldKey)
		assert.True(t, got.OldKeyReused)
	})
	t.Run("reuse seen as an update of a key the buffer does not hold", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 25, col("label", "r"), marker("doc"))))
		_ = b.Drain(position(t, 30, [2]int64{21, 30}))
		require.NoError(t, b.Add(update(20, 50, col("label", "s2"))))
		assert.True(t, entry(t, b, 25).OldKeyReused, "a live row at 50 is not R")
	})
	t.Run("reuse by a different row moving onto the old key", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 25, col("label", "r"), marker("doc"))))
		require.NoError(t, b.Add(keyMove(20, 7, 50, col("label", "s"), marker("doc"))))
		assert.True(t, entry(t, b, 25).OldKeyReused)
		assert.False(t, entry(t, b, 50).OldKeyReused, "7 was not reused")
	})
	t.Run("a row returning to its own origin is not a reuse", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 1, 2, col("label", "a"), marker("doc"))))
		require.NoError(t, b.Add(keyMove(20, 2, 1, col("label", "a"), marker("doc"))))
		assert.False(t, entry(t, b, 1).OldKeyReused)
	})
	t.Run("a reuse before the return travels with the row", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 1, 2, col("label", "a"), marker("doc"))))
		require.NoError(t, b.Add(insert(20, 1, col("label", "s"), col("doc", "S-doc"))))
		require.NoError(t, b.Add(del(30, 1)))
		require.NoError(t, b.Add(keyMove(40, 2, 1, col("label", "a"), marker("doc"))))
		assert.True(t, entry(t, b, 1).OldKeyReused, "key 1's shadow row may be S's")
	})
}

// The images that left a key are found through an index the buffer keeps
// exact: an image the drain flushed, or that a later event replaced, is no
// longer found when its old key is reused.
func TestBufferReuseIndexFollowsTheEntries(t *testing.T) {
	t.Run("image drained, then the old key reused", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 25, col("label", "r"), marker("doc"))))
		batch := b.Drain(position(t, 6000))
		require.Equal(t, []int64{25, 50}, keys(batch.Entries))
		require.NoError(t, b.Add(insert(20, 50, col("label", "s"), col("doc", "S-doc"))))
		assert.Equal(t, 1, b.Len())
		assert.Empty(t, b.movedFrom)
	})
	t.Run("moved image deleted, then the old key reused", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 25, col("label", "r"), marker("doc"))))
		require.NoError(t, b.Add(del(20, 25)))
		require.NoError(t, b.Add(insert(30, 50, col("label", "s"), col("doc", "S-doc"))))
		assert.False(t, entry(t, b, 25).OldKeyReused, "the marker at 25 is not an image that left 50")
		assert.Empty(t, b.movedFrom)
	})
	t.Run("moved image moved on, then the origin reused", func(t *testing.T) {
		b := NewBuffer()
		require.NoError(t, b.Add(keyMove(10, 50, 15, col("label", "r"), marker("doc"))))
		require.NoError(t, b.Add(keyMove(20, 15, 25, col("label", "r"), marker("doc"))))
		require.NoError(t, b.Add(insert(30, 50, col("label", "s"), col("doc", "S-doc"))))
		assert.True(t, entry(t, b, 25).OldKeyReused)
		assert.False(t, entry(t, b, 15).OldKeyReused, "the marker at 15 is not an image that left 50")
		assert.Equal(t, map[int64]map[int64]struct{}{50: {25: {}}}, b.movedFrom)
	})
}

// A row that moves away and back is still a moved image with OldKey equal to
// its Key, not a plain update: its shadow row under that key may be absent
// when the chunk was read while the row was elsewhere, and only a moved image
// may complete from the source instead of failing closed (D13).
func TestBufferKeyMoveBackToOriginKeepsOldKey(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 1, 2, col("label", "a"), marker("doc"))))
	require.NoError(t, b.Add(keyMove(20, 2, 1, col("label", "b"), marker("doc"))))

	got := entry(t, b, 1)
	assert.Equal(t, Image, got.Kind)
	require.NotNil(t, got.OldKey)
	assert.Equal(t, int64(1), *got.OldKey)
	assert.Equal(t, []decode.Column{col("label", "b"), marker("doc")}, got.Columns)
	assert.Equal(t, DeleteMarker, entry(t, b, 2).Kind)
	assert.Equal(t, 2, b.Len())
}

// A row may move onto a key the buffer holds deleted; the marker is replaced.
func TestBufferKeyMoveOntoDeletedKey(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(del(10, 9)))
	require.NoError(t, b.Add(keyMove(20, 1, 9, col("label", "a"))))
	got := entry(t, b, 9)
	assert.Equal(t, Image, got.Kind)
	assert.Equal(t, decode.LSN(10), got.FirstLSN, "the key has been pending since its delete")
}

// A delete of a moved image drops the link: a marker needs no completion, so
// nothing ties it to the old key any more.
func TestBufferDeleteOfMovedImageDropsOldKey(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(keyMove(10, 1, 2, col("label", "a"))))
	require.NoError(t, b.Add(del(20, 2)))
	got := entry(t, b, 2)
	assert.Equal(t, DeleteMarker, got.Kind)
	assert.Nil(t, got.OldKey)
}

// Events the source could not have produced, given what is buffered, fail
// closed and leave the buffer unchanged.
func TestBufferRefusesImpossibleEvents(t *testing.T) {
	cases := map[string]struct {
		setup []decode.ChangeEvent
		event decode.ChangeEvent
	}{
		"update over a deleted row": {
			setup: []decode.ChangeEvent{del(10, 1)},
			event: update(20, 1, col("label", "a")),
		},
		"insert over a live row": {
			setup: []decode.ChangeEvent{update(10, 1, col("label", "a"))},
			event: insert(20, 1, col("label", "b")),
		},
		"insert that omits a column": {
			event: insert(10, 1, col("label", "a"), marker("doc")),
		},
		"key move onto a live row": {
			setup: []decode.ChangeEvent{update(10, 2, col("label", "a"))},
			event: keyMove(20, 1, 2, col("label", "b")),
		},
		"key move from a deleted row": {
			setup: []decode.ChangeEvent{del(10, 1)},
			event: keyMove(20, 1, 2, col("label", "b")),
		},
		"unknown change kind": {
			event: decode.ChangeEvent{Kind: decode.ChangeKind(99), Key: 1},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer()
			for _, ev := range tc.setup {
				require.NoError(t, b.Add(ev))
			}
			before := snapshot(b)
			err := b.Add(tc.event)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Equal(t, before, snapshot(b), "a refused event leaves the buffer as it was")
		})
	}
}

// An UPDATE whose OldKey equals its Key did not move the row and is a plain
// overlay.
func TestBufferUpdateWithSameOldKeyIsPlainUpdate(t *testing.T) {
	b := NewBuffer()
	require.NoError(t, b.Add(update(10, 1, col("label", "a"), col("doc", "d"))))
	require.NoError(t, b.Add(keyMove(20, 1, 1, col("label", "b"), marker("doc"))))
	got := entry(t, b, 1)
	assert.Nil(t, got.OldKey)
	assert.Equal(t, []decode.Column{col("label", "b"), col("doc", "d")}, got.Columns)
	assert.Equal(t, 1, b.Len())
}

func TestBufferOldestPending(t *testing.T) {
	b := NewBuffer()
	_, ok := b.OldestPending()
	assert.False(t, ok, "an empty buffer owes the stream nothing")

	require.NoError(t, b.Add(update(30, 3, col("label", "c"))))
	require.NoError(t, b.Add(update(10, 1, col("label", "a"))))
	require.NoError(t, b.Add(update(20, 2, col("label", "b"))))
	oldest, ok := b.OldestPending()
	require.True(t, ok)
	assert.Equal(t, decode.LSN(10), oldest)
}

// The position an entry owes the stream is the delivered position its
// event arrived with, not the event's own LSN: a change written before an
// earlier transaction committed arrives with an LSN below what the caller
// has already confirmed, and bounding the confirm by that LSN would ask the
// stream to go backwards.
func TestBufferOldestPendingIsTheDeliveredPositionNotTheChangesLSN(t *testing.T) {
	b := NewBuffer()
	writtenFirstCommittedLast := decode.ChangeEvent{
		Kind: decode.Insert, LSN: 50, Delivered: 80, Key: 1, Columns: []decode.Column{col("label", "a")},
	}
	require.NoError(t, b.Add(writtenFirstCommittedLast))

	oldest, ok := b.OldestPending()
	require.True(t, ok)
	assert.Equal(t, decode.LSN(80), oldest)
	assert.Equal(t, decode.LSN(80), b.entries[1].FirstLSN)
}

// Every kind of event keys its entry on the delivered position it arrived
// with, not on its own LSN: an UPDATE of a key the buffer does not hold, a
// DELETE, and a key move, whose moved image and old-key marker both owe the
// stream the same position.
func TestBufferKeysEveryEventKindOnItsDeliveredPosition(t *testing.T) {
	oldKey := int64(1)
	for name, ev := range map[string]decode.ChangeEvent{
		"update":   {Kind: decode.Update, LSN: 50, Delivered: 80, Key: 2, Columns: []decode.Column{col("label", "a")}},
		"delete":   {Kind: decode.Delete, LSN: 50, Delivered: 80, Key: 2},
		"key move": {Kind: decode.Update, LSN: 50, Delivered: 80, Key: 2, OldKey: &oldKey, Columns: []decode.Column{col("label", "a")}},
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer()
			require.NoError(t, b.Add(ev))

			oldest, ok := b.OldestPending()
			require.True(t, ok)
			assert.Equal(t, decode.LSN(80), oldest)
			require.NotEmpty(t, b.entries)
			for key, e := range b.entries {
				assert.Equal(t, decode.LSN(80), e.FirstLSN, "entry for key %d", key)
			}
		})
	}
}

func TestEntryKindStrings(t *testing.T) {
	assert.Equal(t, "image", Image.String())
	assert.Equal(t, "delete-marker", DeleteMarker.String())
	assert.Equal(t, "EntryKind(99)", EntryKind(99).String())
}

func snapshot(b *Buffer) map[int64]Entry {
	out := make(map[int64]Entry, len(b.entries))
	for k, e := range b.entries {
		out[k] = *e
	}
	return out
}
