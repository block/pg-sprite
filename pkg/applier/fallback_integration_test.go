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

// The same exchange when both updated rows left their out-of-line document
// unchanged, so both images carry a marker. The fallback needs every image
// whole: it completes each from the shadow row under its own key before its
// deletes, and both documents survive byte for byte (CO-8, D13).
func TestFlushFallbackCompletesMarkersFromTheShadow(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 2)
	p := f.prepare(t, "seats")
	doc1, _ := f.text(t, "seats", "doc", 1)
	doc2, _ := f.text(t, "seats", "doc", 2)
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 1`)
	f.exec(t, `UPDATE %s.seats SET slot = 'A' WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), marker("doc")),
		image(2, col("slot", "A"), marker("doc")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Completed: 2, Fallback: true}, result)
	f.assertConverged(t, p.shadow)
	got1, _ := f.text(t, p.shadow.ShadowTable(), "doc", 1)
	got2, _ := f.text(t, p.shadow.ShadowTable(), "doc", 2)
	assert.Equal(t, *doc1, *got1, "completed from the row under its own key before that row was deleted")
	assert.Equal(t, *doc2, *got2)
}

// A row that moves to a lower key and keeps its unique slot is not an
// exchange: the only row holding the slot is its own, under the old key.
// The flush writes the batch's delete markers before its images, so the
// upsert at the new key finds the slot free and the batch stays column-wise
// (CO-6).
func TestFlushMoveToALowerKeyStaysColumnWise(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	doc3, _ := f.text(t, "seats", "doc", 3)
	f.exec(t, `UPDATE %s.seats SET id = 0 WHERE id = 3`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		moved(3, 0, col("slot", "C"), col("doc", *doc3)),
		deleted(3),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1, Deletes: 1}, result, "only the row's own old key held its slot")
	f.assertConverged(t, p.shadow)
}

// An exclusion constraint refuses an exchange exactly as a unique index
// does, with its own SQLSTATE; the flush takes the same fallback and the
// batch converges (CO-6). The constraint is a btree equality exclusion,
// which preflight admits.
func TestFlushConvergesAnExclusionExchange(t *testing.T) {
	f := newFlushFixture(t)
	f.exec(t, `
		CREATE TABLE %s.seats (
			id bigint PRIMARY KEY,
			slot text,
			doc text,
			note text,
			EXCLUDE USING btree (slot WITH =)
		)`)
	f.exec(t, `INSERT INTO %s.seats VALUES (1, 'A', 'd1', 'n1'), (2, 'B', 'd2', 'n2')`)
	p := f.prepare(t, "seats")
	f.exec(t, `UPDATE %s.seats SET slot = NULL WHERE id = 1`)
	f.exec(t, `UPDATE %s.seats SET slot = 'A' WHERE id = 2`)
	f.exec(t, `UPDATE %s.seats SET slot = 'B' WHERE id = 1`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(1, col("slot", "B"), col("doc", "d1")),
		image(2, col("slot", "A"), col("doc", "d2")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Fallback: true}, result)
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
