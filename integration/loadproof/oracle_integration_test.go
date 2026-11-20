package loadproof_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// The row oracle over real tables, staged as a swap would leave them: the
// retained source under its _old name and the live table under the
// source's name, holding the same rows. Three writes then reach the two
// sides the way a swap under load can carry them:
//
//   - key 4 is updated in the retained source at a position below the
//     swap's and never reaches the live table, a write the drain lost
//     between the last checkpoint poll and the swap;
//   - key 7 is updated in the live table above the swap's position, a write
//     the application made after the swap;
//   - key 9 is updated in the live table by a write the load listed as
//     unheard, one that committed after its client deadline fired.
//
// The oracle reports key 4 on both sides and nothing else: it judges a
// write committed before the swap however late, and leaves out a write
// after it and a write whose outcome the load never heard.
func TestJudgeRowsCatchesAWriteLostBeforeTheSwapAndLeavesOutTheRest(t *testing.T) {
	dsn := testutil.StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := testutil.NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 10))
	old := pgx.Identifier{table.Schema, schemachange.OldName(table.Schema, table.Table)}.Sanitize()
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+old+` (LIKE `+table.Qualified()+` INCLUDING ALL)`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `INSERT INTO `+old+` SELECT * FROM `+table.Qualified())
	require.NoError(t, err)

	lost := touchedBy(t, pool, `
		UPDATE `+old+`
		SET label = 'lost by the drain'
		WHERE id = 4
		RETURNING id, (pg_current_wal_insert_lsn() - '0/0'::pg_lsn)::bigint`)
	var swapText string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT pg_current_wal_insert_lsn()::text`).Scan(&swapText))
	swap, err := decode.ParseLSN(swapText)
	require.NoError(t, err)
	afterSwap := touchedBy(t, pool, `
		UPDATE `+table.Qualified()+`
		SET label = 'written after the swap'
		WHERE id = 7
		RETURNING id, (pg_current_wal_insert_lsn() - '0/0'::pg_lsn)::bigint`)
	_, err = pool.Exec(t.Context(), `
		UPDATE `+table.Qualified()+`
		SET label = 'committed after its deadline'
		WHERE id = 9`)
	require.NoError(t, err)
	summary := testutil.Summary{Touches: []testutil.Touch{lost, afterSwap}, Ambiguous: []int64{9}}

	report, excluded, err := judgeRows(t.Context(), pool, table, summary, swap)
	require.NoError(t, err)

	assert.Equal(t, []int64{7, 9}, excluded)
	assert.Equal(t, []testutil.DirectionDiff{
		{Direction: "source-minus-shadow", Keys: []int64{4}},
		{Direction: "shadow-minus-source", Keys: []int64{4}},
	}, report.Differences, "the write lost before the swap is a difference")
	assert.Equal(t, int64(8), report.SourceCount, "ten rows less the two left out")
}

// touchedBy runs an update that returns its key and the WAL position it
// read while it wrote, and returns them as the load would record them.
func touchedBy(t *testing.T, pool *pgxpool.Pool, update string) testutil.Touch {
	t.Helper()
	var id, position int64
	require.NoError(t, pool.QueryRow(t.Context(), update).Scan(&id, &position))
	return testutil.Touch{ID: id, Position: uint64(position)}
}
