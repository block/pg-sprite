package decode_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/decode"
)

// MarkWAL renders the position past its marker with ::pg_catalog.text, so
// a transaction on a caller's pool whose search_path lists a lookalike
// text type ahead of pg_catalog still returns the server's own position:
// past the write position before the marker and no further than the
// insert position after it (CO-9). A bare ::text would cut it to 0/1, a
// position every stream has passed, and a caller waiting for the stream to
// pass the marker would stop waiting before it had delivered what the
// transaction's snapshot saw.
func TestMarkWALReturnsTheServersPositionUnderALookalikeTextType(t *testing.T) {
	f := newSlotFixture(t)
	lookalike := testutil.NewLookalikeTextTypePool(t, f.databaseURL, f.pool)
	before := f.currentWALLSN(t)

	tx, err := lookalike.Begin(t.Context())
	require.NoError(t, err)
	// Redundant safety closer: the test commits the transaction, after
	// which Rollback returns the guaranteed ErrTxClosed. A failure before
	// the commit must still release the connection.
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })
	mark, err := decode.MarkWAL(t.Context(), tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))

	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_insert_lsn()::text`).Scan(&text))
	after, err := decode.ParseLSN(text)
	require.NoError(t, err)
	assert.Greater(t, mark, before, "the marker lies past the write position read before it")
	assert.LessOrEqual(t, mark, after, "the marker lies at or below the insert position read after it")
}
