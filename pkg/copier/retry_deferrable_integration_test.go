package copier_test

import (
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// A shadow whose unique constraint is DEFERRABLE still converges when a
// chunk collides. The server refuses ON CONFLICT with no conflict target on
// such a table (SQLSTATE 55000), so the chunk cannot be committed around
// the colliding row; it reruns the strict insert over its whole range
// instead, and lands once the row holding slot 15 is gone. Nothing is
// pending meanwhile: the whole chunk stays in flight.
func TestCopierConvergesWhenTheUniqueConstraintIsDeferrable(t *testing.T) {
	f := newCopierFixture(t)
	f.exec(t, `
		CREATE TABLE %s.seats (
			id bigint PRIMARY KEY,
			slot integer NOT NULL,
			note text,
			CONSTRAINT seats_slot_key UNIQUE (slot) DEFERRABLE INITIALLY DEFERRED
		)`)
	f.exec(t, `
		INSERT INTO %s.seats (id, slot, note)
		SELECT n, n, 'seat ' || n FROM generate_series(1, 300) AS n`)
	target := f.prove(t, "seats")
	lock := f.lock(t, "seats")
	shadow := f.build(t, lock, target)
	shadowName := pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
	clock, placed := f.holdSlotAtFirstChunk(t, shadow, 2000, 15)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:           1,
		Chunker:           copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionAttempts: 12,
		CollisionBackoff:  20 * time.Millisecond,
		Clock:             clock,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	var early error
	ended := false
	const collideDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		select {
		case early = <-results:
			ended = true
			return true
		default:
			return c.Collisions() >= 3
		}
	}, collideDeadline, 5*time.Millisecond)
	require.False(t, ended, "the copy ended while the colliding row was still held: %v", early)
	placed(t)
	assert.Empty(t, c.Position().Pending, "a chunk that cannot be copied around holds no keys pending")
	assert.Equal(t, copier.KeyInFlight, c.Position().Classify(16), "the whole chunk stays in flight")
	f.exec(t, "DELETE FROM "+shadowName+" WHERE id = 2000")

	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, results, stopDeadline, "copy after the colliding row's removal"))
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(300), c.Position().RowsInserted)
}
