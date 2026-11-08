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

// A desired file that widens a column a row-level security policy reads
// plans the type change as a refused statement naming the policy, since
// PostgreSQL would refuse it while the policy exists. The rest of the file
// stays planned as usual: an unrelated column add still routes natively.
func TestPlanRefusesChangingAColumnAPolicyReads(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.documents (
			id bigint PRIMARY KEY,
			owner_id varchar(36)
		);
		CREATE POLICY own_rows ON %[1]s.documents
			USING (owner_id = current_user)`, schema))
	require.NoError(t, err)

	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema,
		Desired: parseDesired(t, `CREATE TABLE documents (
			id bigint PRIMARY KEY,
			owner_id varchar(64),
			note text
		);`)})
	require.NoError(t, err)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)

	var found, addPlanned bool
	for _, st := range report.Statements {
		switch st.SQL {
		case fmt.Sprintf("ALTER TABLE %s.documents ALTER COLUMN owner_id TYPE varchar(64)", schema):
			found = true
			assert.Equal(t, planner.RouteRefuse, st.Route)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, st.Reason)
			assert.Empty(t, st.ExecSQL)
			require.Len(t, st.Decisions, 1)
			assert.Equal(t, planner.ReasonDependentPolicy, st.Decisions[0].Reason)
			assert.Equal(t, []string{fmt.Sprintf(`"own_rows" ON "%s"."documents"`, schema)}, st.Decisions[0].DependentPolicies)
		case fmt.Sprintf("ALTER TABLE %s.documents ADD COLUMN note text", schema):
			addPlanned = true
			assert.Equal(t, planner.RouteNative, st.Route)
		}
	}
	assert.True(t, found, "the type change is planned and refused, got %v", report.Statements)
	assert.True(t, addPlanned, "the unrelated column add is still planned, got %v", report.Statements)
}
