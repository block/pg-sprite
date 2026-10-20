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
	"github.com/block/pg-sprite/pkg/verdict"
)

// A desired file that leaves out the unique index REPLICA IDENTITY USING
// INDEX selects, or a column that index keys on, plans the drop as a
// refused statement with the replica-identity reason, so the plan shows
// the change and no apply runs it. The rest of the file stays planned as
// usual: an unrelated column add still routes natively.
func TestPlanRefusesDroppingTheReplicaIdentity(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.accounts (id bigint PRIMARY KEY, tenant bigint NOT NULL, ref text NOT NULL);
			CREATE UNIQUE INDEX accounts_ref_idx ON %[1]s.accounts (tenant, ref);
			ALTER TABLE %[1]s.accounts REPLICA IDENTITY USING INDEX accounts_ref_idx`, schema))
		require.NoError(t, err)
		return schema
	}
	cases := []struct {
		name    string
		desired string
		refused string
	}{
		{
			name:    "the identity index left out",
			desired: "CREATE TABLE accounts (id bigint PRIMARY KEY, tenant bigint NOT NULL, ref text NOT NULL, note text);",
			refused: "DROP INDEX %s.accounts_ref_idx",
		},
		{
			name: "a key column left out",
			desired: "CREATE TABLE accounts (id bigint PRIMARY KEY, tenant bigint NOT NULL, note text);\n" +
				"CREATE UNIQUE INDEX accounts_ref_idx ON accounts (tenant);",
			refused: "ALTER TABLE %s.accounts DROP ref",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			schema := seed(t)
			report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, c.desired)})
			require.NoError(t, err)
			assert.Equal(t, router.DispositionRefuse, report.Disposition)

			refused := fmt.Sprintf(c.refused, schema)
			var found, addPlanned bool
			for _, st := range report.Statements {
				if st.SQL == refused {
					found = true
					assert.Equal(t, planner.RouteRefuse, st.Route)
					assert.Equal(t, verdict.ReasonUnsupportedStatement, st.Reason)
					assert.Equal(t, verdict.ClassCapabilityBoundary, st.Class)
					assert.Empty(t, st.ExecSQL)
					require.Len(t, st.Decisions, 1)
					assert.Equal(t, planner.ReasonReplicaIdentity, st.Decisions[0].Reason)
					continue
				}
				if st.SQL == fmt.Sprintf("ALTER TABLE %s.accounts ADD COLUMN note text", schema) {
					addPlanned = true
					assert.Equal(t, planner.RouteNative, st.Route)
				}
			}
			assert.True(t, found, "the drop is planned and refused, got %v", report.Statements)
			assert.True(t, addPlanned, "the unrelated column add is still planned, got %v", report.Statements)
		})
	}
}
