package applier_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// The CO-6 vector: seats (1,'A'),(2,'B') exchange slots in one source
// transaction. The key-targeted upserts collide on the unique slot in both
// orders, so the flush falls back to delete-all-then-insert-all and the
// batch converges in one flush (D13).
func TestFlushConvergesAUniqueExchange(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 2)
	p := f.prepare(t, "seats")
	// The source's own index is not deferrable either, so the exchange goes
	// through NULL; the buffer merges the three statements into one image
	// per key.
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 1`)
	f.exec(t, `UPDATE %s.seats SET slot = 'A' WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)
	doc1, _ := f.text(t, "seats", "doc", 1)
	doc2, _ := f.text(t, "seats", "doc", 2)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), col("doc", *doc1)),
		image(2, col("slot", "A"), col("doc", *doc2)),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Fallback: true}, result)
	f.assertConverged(t, p.shadow)
}

// The second CO-6 vector: the exchange moves one row's primary key, and
// both updated rows left their out-of-line document unchanged, so both
// images carry a marker. The moved image is completed from the old key's
// shadow row before anything is written, the fallback completes the other
// from the row under its own key, and both documents survive byte for byte
// (CO-8, D13).
func TestFlushFallbackCompletesMarkersFromTheShadow(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 2)
	p := f.prepare(t, "seats")
	doc1, _ := f.text(t, "seats", "doc", 1)
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)
	f.exec(t, `UPDATE %s.seats SET id = 10, slot = 'A' WHERE id = 2`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), marker("doc")),
		deleted(2),
		moved(2, 10, col("slot", "A"), marker("doc")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Deletes: 1, CompletedFromShadow: 2, Fallback: true}, result)
	f.assertConverged(t, p.shadow)
	got1, _ := f.text(t, p.shadow.ShadowTable(), "doc", 1)
	got10, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc1, *got1, "the document under the unmoved key was completed from its own row")
	assert.Equal(t, *doc2, *got10, "the moved row's document was completed from the old key's row before that row was deleted")
}

// The third CO-6 vector: a move whose old key never landed — the copier
// read the old key's chunk after the row had left it, so no shadow row
// holds the pre-move version under either key — is completed from the live
// source row under the new key (D13).
func TestFlushCompletesAMoveWhoseOldKeyNeverLandedFromTheSource(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)
	// The copier's read of the chunk holding key 2 came after the move and
	// found no row there; the shadow never held one.
	f.exec(t, `DELETE FROM %s.`+p.shadow.ShadowTable()+` WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET id = 10 WHERE id = 2`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(moved(2, 10, col("slot", "B"), marker("doc"))))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1, CompletedFromSource: 1}, result)
	f.assertConverged(t, p.shadow)
	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got)
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
	movedImage := moved(2, 10, col("slot", "B"), marker("doc"))
	movedImage.OldKeyReused = true

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(2, col("slot", "Z"), col("doc", "the other row's document")),
		movedImage,
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, CompletedFromSource: 1}, result)
	f.assertConverged(t, p.shadow)
	got, _ := f.text(t, p.shadow.ShadowTable(), "doc", 10)
	assert.Equal(t, *doc2, *got, "the moved row's own document, not the row that took its old key")
}

// A moved image with a marker that neither the old key's shadow row nor the
// source row under the new key can complete has been removed from the
// source since: a later event deletes or moves the key again, and the flush
// skips the image rather than invent a document (D13).
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
	assert.Equal(t, applier.Result{Images: 1, Skipped: 1}, result)
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 10)
	assert.False(t, present, "no row was invented for the skipped image")
	f.assertConverged(t, p.shadow)
}

// A unique index that refuses the fallback too is a collision with a row
// the batch does not name: here the deferred half of a unique move, whose
// key is inside an in-flight chunk. The flush writes nothing and reports
// the batch deferred; the stream owner requeues it, and once the other half
// drains with it the batch converges.
func TestFlushDefersABatchAUniqueIndexStillRefuses(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 2)
	p := f.prepare(t, "seats")
	doc1, _ := f.text(t, "seats", "doc", 1)
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)

	deferred := batch(image(1, col("slot", "B"), col("doc", *doc1)))
	_, err := p.flush.Flush(t.Context(), f.pool, deferred)

	require.ErrorIs(t, err, applier.ErrBatchDeferred)
	slot, _ := f.text(t, p.shadow.ShadowTable(), "slot", 1)
	assert.Equal(t, "A", *slot, "the shadow is as it was")

	buffer := applier.NewBuffer()
	require.NoError(t, buffer.Requeue(deferred))
	assert.Equal(t, 1, buffer.Len())
	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), col("doc", *doc1)),
		image(2, col("slot", nil), col("doc", *doc2)),
	))
	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Fallback: true}, result)
	f.assertConverged(t, p.shadow)
}

// The fallback needs every image whole, so a surviving image that still
// carries a marker after completion from its own row — a row the shadow
// does not hold — is refused, and the flush commits nothing (CO-8).
func TestFlushFallbackRefusesAnImageItCannotComplete(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 2)
	p := f.prepare(t, "seats")
	doc2, _ := f.text(t, "seats", "doc", 2)

	_, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), col("doc", "new document")),
		image(2, col("slot", "A"), col("doc", *doc2)),
		image(50, col("slot", "C"), marker("doc")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(CO-8): flush key 50")
	slot, _ := f.text(t, p.shadow.ShadowTable(), "slot", 1)
	assert.Equal(t, "A", *slot, "the fallback's deletes were rolled back with the rest")
	assert.Equal(t, int64(2), f.count(t, p.shadow.ShadowTable()))
}
