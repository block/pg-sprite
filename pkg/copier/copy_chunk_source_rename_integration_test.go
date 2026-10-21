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

// A source column renamed while the copy runs fails the copy at the next
// chunk, and fails it loudly: the copier selects the source by column name,
// so the chunk statement after the rename names a column the source no
// longer has, and PostgreSQL rejects it as undefined_column (42703). The
// rename itself is not refused by the copier's locks — a chunk holds only
// ACCESS SHARE on the source, and only for its own transaction, so the
// rename's ACCESS EXCLUSIVE queues behind the chunk in flight and completes
// in the gap before the next one — and the per-chunk relation check cannot
// see it, because the renamed table keeps its OID. What the test proves is
// that nothing lands misplaced: the chunk the rename interrupted is still in
// flight, the shadow holds nothing from it, and the source keeps every row.
func TestCopierFailsWhenASourceColumnIsRenamedDuringTheCopy(t *testing.T) {
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
	assert.Equal(t, copier.RefusalCause(""), copier.RefusalCauseOf(err), "a renamed column is the server's error, not a copier refusal")

	assert.Equal(t, int64(0), f.count(t, shadow.ShadowTable(), "true"), "no chunk landed after the rename")
	assert.Equal(t, int64(100), f.count(t, shadow.SourceTable(), "true"), "the source keeps every row")
	pos := c.Position()
	assert.Len(t, pos.InFlight, 1, "the chunk the rename interrupted never landed")
	assert.Equal(t, int64(math.MinInt64), pos.InFlight[0].Lower())
	assert.False(t, pos.Watermark.Valid(), "nothing landed")
}
