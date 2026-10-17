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
)

// A desired file that adds a column plans it as the server would run it.
// A domain with a CHECK rewrites every row even with a constant default, so
// it plans as copy-and-swap; a NOT NULL DEFAULT now() is stored in the
// catalog, so it plans in place. A stored generated column whose expression
// names another added column still plans, as copy-and-swap.
func TestPlanAddColumnFollowsTheProvenRewrite(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	cases := []struct {
		name   string
		column string
		route  planner.Route
		reason planner.Reason
	}{
		{"a constrained domain with a constant default", "score positive_int DEFAULT 1", planner.RouteCopyAndSwap, planner.ReasonTypeRewrite},
		{"a stored now() default", "created timestamptz NOT NULL DEFAULT now()", planner.RouteNative, planner.ReasonFastDefault},
		{"a volatile default", "token uuid DEFAULT gen_random_uuid()", planner.RouteCopyAndSwap, planner.ReasonVolatileDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(
				"CREATE DOMAIN %[1]s.positive_int AS int CHECK (VALUE > 0); CREATE TABLE %[1]s.t (id bigint PRIMARY KEY)", schema))
			require.NoError(t, err)

			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
				Schema:  schema,
				Desired: parseDesired(t, fmt.Sprintf("CREATE TABLE t (id bigint PRIMARY KEY, %s);", c.column)),
			})
			require.NoError(t, err)
			require.Len(t, report.Statements, 1)
			st := report.Statements[0]
			assert.Equal(t, c.route, st.Route)
			require.Len(t, st.Decisions, 1)
			assert.Equal(t, c.reason, st.Decisions[0].Reason)
		})
	}

	t.Run("a generated column over another added column", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id bigint PRIMARY KEY)", schema))
		require.NoError(t, err)

		report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{
			Schema: schema,
			Desired: parseDesired(t,
				"CREATE TABLE t (id bigint PRIMARY KEY, price int, doubled int GENERATED ALWAYS AS (price * 2) STORED);"),
		})
		require.NoError(t, err)
		reasons := map[string]planner.Reason{}
		for _, st := range report.Statements {
			require.Len(t, st.Decisions, 1, st.SQL)
			reasons[st.SQL] = st.Decisions[0].Reason
		}
		assert.Equal(t, map[string]planner.Reason{
			fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN price int", schema):                                          planner.ReasonMetadataOnly,
			fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN doubled int GENERATED ALWAYS AS (price * 2) STORED", schema): planner.ReasonGeneratedStored,
		}, reasons)
	})
}
