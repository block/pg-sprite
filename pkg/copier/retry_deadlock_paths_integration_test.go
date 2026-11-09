package copier_test

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The tests below need a pass to be rolled back with deadlock_detected on
// demand, on paths where arranging a real two-writer cycle would take a
// writer per pass. A trigger on the shadow raises the server's own
// SQLSTATE 40P01 for every chunk row while a row sits in the gate table, so
// the pass fails exactly as a deadlock victim's does and commits nothing;
// the real cycle is covered by the tests in retry_deadlock_integration_test.go.
func (f copierFixture) deadlockWhileGated(t *testing.T, shadow schemachange.BuiltShadow) (gate func(), ungate func()) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.deadlock_gate (
			on_ boolean
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.raise_deadlock_while_gated() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id < 2000 AND EXISTS (SELECT 1 FROM %s.deadlock_gate) THEN
				RAISE EXCEPTION 'deadlock stand-in' USING ERRCODE = '40P01';
			END IF;
			RETURN NEW;
		END $$`)
	f.exec(t, `
		CREATE TRIGGER raise_deadlock_while_gated
			BEFORE INSERT ON %s.`+pgx.Identifier{shadow.ShadowTable()}.Sanitize()+`
			FOR EACH ROW EXECUTE FUNCTION %s.raise_deadlock_while_gated()`)
	return func() { f.exec(t, `INSERT INTO %s.deadlock_gate VALUES (true)`) },
		func() { f.exec(t, `DELETE FROM %s.deadlock_gate`) }
}

// A shadow with a DEFERRABLE unique constraint cannot be copied around, so
// its chunk reruns the strict insert over the whole range; a rerun the
// server rolls back to break a deadlock takes the next pass there too. The
// strict insert collides on slot 15, which a shadow row under key 2000
// holds; the pass that would copy around it is refused for the DEFERRABLE
// constraint (SQLSTATE 55000), which the server checks after BEFORE
// triggers, so the gate stays shut until the chunk is rerunning: the first
// rerun collides again, and only then does the gate open, so every rerun
// after it deadlocks on its first row. Once three have deadlocked, the gate
// shuts and the row holding slot 15 goes, and the next rerun lands the chunk.
func TestCopierRetriesADeadlockedRerunOnADeferrableShadow(t *testing.T) {
	f := newCopierFixture(t)
	f.exec(t, `
		CREATE TABLE %s.seats (
			id bigint PRIMARY KEY,
			slot integer NOT NULL,
			note text,
			CONSTRAINT seats_slot_key UNIQUE (slot) DEFERRABLE INITIALLY IMMEDIATE
		)`)
	f.exec(t, `
		INSERT INTO %s.seats (id, slot, note)
		SELECT n, n, 'seat ' || n FROM generate_series(1, 300) AS n`)
	target := f.prove(t, "seats")
	lock := f.lock(t, "seats")
	shadow := f.build(t, lock, target)
	gate, ungate := f.deadlockWhileGated(t, shadow)
	clock, placed := f.holdSlotAtFirstChunk(t, shadow, 2000, 15)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:           1,
		Chunker:           copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionAttempts: 12,
		CollisionBackoff:  50 * time.Millisecond,
		Clock:             clock,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	// The strict insert and the first rerun each count one collision; the
	// refused copy-around pass between them counts nothing.
	awaitCollisions(t, c, results, 2, "the strict insert and the first rerun should collide on slot 15")
	placed(t)
	gate()
	// Each deadlocked rerun counts one more.
	awaitCollisions(t, c, results, 5, "the reruns after the gate opened should deadlock and take further passes")
	ungate()
	f.exec(t, "DELETE FROM %s."+pgx.Identifier{shadow.ShadowTable()}.Sanitize()+" WHERE id = 2000")

	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, results, stopDeadline, "copy after the gate shut and slot 15 was freed"))
	f.assertConverged(t, shadow)
	assert.Equal(t, int64(300), c.Position().RowsInserted)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}

// awaitCollisions waits until c has counted at least n collisions, and
// fails the test at once if the copy stops first.
func awaitCollisions(t *testing.T, c *copier.Copier, results <-chan error, n int64, what string) {
	t.Helper()
	const collisionsDeadline = 15 * time.Second
	deadline := time.Now().Add(collisionsDeadline)
	for c.Collisions() < n {
		select {
		case err := <-results:
			t.Fatalf("the copy stopped at %d collisions instead of taking the next pass: %v", c.Collisions(), err)
		case <-time.After(5 * time.Millisecond):
		}
		require.True(t, time.Now().Before(deadline), what)
	}
}

// Deadlocks spend the same bounded budget as collisions, and a chunk that
// runs out on them reports the deadlock that ended it. The strict insert
// collides on slot 15, which a shadow row under key 2000 holds; the pass
// after it commits around key 15 and leaves it pending; then the gate
// opens and every later pass over key 15 deadlocks. The copy stops after
// exactly CollisionAttempts passes, with key 15 still pending and the
// chunk in flight, and the error it returns wraps the last pass's 40P01,
// not the first pass's 23505.
func TestCopierStopsWhenDeadlocksOutlastEveryAttempt(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepareSeats(t, 300)
	gate, _ := f.deadlockWhileGated(t, shadow)
	clock, placed := f.holdSlotAtFirstChunk(t, shadow, 2000, 15)

	const attempts = 5
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:           1,
		Chunker:           copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionAttempts: attempts,
		CollisionBackoff:  200 * time.Millisecond,
		Clock:             clock,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)

	// Pass 3 waits one backoff after pass 2 settles key 15 as pending.
	const pendingDeadline = 15 * time.Second
	require.Eventually(t, func() bool { return len(c.Position().Pending) == 1 }, pendingDeadline, 5*time.Millisecond,
		"the strict insert should collide on slot 15 and the next pass commit around key 15")
	placed(t)
	gate()

	const stopDeadline = 30 * time.Second
	err = awaitRun(t, results, stopDeadline, "copy whose later passes all deadlock")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "the copy should stop with the server's error: %v", err)
	assert.Equal(t, "40P01", pgErr.Code, "the error names the deadlock that ended the run, not the first collision")
	assert.Equal(t, int64(attempts), c.Collisions(), "one per pass: the collision, the pass that held key 15 off, and three deadlocks")
	pos := c.Position()
	require.Len(t, pos.InFlight, 1, "the exhausted chunk never landed")
	assert.Equal(t, []copier.PendingKeys{{Chunk: pos.InFlight[0], Keys: []int64{15}}}, pos.Pending)
}
