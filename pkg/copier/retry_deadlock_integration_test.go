package copier_test

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The applier's flush and the copier's workers all write the shadow, and
// while source changes move unique values between keys, two of them can
// each hold a value the other is inserting: each waits on the other's
// transaction, and the server breaks the cycle by rolling one back with
// deadlock_detected. A chunk pass rolled back that way committed nothing,
// so the chunk is given another pass, as a collision is, rather than
// ending the copy.
//
// In both tests the other writer is an open transaction standing in for a
// flush. Before the first chunk starts it holds slot 20 under key 2001, so
// a pass that has inserted slot 10 for key 10 waits on it at key 20; the
// writer then inserts slot 10 under key 2000 and waits on the pass. The
// writer's deadlock check is put off for a minute, so the pass's check,
// at the server's default deadlock_timeout, finds the cycle and the server
// rolls the pass back. The writer commits, the next pass commits the chunk
// around the slots the writer holds, and once the writer's rows are gone,
// as a flush would rewrite them, every row lands.

// The strict insert is the pass that deadlocks.
func TestCopierRetriesAStrictInsertThatDeadlocksWithAnotherShadowWriter(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepareSeats(t, rows)
	writer := f.shadowWriterAtFirstChunk(t, shadow)
	run := f.startDeadlockCopy(t, target, lock, shadow, writer.clock)

	writer.deadlockWithTheChunk(t)

	run.awaitPending(t, []int64{10, 20})
	f.exec(t, "DELETE FROM "+writer.shadowName+" WHERE id IN (2000, 2001)")
	run.awaitLanded(t, rows)
	assert.GreaterOrEqual(t, run.c.Collisions(), int64(2), "the deadlocked pass and the pass that held keys 10 and 20 off each count")
}

// The strict insert collides with a committed row holding slot 15 under key
// 2002, so the chunk moves on to the pass that copies around held-off rows;
// that pass is the one that deadlocks, and the pass after it holds off keys
// 10, 15, and 20.
func TestCopierRetriesAPassAroundHeldOffRowsThatDeadlocksWithAnotherShadowWriter(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepareSeats(t, rows)
	writer := f.shadowWriterAtFirstChunk(t, shadow)
	writer.committedFirst = "INSERT INTO " + writer.shadowName + " (id, slot) VALUES (2002, 15)"
	run := f.startDeadlockCopy(t, target, lock, shadow, writer.clock)

	writer.deadlockWithTheChunk(t)

	run.awaitPending(t, []int64{10, 15, 20})
	f.exec(t, "DELETE FROM "+writer.shadowName+" WHERE id IN (2000, 2001, 2002)")
	run.awaitLanded(t, rows)
	assert.GreaterOrEqual(t, run.c.Collisions(), int64(3), "the collided strict insert, the deadlocked pass, and the pass that held keys off each count")
}

// shadowWriter is a transaction writing the shadow beside the copier.
type shadowWriter struct {
	// watch is a connection of its own for seeing the chunk pass wait,
	// acquired before the copy starts so the check never queues behind the
	// copier for one of the pool's.
	watch      *pgxpool.Conn
	shadowName string
	// committedFirst, when set, is a statement committed on its own before
	// the writer's transaction begins.
	committedFirst string
	clock          *hookedClock
	// begun is closed once tx, pid, and err are set.
	begun chan struct{}
	tx    pgx.Tx
	pid   int
	err   error
}

// shadowWriterAtFirstChunk returns a writer whose clock, at the copier's
// first reading — before the first chunk's transaction — opens the writer's
// transaction and inserts slot 20 under key 2001 without committing.
func (f copierFixture) shadowWriterAtFirstChunk(t *testing.T, shadow schemachange.BuiltShadow) *shadowWriter {
	t.Helper()
	watch, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(watch.Release)
	w := &shadowWriter{
		watch:      watch,
		shadowName: pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize(),
		begun:      make(chan struct{}),
	}
	w.clock = &hookedClock{hook: func() {
		defer close(w.begun)
		// The hook runs on a copier worker; failures are reported through
		// err and checked on the test goroutine.
		if w.committedFirst != "" {
			if _, err := f.pool.Exec(t.Context(), w.committedFirst); err != nil {
				w.err = fmt.Errorf("commit %q: %w", w.committedFirst, err)
				return
			}
		}
		tx, err := f.pool.Begin(t.Context())
		if err != nil {
			w.err = fmt.Errorf("begin the other writer: %w", err)
			return
		}
		w.tx = tx
		// The server checks a wait for a deadlock once, deadlock_timeout
		// after it starts, and rolls back the waiter whose check finds the
		// cycle. The writer's check is put off well past the chunk pass's,
		// which keeps the server default, so the pass is always the one
		// rolled back however the two waits' start times fall.
		if _, err := tx.Exec(t.Context(), "SET LOCAL deadlock_timeout = '1min'"); err != nil {
			w.err = fmt.Errorf("put off the other writer's deadlock check: %w", err)
			return
		}
		if err := tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&w.pid); err != nil {
			w.err = fmt.Errorf("read the other writer's pid: %w", err)
			return
		}
		if _, err := tx.Exec(t.Context(), "INSERT INTO "+w.shadowName+" (id, slot) VALUES (2001, 20)"); err != nil {
			w.err = fmt.Errorf("hold slot 20 under key 2001: %w", err)
		}
	}}
	t.Cleanup(func() {
		select {
		case <-w.begun:
			if w.tx != nil {
				// Redundant safety closer: after the commit in
				// deadlockWithTheChunk this returns the guaranteed
				// ErrTxClosed.
				_ = w.tx.Rollback(context.WithoutCancel(t.Context()))
			}
		default:
		}
	})
	return w
}

// deadlockWithTheChunk waits until a chunk pass is blocked on the writer's
// slot 20, inserts slot 10 under key 2000, which the pass already holds,
// and commits once the server has rolled the pass back.
func (w *shadowWriter) deadlockWithTheChunk(t *testing.T) {
	t.Helper()
	const begunDeadline = 15 * time.Second
	select {
	case <-w.begun:
	case <-time.After(begunDeadline):
		t.Fatalf("the copier did not reach its first chunk within %s", begunDeadline)
	}
	require.NoError(t, w.err)

	const blockedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		var blocked bool
		err := w.watch.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_stat_activity
				WHERE $1 = ANY (pg_catalog.pg_blocking_pids(pid))
			)`, w.pid).Scan(&blocked)
		return err == nil && blocked
	}, blockedDeadline, 10*time.Millisecond, "a chunk pass should wait on slot 20, which the other writer holds")

	_, err := w.tx.Exec(t.Context(), "INSERT INTO "+w.shadowName+" (id, slot) VALUES (2000, 10)")
	require.NoError(t, err, "the server should roll the chunk pass back, not the writer that began waiting later")
	require.NoError(t, w.tx.Commit(t.Context()))
}

// deadlockCopy is a copier run over the seats table, one worker and one
// 100-row first chunk, so keys 10, 15, and 20 share a chunk.
type deadlockCopy struct {
	f       copierFixture
	shadow  schemachange.BuiltShadow
	c       *copier.Copier
	results chan error
}

func (f copierFixture) startDeadlockCopy(t *testing.T, target preflight.CopySwapTarget, lock *dbconn.TableLockSession, shadow schemachange.BuiltShadow, clock *hookedClock) deadlockCopy {
	t.Helper()
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:          1,
		Chunker:          copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		CollisionBackoff: 20 * time.Millisecond,
		Clock:            clock,
	})
	require.NoError(t, err)
	run := deadlockCopy{f: f, shadow: shadow, c: c, results: make(chan error, 1)}
	var wg sync.WaitGroup
	wg.Go(func() { run.results <- c.Run(t.Context(), f.pool) })
	t.Cleanup(wg.Wait)
	return run
}

// awaitPending waits until the first chunk has committed around keys, and
// fails the test at once if the copy stops instead.
func (r deadlockCopy) awaitPending(t *testing.T, keys []int64) {
	t.Helper()
	const pendingDeadline = 15 * time.Second
	deadline := time.Now().Add(pendingDeadline)
	for len(r.c.Position().Pending) == 0 {
		select {
		case err := <-r.results:
			t.Fatalf("the copy stopped instead of giving the deadlocked chunk another pass: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
		require.True(t, time.Now().Before(deadline), "the chunk should commit around the slots the writer holds")
	}
	pos := r.c.Position()
	require.Len(t, pos.InFlight, 1)
	assert.Equal(t, []copier.PendingKeys{{Chunk: pos.InFlight[0], Keys: keys}}, pos.Pending)
}

// awaitLanded waits for the copy to finish and checks every row landed once.
func (r deadlockCopy) awaitLanded(t *testing.T, rows int64) {
	t.Helper()
	const stopDeadline = 30 * time.Second
	require.NoError(t, awaitRun(t, r.results, stopDeadline, "copy after the other writer's rows are gone"))
	r.f.assertConverged(t, r.shadow)
	assert.Equal(t, rows, r.c.Position().RowsInserted, "each row counts as inserted once across the passes")
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), r.c.Position().Watermark)
}
