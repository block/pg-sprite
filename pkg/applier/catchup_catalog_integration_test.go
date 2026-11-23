package applier_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
)

// Every cycle the catch-up reads the server's snapshot and write position
// on the pool Run is given and renders both with ::pg_catalog.text, so a
// caller's pool whose search_path lists a lookalike text type ahead of
// pg_catalog still drains against the server's snapshot and measures the
// server's own write position (CO-9). A bare ::text would cut the
// snapshot into one the drain refuses and end the run, and cut the write
// position to 0/1, below anything the stream has confirmed, so the lag
// would never read as the WAL still to work through.
func TestCatchupMeasuresTheServersPositionUnderALookalikeTextType(t *testing.T) {
	f := newCatchupFixture(t, 100)
	lookalike := testutil.NewLookalikeTextTypePool(t, f.cfg.URL, f.pool)
	run := startCatchupWith(t, f, catchupSetup{pool: lookalike})

	// One change gives the drain an entry to judge, so the cycle reads the
	// snapshot; the copy has cut no key, so the drain discards it as one the
	// copy will read.
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`
		UPDATE %s
		SET amount = amount + 1
		WHERE id = 1`, f.table.Qualified()))
	require.NoError(t, err)
	end := f.currentWALLSN(t)
	run.awaitStatus(t, fmt.Sprintf("discard the change and measure the server's write position at or past %s", end), func(s applier.Status) bool {
		return s.Discarded == 1 && s.WALEnd >= end
	})
	run.stop(t)
}
