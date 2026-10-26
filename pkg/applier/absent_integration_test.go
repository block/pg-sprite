package applier_test

import (
	"context"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// The tests here play the stream owner against a copier whose chunk read
// missed a row the source had already deleted: the shadow holds no row for
// the key, and the stream has delivered the UPDATE that touched the row but
// not yet the DELETE that removed it. The shadow row is removed by hand to
// stand for the chunk that never copied it (CO-4, CO-8, D13).

// An UPDATE that left the out-of-line document unchanged arrives as a marker
// image; the row was deleted on the source before the chunk covering its key
// was read, so the shadow has no row to keep the document from. The flush
// writes nothing and holds the image; the DELETE the stream delivers next
// replaces it, and the shadow converges with the source.
func TestFlushHoldsAMarkerImageWhoseRowTheChunkNeverCopied(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.seats WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.Equal(t, applier.Result{Held: result.Held}, result, "nothing is written for the image")
	held := result.Held[0]
	assert.Equal(t, int64(2), held.Entry.Key)
	assert.True(t, held.Entry.AbsenceAwaited)
	assert.Empty(t, held.Completed, "no row was read, so nothing completes the marker")
	assert.Positive(t, held.ReadLSN)
	assert.Equal(t, 1, buffer.Held())
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 2)
	assert.False(t, present, "no row was invented for the held image")

	require.NoError(t, buffer.Add(deleteEvent(2, 2)))
	assert.Equal(t, 0, buffer.Release(passed(result.Held)), "the DELETE replaced the held image")
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Deletes: 1}, result)
	f.assertConverged(t, p.shadow)
}

// The row was deleted before the chunk was read, so the chunk never copied it,
// and a new row was inserted under the same key after the chunk read: the
// shadow has no row while the source holds one. The DELETE and INSERT the
// stream delivers after the held UPDATE leave a whole image, which lands the
// new row.
func TestFlushHeldAbsentImageIsReplacedByAnInsertOfTheKey(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.seats WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	f.exec(t, `INSERT INTO %s.seats (id, slot, doc, note) VALUES (2, 'R', 'the new row''s document', 'seat 2 again')`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	require.NoError(t, buffer.Add(deleteEvent(2, 2)))
	require.NoError(t, buffer.Add(insertEvent(3, 2, col("slot", "R"), col("doc", "the new row's document"), col("note", "seat 2 again"))))
	assert.Equal(t, 0, buffer.Release(passed(result.Held)), "the events replaced the held image")
	result = f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	doc, _ := f.text(t, p.shadow.ShadowTable(), "doc", 2)
	assert.Equal(t, "the new row's document", *doc)
	f.assertConverged(t, p.shadow)
}

// The check reads the WAL position after the source's DELETE committed, so
// the held image stays out of every drain until the stream has delivered
// that DELETE: a stream that stops just short of it releases nothing, and
// the image is not flushed ahead of the change that explains the absence.
func TestHeldAbsentImageWaitsForTheStreamToPassTheCheck(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.seats WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_insert_lsn()::text`).Scan(&text))
	deleteDone, err := decode.ParseLSN(text)
	require.NoError(t, err)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.GreaterOrEqual(t, result.Held[0].ReadLSN, deleteDone, "the check reads a position at or past the DELETE's commit")
	assert.Equal(t, 0, buffer.Release(deleteDone-1), "the stream has not delivered the DELETE")
	assert.Equal(t, 1, buffer.Held())
	next := buffer.Drain(everyKeyLanded(), applier.AllVisible())
	assert.Empty(t, next.Entries, "the held image is not flushed before the stream passes the check")
	assert.Equal(t, 1, next.Held)
}

// A batch whose column-wise writes collide on the unique slot falls back to
// whole-row writes; an absent-row image the presence check held before the
// collision stays held through the fallback, and no row is invented for it.
func TestFallbackKeepsAnAbsentRowHold(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 4)
	p := f.prepare(t, "seats")
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 3`)
	doc1, _ := f.text(t, "seats", "doc", 1)
	doc2, _ := f.text(t, "seats", "doc", 2)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), col("doc", *doc1)),
		image(2, col("slot", "A"), col("doc", *doc2)),
		image(3, col("slot", "C"), marker("doc")),
	))

	require.NoError(t, err)
	assert.True(t, result.Fallback)
	require.Len(t, result.Held, 1)
	assert.Equal(t, int64(3), result.Held[0].Entry.Key)
	assert.Equal(t, 2, result.Images)
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 3)
	assert.False(t, present, "no row was invented for the held image")
	slot, _ := f.text(t, p.shadow.ShadowTable(), "slot", 1)
	assert.Equal(t, "B", *slot)
}

// A row the source still holds, whose shadow row is gone with no DELETE on the
// way, is not one the stream can explain: the image is held once, and when the
// stream has passed the read and delivered nothing for the key, the next flush
// refuses it and writes nothing (CO-8).
func TestFlushRefusesAnAbsentRowOnceTheStreamHasPassedTheCheck(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	assert.Equal(t, 1, buffer.Release(passed(result.Held)), "the stream passed the check and nothing replaced the image")
	_, err := p.flush.Flush(t.Context(), f.pool, buffer.Drain(everyKeyLanded(), applier.AllVisible()))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.ErrorContains(t, err, "(CO-8): flush key 2")
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 2)
	assert.False(t, present, "no row was invented for the refused image")
}

// A later UPDATE of the held key that supplies every marker column leaves a
// whole image, which a plain upsert would write as a new row. The image still
// carries the flag, and the stream still holds the row live under a key the
// shadow lost, so once the stream has passed the check the flush refuses it
// and writes nothing (CO-8).
func TestFlushRefusesAHeldImageAnUpdateFilledInWhenTheRowIsStillAbsent(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET doc = 'a new document' WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	require.Len(t, result.Held, 1)
	require.NoError(t, buffer.Add(updateEvent(2, 2, col("slot", "Q"), col("doc", "a new document"), col("note", "seat 2"))))
	assert.Equal(t, 1, buffer.Release(passed(result.Held)), "an UPDATE keeps the held image pending")
	_, err := p.flush.Flush(t.Context(), f.pool, buffer.Drain(everyKeyLanded(), applier.AllVisible()))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.ErrorContains(t, err, "(CO-8): flush key 2")
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 2)
	assert.False(t, present, "no row was written for the image")
}

// The swap's drain flushes inside the swap transaction, with the whole stream
// already gathered: it flushes, takes back what the flush held, releases every
// completion, and flushes again. A row the source still holds and the shadow
// has lost is held by the first flush and refused by the second, so the swap
// cannot rename a shadow that is missing a live row (CO-8).
func TestFlushInRefusesALostRowOnTheSecondPassOfTheSwapDrain(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadWrite})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })

	first, err := p.flush.FlushIn(t.Context(), tx, buffer.Drain(everyKeyLanded(), applier.AllVisible()))
	require.NoError(t, err)
	require.Len(t, first.Held, 1)
	require.NoError(t, buffer.Hold(first.Held))
	assert.Equal(t, 1, buffer.Release(math.MaxUint64))
	_, err = p.flush.FlushIn(t.Context(), tx, buffer.Drain(everyKeyLanded(), applier.AllVisible()))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.ErrorContains(t, err, "(CO-8): flush key 2")
}

// A key whose shadow row is present is written column-wise as before, with no
// extra hold: the check finds the row and the document stays in place.
func TestFlushWritesAMarkerImageWhoseRowIsPresent(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	before, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET slot = 'Q' WHERE id = 2`)
	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Add(updateEvent(1, 2, col("slot", "Q"), marker("doc"), col("note", "seat 2"))))

	result := f.flushAll(t, p, buffer)

	assert.Equal(t, applier.Result{Images: 1}, result)
	after, _ := f.text(t, p.shadow.ShadowTable(), "doc", 2)
	assert.Equal(t, *before, *after)
	f.assertConverged(t, p.shadow)
}
