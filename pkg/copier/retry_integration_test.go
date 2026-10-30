package copier_test

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// prepareSeats creates, proves, locks, and builds the shadow of a seats
// table whose slot column is unique, holding rows rows with slot = id. The
// shadow keeps the unique index on slot, so a shadow row holding a slot the
// source assigns to another key makes the chunk carrying that key collide.
func (f copierFixture) prepareSeats(t *testing.T, rows int64) (preflight.CopySwapTarget, *dbconn.TableLockSession, schemachange.BuiltShadow) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.seats (
			id bigint PRIMARY KEY,
			slot integer NOT NULL UNIQUE,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.seats (id, slot, note)
		SELECT n, n, 'seat ' || n FROM generate_series(1, %d) AS n`, rows))
	target := f.prove(t, "seats")
	lock := f.lock(t, "seats")
	return target, lock, f.build(t, lock, target)
}

// holdSlotAtFirstChunk returns a Clock that, at the copier's first reading
// — after the resume clear, before the first chunk's transaction — commits a
// shadow row under key holding slot, standing in for an applier write that
// moved the slot there from a row the first chunk is about to read. The
// returned check fails the test if the row could not be placed.
func (f copierFixture) holdSlotAtFirstChunk(t *testing.T, shadow schemachange.BuiltShadow, key, slot int64) (*hookedClock, func(t *testing.T)) {
	t.Helper()
	var placeErr error
	clock := &hookedClock{hook: func() {
		if _, err := f.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()+" (id, slot) VALUES ($1, $2)", key, slot); err != nil {
			placeErr = fmt.Errorf("hold slot %d under key %d: %w", slot, key, err)
		}
	}}
	return clock, func(t *testing.T) {
		t.Helper()
		require.NoError(t, placeErr)
	}
}

// A chunk whose insert collides with a shadow row on the unique slot index
// commits around the colliding row and lands the rest once that row is
// gone: here a shadow row under another key holds slot 15, as it would
// after a source change moved the slot from the row the copier read to a
// landed key the applier has yet to flush the move's other half for (CO-4).
// After the first collision the shadow holds every row of the chunk but
// key 15, the chunk stays in flight with key 15 pending — so a captured
// change for key 15 still waits while one for key 16 applies — and the pass
// after the row's removal copies key 15 alone, so every row lands and each
// row counts as inserted once.
func TestCopierCommitsAroundACollisionAndLandsTheRestWhenTheRowIsGone(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepareSeats(t, rows)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	clock, placed := f.holdSlotAtFirstChunk(t, shadow, 2000, 15)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:          1,
		Chunker:          copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionBackoff: 20 * time.Millisecond,
		Clock:            clock,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	const settleDeadline = 15 * time.Second
	require.Eventually(t, func() bool { return len(c.Position().Pending) == 1 }, settleDeadline, 10*time.Millisecond,
		"the first chunk, carrying key 15, should collide on the slot the shadow row holds and commit around it")
	placed(t)
	pos := c.Position()
	require.Len(t, pos.InFlight, 1, "the colliding chunk stays in flight while key 15 is pending")
	first := pos.InFlight[0]
	assert.Equal(t, int64(math.MinInt64), first.Lower())
	assert.Equal(t, []copier.PendingKeys{{Chunk: first, Keys: []int64{15}}}, pos.Pending)
	assert.Equal(t, copier.KeyInFlight, pos.Classify(15), "the held-off key still waits")
	assert.Equal(t, copier.KeyLanded, pos.Classify(16), "the rest of the chunk has landed")
	assert.False(t, pos.Watermark.Valid(), "the watermark waits for the pending key")
	assert.Equal(t, first.Rows()-1, f.count(t, shadow.ShadowTable(), fmt.Sprintf("id <= %d", first.Upper())), "every row of the chunk but key 15 is in the shadow")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id = 15"))
	f.exec(t, "DELETE FROM "+shadowName+" WHERE id = 2000")

	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, results, stopDeadline, "copy after the colliding row's removal"))
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(rows), c.Position().RowsInserted, "each row counts as inserted once across the passes")
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
	assert.Empty(t, c.Position().Pending)
	assert.GreaterOrEqual(t, c.Collisions(), int64(2), "the strict insert and the pass that held key 15 off each count")
}

// A collision no pass outlasts stops the copy with the server's own
// refusal: the error is the unique violation wrapped, not a refusal class of
// the copier's and not a transient dbconn would retry, every pass is
// counted, and the chunk stays in flight with its pending key, so a caller
// that checkpoints Position records no key of it as copied and a resumed
// run re-copies the chunk; the rows committed around the pending key stay
// in the shadow, where the resume clear removes them.
func TestCopierStopsWhenACollisionOutlastsEveryAttempt(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepareSeats(t, 300)
	clock, placed := f.holdSlotAtFirstChunk(t, shadow, 2000, 15)

	const attempts = 3
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:           1,
		Chunker:           copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionAttempts: attempts,
		CollisionBackoff:  10 * time.Millisecond,
		Clock:             clock,
	})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	placed(t)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code, "the server's unique violation is the copy's error")
	assert.Equal(t, copier.RefusalCause(""), copier.RefusalCauseOf(err), "a collision is the server's answer, not a copier refusal")
	assert.False(t, errors.Is(err, copier.ErrInvariantViolation))
	assert.False(t, dbconn.Retryable(err), "a collision is not a transient the connection layer retries")
	assert.Contains(t, err.Error(), fmt.Sprintf("on each of %d attempts", attempts))
	assert.Equal(t, int64(attempts), c.Collisions())

	pos := c.Position()
	require.Len(t, pos.InFlight, 1, "the exhausted chunk never landed")
	first := pos.InFlight[0]
	assert.Equal(t, int64(math.MinInt64), first.Lower())
	assert.Equal(t, []copier.PendingKeys{{Chunk: first, Keys: []int64{15}}}, pos.Pending, "the held-off key is still pending")
	assert.False(t, pos.Watermark.Valid(), "nothing landed")
	assert.Equal(t, first.Rows()-1, pos.RowsInserted, "the rows committed around the pending key count as inserted")
	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "id = 15"), "the pending key's row never reached the shadow")
	assert.Equal(t, first.Rows()-1, f.count(t, shadow.ShadowTable(), "id <= 300"), "the rest of the chunk stays in the shadow for the resume clear to remove")
}
