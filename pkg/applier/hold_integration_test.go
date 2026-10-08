package applier_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// The tests here run the real Buffer in front of the flusher across several
// drains, because a moved image's completion is held back rather than
// written (D13): the flush reads it, the buffer keeps the image until the
// stream has passed the read's position, and a later flush writes it. Each
// test plays the stream owner — Add, Drain, Flush, Hold, Release — against a
// copier that has landed every chunk.

// everyKeyLanded is the copier's position once every chunk has landed, so
// Drain flushes every buffered key.
func everyKeyLanded() copier.Position {
	return copier.Position{Watermark: copier.NewWatermark(math.MaxInt64), Cut: copier.NewWatermark(math.MaxInt64)}
}

func moveEvent(lsn decode.LSN, from, to int64, cols ...decode.Column) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Update, LSN: lsn, Key: to, OldKey: &from, Columns: cols}
}

func insertEvent(lsn decode.LSN, key int64, cols ...decode.Column) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Insert, LSN: lsn, Key: key, Columns: cols}
}

func updateEvent(lsn decode.LSN, key int64, cols ...decode.Column) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Update, LSN: lsn, Key: key, Columns: cols}
}

func deleteEvent(lsn decode.LSN, key int64) decode.ChangeEvent {
	return decode.ChangeEvent{Kind: decode.Delete, LSN: lsn, Key: key}
}

// flushAll drains every landed key and flushes it, then puts the held
// images back into the buffer with their completions pending, as the stream
// owner does after every flush.
func (f flushFixture) flushAll(t *testing.T, p prepared, buffer *applier.Buffer) applier.Result {
	t.Helper()
	result, err := p.flush.Flush(t.Context(), f.pool, buffer.Drain(everyKeyLanded()))
	require.NoError(t, err)
	require.NoError(t, buffer.Hold(result.Held))
	return result
}

// passed is the stream position that lets every completion in held in: the
// highest of their read positions.
func passed(held []applier.HeldImage) decode.LSN {
	var highest decode.LSN
	for _, h := range held {
		if h.ReadLSN > highest {
			highest = h.ReadLSN
		}
	}
	return highest
}

// A move whose old key's shadow row is present is completed from that row,
// and the completion is held rather than written: the first flush deletes
// the old key's row and writes nothing under the new key, and once the
// stream has passed the read the next flush writes the image whole, with
// the document the marker stood for (CO-8, D13).
func TestFlushHoldsAMoveCompletedFromTheOldKeysShadowRow(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	held := result.Held[0]
	assert.Equal(t, applier.Result{Deletes: 1, Held: result.Held}, result, "the delete at the old key is written; the image is held")
	assert.Equal(t, int64(10), held.Entry.Key)
	assert.False(t, held.FromSource, "completed from the old key's shadow row")
	assert.Positive(t, held.ReadLSN)
	assert.Equal(t, []decode.Column{col("doc", *doc2)}, held.Completed)
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 10)
	assert.False(t, present, "nothing is written under the new key until the completion is released")
	assert.Equal(t, 1, buffer.Held())

	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got)
	f.assertConverged(t, p.shadow)
}

// A move whose old key never landed — the copier read the old key's chunk
// after the row had left it, so no shadow row holds the pre-move version
// under either key — is completed from the live source row under the new
// key, and held the same way (D13).
func TestFlushCompletesAMoveWhoseOldKeyNeverLandedFromTheSource(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.True(t, result.Held[0].FromSource)
	assert.Equal(t, []decode.Column{col("doc", *doc2)}, result.Held[0].Completed)
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got)
	f.assertConverged(t, p.shadow)
}

// A moved image whose old key the source has since reused is never
// completed from the old key's shadow row, which is the other row's: the
// copier read that chunk after the reuse, so the shadow row under key 2 is
// the new row's document, and the moved row's document comes from the
// source (D13).
func TestFlushCompletesAReusedOldKeyFromTheSource(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	f.exec(t, `INSERT INTO %s.seats (id, slot, doc, note) VALUES (2, 'Z', 'the other row''s document', 'seat 2 again')`)
	// The copier read key 2's chunk after the reuse: the shadow row under
	// key 2 is the other row's.
	f.exec(t, `UPDATE %s.`+p.shadow.ShadowTable()+` SET slot = 'Z', doc = 'the other row''s document' WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), marker("doc"), col("note", "seat 2"))))
	require.NoError(t, buffer.Add(insertEvent(2, 2, col("slot", "Z"), col("doc", "the other row's document"), col("note", "seat 2 again"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.True(t, result.Held[0].FromSource, "the old key's shadow row is another row's")
	assert.Equal(t, applier.Result{Images: 1, Held: result.Held}, result, "the row that took the old key is written; the moved image is held")
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got, "the moved row's own document, not the row that took its old key")
	f.assertConverged(t, p.shadow)
}

// A moved image with a marker that neither the old key's shadow row nor the
// source row under the new key can complete has been removed from the
// source since: the flush writes a delete at its key, names the key in
// Skipped, and invents no document (D13).
func TestFlushSkipsAMovedImageWhoseRowIsGone(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.seats WHERE id = 10`)
	f.exec(t, `UPDATE %s.seats SET slot = 'Y' WHERE id = 3`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(3, col("slot", "Y"), marker("doc")),
		moved(2, 10, col("slot", "B"), marker("doc")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1, Deletes: 1, Skipped: []int64{10}}, result)
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 10)
	assert.False(t, present, "no row was invented for the skipped image")
	f.assertConverged(t, p.shadow)
}

// A key-moving UPDATE onto a key whose row was deleted earlier in the same
// window replaces the delete marker, so the image is the only record that
// the shadow row under the key has to go. Row 3 moves onto key 2 after the
// row there was deleted, then on to key 10; the copier read key 3's chunk
// after the first move. The first flush has nothing to complete key 2 from
// and writes the skip as a delete, so the second flush finds no predecessor
// row under key 2 and completes the move to key 10 with row 3's own
// document (D13).
func TestFlushSkippedMoveLeavesNoPredecessorRow(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc3, _ := f.text(t, "seats", "doc", 3)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 3`)
	f.exec(t, `DELETE FROM %s.seats WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET id = 2 WHERE id = 3`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(deleteEvent(1, 2)))
	require.NoError(t, buffer.Add(moveEvent(2, 3, 2, col("slot", "C"), marker("doc"), col("note", "seat 3"))))

	result := f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Deletes: 2, Skipped: []int64{2}}, result, "the delete at key 3 and the skip written as a delete at key 2")
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 2)
	assert.False(t, present, "the deleted row under key 2 is gone")

	require.NoError(t, buffer.Add(moveEvent(3, 2, 10, col("slot", "C"), marker("doc"), col("note", "seat 3"))))
	result = f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.True(t, result.Held[0].FromSource, "no shadow row under key 2 to complete from")
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	f.flushAll(t, p, buffer)

	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc3, *got, "the moved row keeps its own document")
	f.assertConverged(t, p.shadow)
}

// The source row under the new key is read at flush time, and the stream
// can be behind it. Row 3 moves to key 10 and on to 11, and a new row is
// inserted at key 10, all before the first flush reads the source: that
// read finds the new row. The completion is held, and the move away from
// key 10 that the stream delivers next drops it, so the image is completed
// again from the row it now sits under, and every key ends holding its own
// row's document (CO-8, D13).
func TestFlushSourceCompletionNeverTakesAnotherRowsValue(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc3, _ := f.text(t, "seats", "doc", 3)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 3`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 3`)
	f.exec(t, `UPDATE %s.seats SET id = 11 WHERE id = 10`)
	f.exec(t, `INSERT INTO %s.seats (id, slot, doc, note) VALUES (10, 'Z', 'the other row''s document', 'seat 10')`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 3, 10, col("slot", "C"), marker("doc"), col("note", "seat 3"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.True(t, result.Held[0].FromSource)
	assert.Equal(t, []decode.Column{col("doc", "the other row's document")}, result.Held[0].Completed, "the read saw the row the stream has not delivered yet")
	readAt := passed(result.Held)

	// The stream delivers the two changes the read saw, then passes it.
	require.NoError(t, buffer.Add(moveEvent(2, 10, 11, col("slot", "C"), marker("doc"), col("note", "seat 3"))))
	require.NoError(t, buffer.Add(insertEvent(3, 10, col("slot", "Z"), col("doc", "the other row's document"), col("note", "seat 10"))))
	assert.Equal(t, 0, buffer.Release(readAt), "the move away from key 10 dropped the completion")
	result = f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.Equal(t, int64(11), result.Held[0].Entry.Key)
	assert.Equal(t, []decode.Column{col("doc", *doc3)}, result.Held[0].Completed)
	assert.Equal(t, applier.Result{Images: 1, Held: result.Held}, result, "the new row at key 10 is written; the move to 11 is held")
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	f.flushAll(t, p, buffer)

	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 11)
	assert.Equal(t, *doc3, *got, "the moved row keeps its own document")
	f.assertConverged(t, p.shadow)
}

// The old key's shadow row has the same blind spot: the copier can copy the
// old key's chunk after another row has moved in, before the stream has
// delivered that row's event, so the image is not yet flagged reused and
// the completion reads the other row. The completion is held, and the
// INSERT at the old key that arrives next drops it; the image is then
// flagged reused and completed from the source instead (CO-8, D13).
func TestFlushHeldShadowCompletionIsDroppedWhenTheOldKeyIsReused(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	f.exec(t, `INSERT INTO %s.seats (id, slot, doc, note) VALUES (2, 'Z', 'the other row''s document', 'seat 2 again')`)
	// The copier read key 2's chunk after the reuse.
	f.exec(t, `UPDATE %s.`+p.shadow.ShadowTable()+` SET slot = 'Z', doc = 'the other row''s document' WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.False(t, result.Held[0].FromSource, "the buffer has not seen the reuse yet")
	assert.Equal(t, []decode.Column{col("doc", "the other row's document")}, result.Held[0].Completed)
	readAt := passed(result.Held)

	require.NoError(t, buffer.Add(insertEvent(2, 2, col("slot", "Z"), col("doc", "the other row's document"), col("note", "seat 2 again"))))
	assert.Equal(t, 0, buffer.Release(readAt), "the reuse of the old key dropped the completion")
	result = f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.True(t, result.Held[0].FromSource)
	assert.Equal(t, []decode.Column{col("doc", *doc2)}, result.Held[0].Completed)
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	f.flushAll(t, p, buffer)

	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got, "the moved row's own document, not the row that took its old key")
	f.assertConverged(t, p.shadow)
}

// An UPDATE of the held key that arrives before its completion is released
// keeps the completion pending: the update's values are newer than the
// read's, and when the completion is taken it fills only the columns the
// image still carries as markers (D13).
func TestFlushHeldCompletionFillsOnlyTheMarkersLeft(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 10`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(moveEvent(1, 2, 10, col("slot", "B"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	require.NoError(t, buffer.Add(updateEvent(2, 10, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))
	assert.Equal(t, 1, buffer.Held(), "a plain update keeps the completion pending")
	assert.Equal(t, 1, buffer.Release(passed(result.Held)))
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	slot, _ := f.text(t, p.shadow.ShadowTable(), "slot", 10)
	assert.Equal(t, "Q", *slot, "the update's value, newer than the read")
	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got, "the completion filled the marker")
	f.assertConverged(t, p.shadow)
}
