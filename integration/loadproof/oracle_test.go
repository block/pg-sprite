package loadproof_test

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// excludedKeys returns, sorted, the keys the row oracle leaves out of both
// the retained source and the live table: every key the load wrote at a
// WAL position above the swap's, a write that ran after the swap and
// exists only in the live table, and every key of a write whose outcome
// the load never heard, which may have landed on either side or neither.
// Every other key was last written below the swap's position, before the
// swap took its lock, so the retained source holds that write and the live
// table must hold it too: a write the drain lost or applied twice, however
// close to the swap, is a difference.
func excludedKeys(s testutil.Summary, swap decode.LSN) []int64 {
	out := map[int64]struct{}{}
	for _, touch := range s.Touches {
		if decode.LSN(touch.Position) > swap {
			out[touch.ID] = struct{}{}
		}
	}
	for _, id := range s.Ambiguous {
		out[id] = struct{}{}
	}
	return slices.Sorted(maps.Keys(out))
}

// judgeRows compares the retained source with the live table over every
// key excludedKeys does not leave out, returning the report and the keys
// left out.
func judgeRows(ctx context.Context, pool *pgxpool.Pool, table testutil.WorkloadTable, summary testutil.Summary, swap decode.LSN) (testutil.Report, []int64, error) {
	excluded := excludedKeys(summary, swap)
	report, err := testutil.Diff(ctx, pool,
		testutil.RelationRef{Schema: table.Schema, Table: schemachange.OldName(table.Schema, table.Table)},
		testutil.RelationRef{Schema: table.Schema, Table: table.Table},
		testutil.ConvergeOptions{ExcludeKeys: excluded})
	return report, excluded, err
}

// A write the load committed after the last checkpoint poll before the
// cutover but below the swap's position happened while the drain or the
// swap's lock wait was under way, before the lock was granted: the
// retained source holds it, so the oracle judges the key. Only a write
// above the swap's position is left out.
func TestExcludedKeysJudgesAWriteBetweenTheMarkAndTheSwap(t *testing.T) {
	summary := testutil.Summary{Touches: []testutil.Touch{
		{ID: 1, Position: 100}, // before the mark
		{ID: 2, Position: 300}, // after the mark, during the drain
		{ID: 3, Position: 500}, // after the swap committed
	}}
	assert.Equal(t, []int64{3}, excludedKeys(summary, 400))
}

// A key's later write decides: one written below the swap's position and
// again above it is in the live table only in its last form, so it is left
// out, once.
func TestExcludedKeysLeavesOutAKeyWrittenAgainAfterTheSwap(t *testing.T) {
	summary := testutil.Summary{Touches: []testutil.Touch{
		{ID: 5, Position: 100},
		{ID: 5, Position: 500},
		{ID: 5, Position: 600},
	}}
	assert.Equal(t, []int64{5}, excludedKeys(summary, 400))
}

// A write whose client deadline expired carries no position, since the
// load never heard it commit; the server may have committed it after the
// swap, into the live table alone, so its key is left out.
func TestExcludedKeysLeavesOutAWriteThatRanOutItsDeadline(t *testing.T) {
	summary := testutil.Summary{
		Touches:   []testutil.Touch{{ID: 1, Position: 100}},
		Ambiguous: []int64{9, 2},
	}
	assert.Equal(t, []int64{2, 9}, excludedKeys(summary, 400))
}
