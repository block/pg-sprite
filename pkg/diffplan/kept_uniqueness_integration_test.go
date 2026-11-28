package diffplan_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/plan"
)

// A unique rule that moves to an object of another name, an index becoming
// a constraint or a constraint becoming an index, keeps rejecting duplicates
// throughout: the new object is in place before the old one is dropped. The
// drop names the new object, read from the definitions the server prints, so
// a reviewer sees that no duplicate gets in.
func TestPlanNamesWhatKeepsAMovedUniqueRule(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	dropOf := func(t *testing.T, report plan.Report, name string) plan.Statement {
		t.Helper()
		for _, st := range report.Statements {
			if st.Drops != nil && st.Drops.Name == name {
				return st
			}
		}
		require.Failf(t, "no drop", "no statement drops %s", name)
		return plan.Statement{}
	}

	t.Run("a unique index becomes a constraint", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.items (id bigint PRIMARY KEY, sku text);
			CREATE UNIQUE INDEX items_sku_uidx ON %[1]s.items (sku);`, schema))
		require.NoError(t, err)
		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
			CREATE TABLE items (id bigint PRIMARY KEY, sku text, CONSTRAINT items_sku_key UNIQUE (sku));`)})
		require.NoError(t, err)
		drop := dropOf(t, report, "items_sku_uidx")
		assert.True(t, drop.Destructive)
		assert.Equal(t, "items_sku_key", drop.UniquenessKeptBy)
	})

	t.Run("a unique constraint becomes an index", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.items (id bigint PRIMARY KEY, sku text, CONSTRAINT items_sku_key UNIQUE (sku));`, schema))
		require.NoError(t, err)
		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
			CREATE TABLE items (id bigint PRIMARY KEY, sku text);
			CREATE UNIQUE INDEX items_sku_uidx ON items (sku) INCLUDE (id);`)})
		require.NoError(t, err)
		drop := dropOf(t, report, "items_sku_key")
		assert.True(t, drop.Destructive)
		assert.Equal(t, "items_sku_uidx", drop.UniquenessKeptBy)
	})

	t.Run("a unique rule that widens", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.items (id bigint PRIMARY KEY, sku text, region text, CONSTRAINT items_sku_key UNIQUE (sku));`, schema))
		require.NoError(t, err)
		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
			CREATE TABLE items (id bigint PRIMARY KEY, sku text, region text);
			CREATE UNIQUE INDEX items_sku_region_uidx ON items (sku, region);`)})
		require.NoError(t, err)
		assert.Empty(t, dropOf(t, report, "items_sku_key").UniquenessKeptBy,
			"two rows can share a sku once the rule covers sku and region")
	})
}
