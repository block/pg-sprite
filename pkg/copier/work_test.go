package copier

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// Outside Run the copier holds no connection: Work reports the ledger's
// rows and the recorded total with the sizes at an honest zero, and touches
// no database.
func TestWorkOutsideRunReportsRowsWithoutSizes(t *testing.T) {
	c := &Copier{ledger: newLedger(Watermark{}), rowsTotal: 5000}
	first := mustChunk(t, math.MinInt64, 100)
	second := mustChunk(t, 101, 200)
	require.True(t, c.ledger.claim(first))
	require.True(t, c.ledger.claim(second))
	require.True(t, c.ledger.land(second, 100))
	require.True(t, c.ledger.land(first, 37))

	work, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{RowsCopied: 137, RowsTotal: 5000}, work)
}

// A copier that has not run knows nothing: every counter is zero.
func TestWorkBeforeRunIsZero(t *testing.T) {
	c := &Copier{ledger: newLedger(Watermark{})}
	work, err := c.Work(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.Work{}, work)
}
