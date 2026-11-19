package applier_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
)

// The whole-row fallback (CO-6) inserts every image of the batch after
// deleting their keys, so it contends for unique values with another
// shadow writer just as the column-wise upsert does, and a deadlock there
// is requeued the same way.
//
// Rows 1 and 2 swap their unique values 1 and 2, and rows 3 and 4 take
// 1000003 and 1000004, in one source transaction. The column-wise upsert
// writes key 1 first and is refused at once by the committed shadow row
// under key 2, which still holds 2, so every column-wise attempt ends
// there, before it reaches key 3 or 4. The fallback deletes keys 1 to 4
// and inserts them in order: it takes 1000003 under key 3 and then waits
// on 1000004, which the writer holds under key 9004, and the writer's
// insert of 1000003 under key 9003 closes the cycle. The server rolls the
// fallback back, the batch's four entries wait in the buffer, and once the
// writer rolls back the retry's fallback commits them (CO-4, CO-6).
func TestCatchupRequeuesAWholeRowFallbackRolledBackAsTheDeadlockVictim(t *testing.T) {
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{positions: &switchPosition{pos: allLanded()}})
	writer := f.holdShadowValue(t, run, rowsThreeAndFour)

	q := f.table.Qualified()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	// Row 1 parks on -1 so the source's own unique index admits the swap.
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = -1 WHERE id = 1`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = 1 WHERE id = 2`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = 2 WHERE id = 1`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = 1000003 WHERE id = 3`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `UPDATE `+q+` SET uniq = 1000004 WHERE id = 4`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))

	writer.deadlockWithTheFlush(t)
	run.awaitStatus(t, "record the fallback the server rolled back", func(s applier.Status) bool {
		return s.BatchesDeadlocked == 1
	})
	deadlocked := run.status(t)
	assert.Equal(t, 4, deadlocked.Deferred, "all four requeued entries wait in the buffer")
	assert.Zero(t, deadlocked.Fallbacks, "the rolled-back fallback committed nothing")
	require.NoError(t, writer.tx.Rollback(t.Context()), "the writer's rows vanish, as a rolled-back chunk pass's would")

	end := f.currentWALLSN(t)
	run.awaitStatus(t, "flush all four rows after the deadlock", func(s applier.Status) bool {
		return s.Confirmed >= end && s.Buffered == 0
	})
	s := run.status(t)
	assert.Equal(t, uint64(1), s.BatchesDeadlocked, "the one fallback the server rolled back")
	assert.Equal(t, uint64(1), s.Fallbacks, "the retry is refused column-wise again and commits whole-row")
	assert.Equal(t, uint64(4), s.Applied, "all four images land on the retry")
	assert.Zero(t, s.BatchesDeferred, "a deadlock is not a refusal")
	run.stop(t)
	f.assertConverged(t, run.shadow)
}
