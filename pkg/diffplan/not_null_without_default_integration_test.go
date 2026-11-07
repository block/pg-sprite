package diffplan_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
)

// A desired file that adds a NOT NULL column with no default plans the add
// as refused when the table holds rows, since PostgreSQL would refuse it,
// and as metadata-only when the table is empty.
func TestPlanRefusesANotNullColumnWithoutDefaultOnATableWithRows(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.customers (id bigint PRIMARY KEY)`, schema))
	require.NoError(t, err)
	desired := `CREATE TABLE customers (id bigint PRIMARY KEY, tier text NOT NULL);`
	add := fmt.Sprintf("ALTER TABLE %s.customers ADD COLUMN tier text NOT NULL", schema)

	plan := func(t *testing.T) (router.Disposition, planner.Decision) {
		t.Helper()
		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, desired)})
		require.NoError(t, err)
		require.Len(t, report.Statements, 1)
		require.Equal(t, add, report.Statements[0].SQL)
		require.Len(t, report.Statements[0].Decisions, 1)
		return report.Disposition, report.Statements[0].Decisions[0]
	}

	disposition, d := plan(t)
	assert.NotEqual(t, router.DispositionRefuse, disposition)
	assert.Equal(t, planner.ReasonMetadataOnly, d.Reason, "an empty table has no rows to fill")

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.customers VALUES (1)`, schema))
	require.NoError(t, err)
	disposition, d = plan(t)
	assert.Equal(t, router.DispositionRefuse, disposition)
	assert.Equal(t, planner.RouteRefuse, d.Route)
	assert.Equal(t, planner.ReasonNotNullWithoutDefault, d.Reason)
}
