package copier_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/copier"
)

// sqlstateUndefinedColumn is the SQLSTATE PostgreSQL raises for a statement
// that names a column the relation does not have.
const sqlstateUndefinedColumn = "42703"

// A copied source column renamed after the first chunk is claimed, and
// before that chunk's transaction begins, fails the copy at that chunk: the
// copier selects the source by column name, so the chunk statement names a
// column the source no longer has, and PostgreSQL rejects it as
// undefined_column (42703). The rename is not refused by the copier's
// locks — a chunk holds only ACCESS SHARE on the source, and only for its
// own transaction, so the rename's ACCESS EXCLUSIVE queues behind the chunk
// in flight and completes in the gap before the next one — and the
// per-chunk relation check cannot see it, because the renamed table keeps
// its OID.
//
// The failure is the server's error, not a copier refusal: the copier has
// no refusal cause for a copied column whose name no longer resolves, so a
// caller tells this case apart only by its SQLSTATE. A changed source is
// typed later, by the cutover gate's fingerprint check.
//
// The test pins the case where a copied column's name stops resolving: the
// chunk the rename interrupted stays in flight, the shadow holds nothing
// from it, and the source keeps every row. A rename that leaves every
// copied name resolving to a column of a compatible type — two columns
// swapping names, a column re-added under the old name — is not detected by
// the copy; the verifier and the cutover gate stop the swap after those.
func TestCopierFailsWhenASourceColumnIsRenamedBeforeTheFirstChunk(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 100)
	source := pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()

	var renameErr error
	clock := &hookedClock{hook: func() {
		if _, err := f.pool.Exec(t.Context(), "ALTER TABLE "+source+" RENAME COLUMN qty TO quantity"); err != nil {
			renameErr = fmt.Errorf("rename the source column: %w", err)
		}
	}}
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1, Clock: clock})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.NoError(t, renameErr, "the rename completed between the claim and the chunk transaction")
	require.Error(t, err)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the server's rejection of the stale column name is the cause")
	assert.Equal(t, sqlstateUndefinedColumn, pgErr.Code)
	assert.Equal(t, copier.RefusalCause(""), copier.RefusalCauseOf(err), "the copier has no refusal cause for a column that no longer resolves")

	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"), "no chunk landed after the rename")
	assert.Equal(t, int64(100), f.count(t, shadow.SourceTable(), "true"), "the source keeps every row")
	pos := c.Position()
	require.Len(t, pos.InFlight, 1, "the chunk the rename interrupted never landed")
	assert.Equal(t, int64(math.MinInt64), pos.InFlight[0].Lower())
	assert.False(t, pos.Watermark.Valid(), "nothing landed")
}

// A copied source column renamed after a chunk has landed fails the copy at
// the next chunk, for the same reason as before the first one, and leaves
// what already landed as it was: the chunk before the rename keeps its rows
// and the values they had, the watermark stops at that chunk's upper bound,
// and the chunk after the rename is the one left in flight. A copier that
// treated the failed chunk as landed would move the watermark past it and
// leave its keys uncopied. With one worker the copier reads the clock twice
// per chunk — before the chunk transaction and after the chunk lands — so
// the third reading falls after the first chunk landed and before the
// second chunk's transaction begins.
func TestCopierFailsWhenASourceColumnIsRenamedBetweenChunks(t *testing.T) {
	f := newCopierFixture(t)
	target, lock, shadow := f.prepare(t, 100)
	source := pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()

	var renameErr error
	clock := &nthReadingClock{at: 3, hook: func() {
		if _, err := f.pool.Exec(t.Context(), "ALTER TABLE "+source+" RENAME COLUMN qty TO quantity"); err != nil {
			renameErr = fmt.Errorf("rename the source column: %w", err)
		}
	}}
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 1,
		Clock:   clock,
		Chunker: copier.ChunkerOptions{InitialRows: 30, MaxRows: 30},
	})
	require.NoError(t, err)
	err = c.Run(t.Context(), f.pool)
	require.NoError(t, renameErr, "the rename completed between the first chunk landing and the second chunk transaction")
	require.Error(t, err)

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "the server's rejection of the stale column name is the cause")
	assert.Equal(t, sqlstateUndefinedColumn, pgErr.Code)

	assert.Equal(t, int64(30), f.count(t, shadow.ShadowTable(), "true"), "only the chunk before the rename landed")
	assert.Equal(t, int64(30), f.count(t, shadow.ShadowTable(), "qty = id"), "the landed rows carry the values they had before the rename")
	assert.Equal(t, int64(100), f.count(t, shadow.SourceTable(), "true"), "the source keeps every row")
	pos := c.Position()
	require.Len(t, pos.InFlight, 1, "the chunk the rename interrupted never landed")
	assert.Equal(t, int64(31), pos.InFlight[0].Lower(), "the chunk after the rename is the one left in flight")
	require.True(t, pos.Watermark.Valid(), "the chunk before the rename landed")
	assert.Equal(t, int64(30), pos.Watermark.Value(), "the watermark stops at the last chunk that landed")
}
