package checksum_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/decode"
)

// The live repairs below exercise what a recopy under the stream has to
// get right beyond the recopy itself: that the repaired chunk is read
// again and judged, and that the stream's flushes are fenced out of the
// shadow while the recopy runs.

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
