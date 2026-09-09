package testutil

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadGenerator(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	table := NewWorkloadTable(t, pool)
	require.NoError(t, table.SeedRows(t.Context(), 200))
	toastBytes, err := table.ToastBytes(t.Context())
	require.NoError(t, err)
	assert.Positive(t, toastBytes, "blob values must be stored out of line for the unchanged-TOAST update profile to mean anything")
	generator := StartLoad(t, pool, table, LoadSpec{
		Seed: 42, Workers: 4, RatePerSecond: 80,
		Mix:            Mix{Insert: 1, Update: 1, Delete: 1, UniqueMove: 1},
		HotRowFraction: 0.2, ToastRewriteFraction: 0.5,
	})
	runDeadline := time.NewTimer(2 * time.Second)
	defer runDeadline.Stop()
	<-runDeadline.C
	summary, err := generator.Stop()
	require.NoError(t, err)
	assert.Positive(t, summary.Inserts)
	assert.Positive(t, summary.Updates)
	assert.Positive(t, summary.Deletes)
	assert.Positive(t, summary.UniqueMoves)
	committed := summary.Inserts + summary.Updates + summary.Deletes + summary.UniqueMoves
	assert.Less(t, summary.Races, committed, "a run should commit far more than it aborts on expected races")
	var count, distinct int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT uniq) FROM `+table.Qualified()).Scan(&count, &distinct))
	assert.Equal(t, 200+summary.Inserts-summary.Deletes, count)
	assert.Equal(t, count, distinct)
}
