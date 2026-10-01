package schemachange_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// extendedStatisticsTargets reads the explicit targets of a table's
// extended-statistics objects as the catalog stores them; an object at the
// default is absent.
func (f shadowFixture) extendedStatisticsTargets(t *testing.T, table string) []schemachange.ExtendedStatisticsTarget {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT s.stxname, s.stxstattarget::int
		FROM pg_statistic_ext s JOIN pg_class c ON c.oid = s.stxrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND COALESCE(s.stxstattarget, -1) <> -1
		ORDER BY s.stxname`, f.schema, table)
	require.NoError(t, err)
	defer rows.Close()
	var targets []schemachange.ExtendedStatisticsTarget
	for rows.Next() {
		var target schemachange.ExtendedStatisticsTarget
		require.NoError(t, rows.Scan(&target.Name, &target.Target))
		targets = append(targets, target)
	}
	require.NoError(t, rows.Err())
	return targets
}

// LIKE … INCLUDING ALL copies an extended-statistics object but not its
// SET STATISTICS target, so a planner tuned through ALTER STATISTICS would
// lose the tuning at the swap. The builder snapshots each explicit target
// and sets it on the shadow object with the same definition — LIKE named
// that object after the shadow, so the two are paired by definition, not
// by name — and the shadow's own snapshot reads the targets back under the
// shadow's names. An object at the default is not a fact to carry.
//
// The gated statement then does to the shadow what it would do to the
// source: ALTER COLUMN … TYPE rebuilds every statistics object on the
// retyped column, and PostgreSQL's rebuild resets the object's target to
// the default. The object on the retyped column therefore comes out of the
// build at the default, exactly as a direct ALTER TABLE would leave it,
// while the object on untouched columns keeps the carried target.
func TestBuildShadowCarriesExtendedStatisticsTargets(t *testing.T) {
	newTunedOrders := func(t *testing.T) shadowFixture {
		f := newShadowFixture(t)
		f.exec(t, `
			CREATE TABLE %s.orders (
				id     bigint PRIMARY KEY,
				tenant integer NOT NULL,
				qty    integer NOT NULL,
				sku    text
			)`)
		f.exec(t, `CREATE STATISTICS %s.orders_tenant_qty_stat (ndistinct) ON tenant, qty FROM %s.orders`)
		f.exec(t, `CREATE STATISTICS %s.orders_tenant_sku_stat (dependencies) ON tenant, sku FROM %s.orders`)
		f.exec(t, `CREATE STATISTICS %s.orders_qty_sku_stat ON qty, sku FROM %s.orders`)
		f.exec(t, `ALTER STATISTICS %s.orders_tenant_qty_stat SET STATISTICS 500`)
		f.exec(t, `ALTER STATISTICS %s.orders_tenant_sku_stat SET STATISTICS 10`)
		return f
	}

	t.Run("a statement on other columns carries every target", func(t *testing.T) {
		f := newTunedOrders(t)
		built, err := f.build(t, "orders", `ALTER TABLE %s.orders ADD COLUMN note text`)
		require.NoError(t, err)

		shadow := built.ShadowTable()
		assert.Equal(t, []schemachange.ExtendedStatisticsTarget{
			{Name: "orders_tenant_qty_stat", Target: 500},
			{Name: "orders_tenant_sku_stat", Target: 10},
		}, built.Fidelity().ExtendedStatisticsTargets)
		want := []schemachange.ExtendedStatisticsTarget{
			{Name: shadow + "_tenant_qty_stat", Target: 500},
			{Name: shadow + "_tenant_sku_stat", Target: 10},
		}
		assert.Equal(t, want, built.ShadowFidelity().ExtendedStatisticsTargets)
		assert.Equal(t, want, f.extendedStatisticsTargets(t, shadow),
			"LIKE does not carry the targets; the builder sets them on the shadow's copies")
	})

	t.Run("a type change resets the target of the object it rebuilds, as PostgreSQL does", func(t *testing.T) {
		f := newTunedOrders(t)
		built, err := f.build(t, "orders", `ALTER TABLE %s.orders ALTER COLUMN qty TYPE bigint`)
		require.NoError(t, err)

		shadow := built.ShadowTable()
		want := []schemachange.ExtendedStatisticsTarget{
			{Name: shadow + "_tenant_sku_stat", Target: 10},
		}
		assert.Equal(t, want, built.ShadowFidelity().ExtendedStatisticsTargets,
			"the object on (tenant, qty) was rebuilt by the type change and left at the default")
		assert.Equal(t, want, f.extendedStatisticsTargets(t, shadow))
	})
}
