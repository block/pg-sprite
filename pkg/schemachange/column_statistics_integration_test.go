package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// statisticsTargets reads the per-column statistics targets a table sets
// explicitly, as the catalog stores them; a column at the default is absent.
func (f shadowFixture) statisticsTargets(t *testing.T, table string) []schemachange.ColumnStatisticsTarget {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT a.attname, a.attstattarget
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		  AND COALESCE(a.attstattarget, -1) <> -1
		ORDER BY a.attnum`, f.schema, table)
	require.NoError(t, err)
	defer rows.Close()
	var targets []schemachange.ColumnStatisticsTarget
	for rows.Next() {
		var target schemachange.ColumnStatisticsTarget
		require.NoError(t, rows.Scan(&target.Column, &target.Target))
		targets = append(targets, target)
	}
	require.NoError(t, rows.Err())
	return targets
}

// LIKE … INCLUDING ALL does not carry a column's statistics target, so a
// planner tuned with SET STATISTICS would lose the tuning at the swap. The
// builder snapshots the explicit targets and sets them on the shadow, and
// the shadow's own snapshot reads them back, so the gate holds the shadow
// to them (ST-5). A column left at the default is not a fact to carry.
func TestBuildShadowCarriesColumnStatisticsTargets(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id     bigint PRIMARY KEY,
			tenant integer NOT NULL,
			qty    integer NOT NULL,
			note   text
		)`)
	f.exec(t, `ALTER TABLE %s.orders ALTER COLUMN tenant SET STATISTICS 500`)
	f.exec(t, `ALTER TABLE %s.orders ALTER COLUMN note SET STATISTICS 10`)

	built, err := f.build(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint`)
	require.NoError(t, err)

	want := []schemachange.ColumnStatisticsTarget{
		{Column: "tenant", Target: 500},
		{Column: "note", Target: 10},
	}
	assert.Equal(t, want, built.Fidelity().ColumnStatisticsTargets)
	assert.Equal(t, want, built.ShadowFidelity().ColumnStatisticsTargets)
	assert.Equal(t, want, f.statisticsTargets(t, built.ShadowTable()),
		"LIKE does not carry statistics targets; the builder sets them on the shadow")
}
