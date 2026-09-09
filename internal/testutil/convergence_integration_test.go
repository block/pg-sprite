package testutil

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvergenceOracle(t *testing.T) {
	dsn := StartPostgres(t)
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sourceTable := NewWorkloadTable(t, pool)
	require.NoError(t, sourceTable.SeedRows(t.Context(), 30))
	source := RelationRef{Schema: sourceTable.Schema, Table: sourceTable.Table}
	shadow := RelationRef{Schema: source.Schema, Table: "shadow"}
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+qualified(shadow)+` (LIKE `+qualified(source)+` INCLUDING ALL)`)
	require.NoError(t, err)
	// amount widens within the numeric category; uniq moves to another
	// category entirely, which EXCEPT cannot match without the oracle's
	// source-side cast.
	_, err = pool.Exec(t.Context(), `ALTER TABLE `+qualified(shadow)+` ALTER COLUMN amount TYPE numeric(14,4), ALTER COLUMN uniq TYPE text`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `INSERT INTO `+qualified(shadow)+` SELECT id, uniq::text, amount, label, blob, updated_at FROM `+qualified(source))
	require.NoError(t, err)

	AssertConverged(t, pool, source, shadow, ConvergeOptions{})
	report, err := Diff(t.Context(), pool, source, shadow, ConvergeOptions{})
	require.NoError(t, err)
	assert.True(t, report.Converged())

	_, err = pool.Exec(t.Context(), `UPDATE `+qualified(shadow)+` SET label='different',updated_at=updated_at+'1 second'::interval WHERE id=7`)
	require.NoError(t, err)
	report, err = Diff(t.Context(), pool, source, shadow, ConvergeOptions{IgnoreColumns: []string{"updated_at"}})
	require.NoError(t, err)
	assert.False(t, report.Converged())
	assert.Equal(t, []DirectionDiff{{Direction: "source-minus-shadow", Keys: []int64{7}}, {Direction: "shadow-minus-source", Keys: []int64{7}}}, report.Differences)

	_, err = pool.Exec(t.Context(), `UPDATE `+qualified(shadow)+` SET label=`+pgx.Identifier{"s"}.Sanitize()+`.label FROM `+qualified(source)+` s WHERE `+qualified(shadow)+`.id=s.id`)
	require.NoError(t, err)
	report, err = Diff(t.Context(), pool, source, shadow, ConvergeOptions{IgnoreColumns: []string{"updated_at"}})
	require.NoError(t, err)
	assert.True(t, report.Converged())

	// A wide divergence is reported by its twenty lowest keys, not
	// enumerated: 25 differing rows yield exactly ids 1..20 per direction.
	_, err = pool.Exec(t.Context(), `UPDATE `+qualified(shadow)+` SET label='wide' WHERE id<=25`)
	require.NoError(t, err)
	report, err = Diff(t.Context(), pool, source, shadow, ConvergeOptions{IgnoreColumns: []string{"updated_at"}})
	require.NoError(t, err)
	lowest := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	assert.Equal(t, []DirectionDiff{{Direction: "source-minus-shadow", Keys: lowest}, {Direction: "shadow-minus-source", Keys: lowest}}, report.Differences)
	assert.Equal(t, report.SourceCount, report.ShadowCount, "row values differ but counts do not")

	// A count skew is reported through the differing keys alone: a row only
	// the shadow holds is a shadow-minus-source key, a row only the source
	// lost is another, and no row appears in the opposite direction.
	_, err = pool.Exec(t.Context(), `UPDATE `+qualified(shadow)+` SET label=`+pgx.Identifier{"s"}.Sanitize()+`.label FROM `+qualified(source)+` s WHERE `+qualified(shadow)+`.id=s.id`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `INSERT INTO `+qualified(shadow)+` (id, uniq, amount, label, blob) VALUES (31, '31', 0.31, 'shadow-only', NULL)`)
	require.NoError(t, err)
	report, err = Diff(t.Context(), pool, source, shadow, ConvergeOptions{IgnoreColumns: []string{"updated_at"}})
	require.NoError(t, err)
	assert.False(t, report.Converged())
	assert.Equal(t, []DirectionDiff{{Direction: "shadow-minus-source", Keys: []int64{31}}}, report.Differences)
	assert.Equal(t, int64(30), report.SourceCount)
	assert.Equal(t, int64(31), report.ShadowCount)

	_, err = pool.Exec(t.Context(), `DELETE FROM `+qualified(source)+` WHERE id=3`)
	require.NoError(t, err)
	report, err = Diff(t.Context(), pool, source, shadow, ConvergeOptions{IgnoreColumns: []string{"updated_at"}})
	require.NoError(t, err)
	assert.False(t, report.Converged())
	assert.Equal(t, []DirectionDiff{{Direction: "shadow-minus-source", Keys: []int64{3, 31}}}, report.Differences)
	assert.Equal(t, int64(29), report.SourceCount)
	assert.Equal(t, int64(31), report.ShadowCount)

	// Ignoring the primary key is refused: the report names rows by it.
	_, err = Diff(t.Context(), pool, source, shadow, ConvergeOptions{IgnoreColumns: []string{"id"}})
	assert.EqualError(t, err, "shadow "+shadow.Schema+".shadow primary key id cannot be ignored")
}
