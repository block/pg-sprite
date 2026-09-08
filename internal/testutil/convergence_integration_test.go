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
	require.NoError(t, sourceTable.SeedRows(t.Context(), 20))
	source := RelationRef{Schema: sourceTable.Schema, Table: sourceTable.Table}
	shadow := RelationRef{Schema: source.Schema, Table: "shadow"}
	_, err = pool.Exec(t.Context(), `CREATE TABLE `+qualified(shadow)+` (LIKE `+qualified(source)+` INCLUDING ALL)`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `ALTER TABLE `+qualified(shadow)+` ALTER COLUMN amount TYPE numeric(14,4)`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), `INSERT INTO `+qualified(shadow)+` SELECT * FROM `+qualified(source))
	require.NoError(t, err)

	AssertConverged(t, t.Context(), pool, source, shadow, ConvergeOptions{})
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
}
