package migrate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A column a row-level security policy reads cannot be retyped or dropped
// while the policy exists; PostgreSQL refuses even a widening it would
// otherwise relabel without a rewrite. The run is refused at plan time
// with the policies named, the table's own and the one on another table
// that reads the column through a subquery, so the operator learns what
// to change before anything reaches the server, and the column is left as
// it was. The detail gives the manual path that fits the change: rewrite
// the policies before a drop, recreate them around an in-place type
// change, and move them to a new column for a type change that rewrites.
// A column no policy reads still retypes.
func TestRunRefusesChangingAColumnAPolicyReads(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	seed := func(t *testing.T) string {
		t.Helper()
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.documents (
				id bigint PRIMARY KEY,
				owner_id varchar(36),
				label varchar(100)
			);
			CREATE TABLE %[1]s.shares (
				id bigint PRIMARY KEY,
				grantee varchar(36)
			);
			ALTER TABLE %[1]s.documents ENABLE ROW LEVEL SECURITY;
			CREATE POLICY own_rows ON %[1]s.documents
				USING (owner_id = current_user);
			CREATE POLICY shared_with_owner ON %[1]s.shares
				USING (grantee IN (SELECT d.owner_id FROM %[1]s.documents d))`, schema))
		require.NoError(t, err)
		return schema
	}

	for _, tc := range []struct {
		name      string
		statement string
		fix       planner.PolicyFix
	}{
		{name: "a widening the server would relabel", statement: "ALTER TABLE %s.documents ALTER COLUMN owner_id TYPE varchar(64)", fix: planner.PolicyFixRecreateAroundChange},
		{name: "a type change to text", statement: "ALTER TABLE %s.documents ALTER COLUMN owner_id TYPE text", fix: planner.PolicyFixRecreateAroundChange},
		{name: "a type change that rewrites", statement: "ALTER TABLE %s.documents ALTER COLUMN owner_id TYPE bigint USING owner_id::bigint", fix: planner.PolicyFixExpandContract},
		{name: "a column drop", statement: "ALTER TABLE %s.documents DROP COLUMN owner_id", fix: planner.PolicyFixRewritePolicies},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := seed(t)
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.statement, schema)), runOptions())
			require.NoError(t, err)
			assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
			assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
			assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
			policies := []string{
				fmt.Sprintf(`"own_rows" ON "%s"."documents"`, schema),
				fmt.Sprintf(`"shared_with_owner" ON "%s"."shares"`, schema),
			}
			assert.Equal(t, policies, v.DependentPolicies)
			assert.Empty(t, v.DependentViews)
			assert.Contains(t, v.Detail, "a column read by policies "+policies[0]+", "+policies[1]+",")
			assert.True(t, strings.HasSuffix(v.Detail, "; "+tc.fix.Help()), v.Detail)
			assert.Equal(t, "character varying(36)", columnType(t, pool, schema, "documents", "owner_id"))
		})
	}

	t.Run("the plan names the policies", func(t *testing.T) {
		schema := seed(t)
		st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.documents ALTER COLUMN owner_id TYPE varchar(64)", schema))
		facts, err := migrate.LiveFacts(t.Context(), pool, st)
		require.NoError(t, err)
		plan, err := planner.Classify(st.SQL(), facts.Classifier)
		require.NoError(t, err)
		require.Len(t, plan.Decisions, 1)
		assert.Equal(t, planner.ReasonDependentPolicy, plan.Decisions[0].Reason)
		assert.Equal(t, []string{fmt.Sprintf(`"own_rows" ON "%s"."documents"`, schema),
			fmt.Sprintf(`"shared_with_owner" ON "%s"."shares"`, schema)}, plan.Decisions[0].DependentPolicies)
	})

	t.Run("a column no policy reads still retypes", func(t *testing.T) {
		schema := seed(t)
		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.documents ALTER COLUMN label TYPE text", schema)), runOptions())
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
		assert.Empty(t, v.DependentPolicies)
		assert.Equal(t, "text", columnType(t, pool, schema, "documents", "label"))
	})
}
