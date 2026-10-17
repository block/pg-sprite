package checksum_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/decode"
)

// The live repairs below exercise what a recopy under the stream has to
// get right beyond the recopy itself: that the repaired chunk is read
// again and judged, that the catch-up's flushes are held off the shadow
// in Go for as long as the recopy runs, and that every other writer is
// fenced out of it with SHARE MODE.

// A repair whose chunk is a confirmed divergence again after the recopy is
// a RepairError (CO-2), not a clean pass: the recopy committed, so it is
// listed, and nothing is minted. The shadow is written behind the stream's
// back once the recopy has put the first corrupted row back, so the reread
// finds a second row wrong that neither side moves.
func TestCheckLiveRefusesARepairThatDoesNotTake(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1400")

	var corrupted atomic.Bool
	corrupt := confirmedFunc(func() decode.LSN {
		var repaired bool
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT amount <> 0 FROM "+f.shadowName()+" WHERE id = 1400").Scan(&repaired))
		if repaired && corrupted.CompareAndSwap(false, true) {
			f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1500")
		}
		return f.catchup.Confirmed()
	})

	v := f.verifier(t, checksum.Options{})
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{Confirmed: corrupt})
	var failed *checksum.RepairError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, chunk(t, 1001, 2000), failed.Repair.Mismatch.Chunk)
	assert.Equal(t, chunk(t, 1001, 2000), failed.After.Chunk)
	require.Len(t, outcome.Repairs, 1, "the recopy committed before the reread found the chunk wrong again")
	assertNoProofs(t, outcome)
}

// A live repair takes SHARE MODE on the shadow before it deletes anything,
// so it waits behind a writer that holds a row lock on the shadow instead
// of interleaving with it. A transaction writes a shadow row and stays
// open; the pass is seen waiting for the lock in pg_locks, and completes
// once the writer rolls back. The verifier's lock wait outlasts the
// writer, so the fence is not refused before the test releases it.
func TestCheckLiveRepairWaitsBehindAWriterOnTheShadow(t *testing.T) {
	f := newLiveFixture(t)

	writer, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the test rolls the writer back itself.
		_ = writer.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = writer.Exec(t.Context(), "UPDATE "+f.shadowName()+" SET amount = amount WHERE id = 5")
	require.NoError(t, err)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1400")

	v := f.verifier(t, checksum.Options{LockTimeout: heldFlushLockTimeout})
	done := make(chan error, 1)
	go func() {
		_, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{})
		done <- err
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		require.NoError(t, f.pool.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM pg_locks WHERE mode = 'ShareLock' AND NOT granted AND relation = $1::regclass)",
			f.shadowName()).Scan(&waiting))
		return waiting
	}, 10*time.Second, 20*time.Millisecond, "the repair asked for SHARE MODE on the shadow and is waiting behind the writer")
	require.NoError(t, writer.Rollback(t.Context()))

	const passDeadline = 30 * time.Second
	select {
	case err := <-done:
		require.NoError(t, err, "the repair ran once the writer let go")
	case <-time.After(passDeadline):
		t.Fatalf("the live pass did not return within %s of the writer rolling back", passDeadline)
	}
	f.assertConverged(t)
}

// writingGate is the fixture's catch-up as a FlushGate that writes the
// source once the flushes are held, so a change the stream carries arrives
// while the repair's recopy is open, and counts the holds and resumes.
type writingGate struct {
	f       *liveFixture
	write   string
	paused  atomic.Int32
	resumed atomic.Int32
}

func (g *writingGate) Pause(ctx context.Context) (func(), error) {
	resume, err := g.f.catchup.Pause(ctx)
	if err != nil {
		return nil, err
	}
	g.paused.Add(1)
	if _, err := g.f.pool.Exec(ctx, g.write); err != nil {
		resume()
		return nil, err
	}
	return func() {
		g.resumed.Add(1)
		resume()
	}, nil
}

// A live repair holds the catch-up's flushes in Go for as long as its
// recopy runs, so a flush never waits behind the recopy's lock on the
// shadow, however long the recopy takes. The flusher's lock wait is one
// millisecond: a flush that reached the shadow while the recopy held it
// would be refused and end the catch-up. A transaction holds a row lock on
// a row in the divergent chunk, so the recopy's delete waits behind it
// with the recopy open; a source write committed under the hold is
// buffered and not applied while the recopy waits; the row lock is
// released, the recopy commits, the flushes resume and apply the write,
// and the pass settles the chunk clean with the catch-up still running.
func TestCheckLiveRepairHoldsTheCatchupsFlushesForTheRecopy(t *testing.T) {
	f := newLiveFixtureWith(t, liveSetup{flushLockTimeout: time.Millisecond})
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1400")

	rowLock, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the test rolls the row lock back itself.
		_ = rowLock.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = rowLock.Exec(t.Context(), "SELECT id FROM "+f.shadowName()+" WHERE id = 1500 FOR UPDATE")
	require.NoError(t, err)

	gate := &writingGate{f: f, write: "UPDATE " + f.table.Qualified() + " SET amount = amount + 1 WHERE id = 5"}
	v := f.verifier(t, checksum.Options{LockTimeout: heldFlushLockTimeout})
	type passResult struct {
		outcome checksum.Outcome
		err     error
	}
	done := make(chan passResult, 1)
	go func() {
		outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{Flushes: gate})
		done <- passResult{outcome, err}
	}()

	// A tuple lock on the shadow exists only while a transaction waits for
	// one of its rows: the recopy's delete, behind the row lock.
	const recopyWaitDeadline = 10 * time.Second
	var held applier.Status
	require.Eventually(t, func() bool {
		var recopyWaiting bool
		require.NoError(t, f.pool.QueryRow(t.Context(),
			"SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'tuple' AND relation = $1::regclass)",
			f.shadowName()).Scan(&recopyWaiting))
		held = f.catchup.Status()
		return recopyWaiting && held.Buffered >= 1
	}, recopyWaitDeadline, 20*time.Millisecond, "the recopy waits behind the row lock with the source write buffered")
	assert.Zero(t, held.Applied, "the buffered write is not flushed while the recopy is open")
	assert.Equal(t, int32(1), gate.paused.Load(), "the pass held the flushes once, before the recopy")
	assert.Zero(t, gate.resumed.Load(), "the flushes stay held while the recopy is open")
	select {
	case err := <-f.done:
		t.Fatalf("the catch-up ended while the recopy was open: %v", err)
	default:
	}

	require.NoError(t, rowLock.Rollback(t.Context()))
	const passDeadline = 30 * time.Second
	var pass passResult
	select {
	case pass = <-done:
	case <-time.After(passDeadline):
		t.Fatalf("the live pass did not return within %s of the row lock being released", passDeadline)
	}
	require.NoError(t, pass.err, "the recopy ran once the row lock let go and the chunk settled clean")
	require.Len(t, pass.outcome.Repairs, 1)
	assert.Equal(t, chunk(t, 1001, 2000), pass.outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, int32(1), gate.resumed.Load(), "the pass resumed the flushes once the recopy committed")
	after := f.catchup.Status()
	assert.GreaterOrEqual(t, after.Applied, uint64(1), "the write buffered under the hold was flushed after the resume")

	// The catch-up outlived the pass: a write after it is applied too.
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 7")
	end := f.currentWALLSN(t)
	const confirmDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		return f.catchup.Status().Confirmed >= end
	}, confirmDeadline, 20*time.Millisecond, "the catch-up confirmed past a write after the pass")
	f.assertConverged(t)
}

// countingGate is the fixture's catch-up as a FlushGate that counts the
// holds and resumes.
type countingGate struct {
	f       *liveFixture
	paused  atomic.Int32
	resumed atomic.Int32
}

func (g *countingGate) Pause(ctx context.Context) (func(), error) {
	resume, err := g.f.catchup.Pause(ctx)
	if err != nil {
		return nil, err
	}
	g.paused.Add(1)
	return func() {
		g.resumed.Add(1)
		resume()
	}, nil
}

// A live repair whose recopy fails before it commits gives the flushes
// back: a transaction writes a shadow row and stays open, so the recopy's
// SHARE MODE wait outlasts the verifier's lock wait and the pass returns
// its error. The hold was taken once and resumed once, the catch-up is
// not paused, and a write made after the failed repair is flushed and
// confirmed. A resume on the success path alone would leave the catch-up
// buffering under a hold nobody gives back.
func TestCheckLiveRepairResumesTheFlushesWhenTheRecopyFails(t *testing.T) {
	f := newLiveFixture(t)

	writer, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		// Redundant safety closer: the test rolls the writer back itself.
		_ = writer.Rollback(context.WithoutCancel(t.Context()))
	})
	_, err = writer.Exec(t.Context(), "UPDATE "+f.shadowName()+" SET amount = amount WHERE id = 5")
	require.NoError(t, err)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1400")

	const recopyLockWait = 500 * time.Millisecond
	v := f.verifier(t, checksum.Options{LockTimeout: recopyLockWait})
	gate := &countingGate{f: f}
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{Flushes: gate})
	require.Error(t, err, "the recopy's SHARE MODE wait ran out behind the writer")
	assert.Empty(t, outcome.Repairs, "the recopy did not commit")
	assertNoProofs(t, outcome)
	assert.Equal(t, int32(1), gate.paused.Load())
	assert.Equal(t, int32(1), gate.resumed.Load(), "a recopy that failed gives the flushes back")
	assert.False(t, f.catchup.Status().FlushesHeld)

	require.NoError(t, writer.Rollback(t.Context()))
	before := f.catchup.Status().Applied
	f.exec(t, "UPDATE "+f.table.Qualified()+" SET amount = amount + 1 WHERE id = 7")
	end := f.currentWALLSN(t)
	const confirmDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		s := f.catchup.Status()
		return s.Confirmed >= end && s.Applied > before
	}, confirmDeadline, 20*time.Millisecond, "the catch-up flushes a write made after the failed repair")
}

// bracketGate is the fixture's catch-up as a FlushGate that records, from
// a second connection, whether the repair's SHARE MODE lock on the shadow
// is granted at the moment of the hold and at the moment of the resume.
// The pass calls Pause and the resume on the test's goroutine.
type bracketGate struct {
	t             *testing.T
	f             *liveFixture
	pauses        int
	resumes       int
	shareAtPause  bool
	shareAtResume bool
}

func (g *bracketGate) shareGranted(ctx context.Context) bool {
	var held bool
	require.NoError(g.t, g.f.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_locks WHERE mode = 'ShareLock' AND granted AND relation = $1::regclass)",
		g.f.shadowName()).Scan(&held))
	return held
}

func (g *bracketGate) Pause(ctx context.Context) (func(), error) {
	resume, err := g.f.catchup.Pause(ctx)
	if err != nil {
		return nil, err
	}
	g.pauses++
	g.shareAtPause = g.shareGranted(ctx)
	return func() {
		g.resumes++
		g.shareAtResume = g.shareGranted(context.WithoutCancel(ctx))
		resume()
	}, nil
}

// The hold brackets the recopy transaction: it is taken before the
// transaction asks for SHARE MODE on the shadow, since a flush that
// started after the lock was granted would wait on it with the gate held
// and the hold behind that flush, and it is given back only after the
// transaction has committed and let the lock go, since a flush resumed
// earlier would wait on the lock under its own lock_timeout.
func TestCheckLiveRepairHoldBracketsTheRecopyTransaction(t *testing.T) {
	f := newLiveFixture(t)
	f.exec(t, "UPDATE "+f.shadowName()+" SET amount = 0 WHERE id = 1400")

	v := f.verifier(t, checksum.Options{LockTimeout: heldFlushLockTimeout})
	gate := &bracketGate{t: t, f: f}
	outcome, err := f.checkLive(t, v, checksum.DivergenceRepair, checksum.LiveOptions{Flushes: gate})
	require.NoError(t, err)
	require.Len(t, outcome.Repairs, 1)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, 1, gate.pauses)
	assert.Equal(t, 1, gate.resumes)
	assert.False(t, gate.shareAtPause, "the hold is taken before the recopy transaction holds its lock")
	assert.False(t, gate.shareAtResume, "the flushes resume only after the recopy transaction has let go of its lock")
	f.assertConverged(t)
}
