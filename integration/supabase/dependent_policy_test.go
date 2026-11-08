package supabase_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A dependent-policy case retypes a column a row-level security policy
// reads, as an imperative statement and as the desired schema that plans
// to it. setup adds policies beyond the fixture's own_rows; policies are
// the policy names the refusal must report, in order.
type dependentPolicyCase struct {
	name, setup, ddl, desired string
	policies                  []string
}

// Retyping owner_id, the column the fixture's own_rows policy compares to
// auth.uid(), is refused at plan time. PostgreSQL refuses any type change
// to a column a policy reads, so the statement could only fail at the
// server; pg-sprite names the policy instead, before the copy-and-swap
// route this rewrite would otherwise take is considered.
func TestRetypeColumnAPolicyReadsRefusesDependentPolicy(t *testing.T) {
	refuseDependentPolicy(t, dependentPolicyCase{
		name: "retype_policy_column",
		ddl:  "ALTER TABLE %s ALTER COLUMN owner_id TYPE text",
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id text NOT NULL,
			body text NOT NULL,
			label varchar(8),
			amount numeric(8, 2),
			note text
		)`,
		policies: []string{"own_rows"},
	})
}

// Widening a varchar is a relabel PostgreSQL runs in place, without a
// rewrite, but not while a policy reads the column. The setup adds a
// restrictive policy on label, and the widening is refused naming it,
// rather than planned as a native change that fails with SQLSTATE 0A000.
func TestWidenVarcharAPolicyReadsRefusesDependentPolicy(t *testing.T) {
	refuseDependentPolicy(t, dependentPolicyCase{
		name: "widen_policy_column",
		setup: `CREATE POLICY visible_labels ON %s
			AS RESTRICTIVE
			TO authenticated
			USING (label IS DISTINCT FROM 'hidden')`,
		ddl: "ALTER TABLE %s ALTER COLUMN label TYPE varchar(32)",
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id uuid NOT NULL,
			body text NOT NULL,
			label varchar(32),
			amount numeric(8, 2),
			note text
		)`,
		policies: []string{"visible_labels"},
	})
}

// refuseDependentPolicy proves both front doors refuse the case with the
// policies named and leave the table, its policies, grants, and
// publication membership as they were, and that tenants read through the
// API exactly what they read before.
func refuseDependentPolicy(t *testing.T, tc dependentPolicyCase) {
	t.Helper()
	pool := fixture(t)
	name := "pgsprite_policy_" + tc.name
	table := seedDDLTable(t, pool, name)
	if tc.setup != "" {
		execSQL(t, pool, fmt.Sprintf(tc.setup, table))
	}
	tenants := map[int][]int{1: {1}, 2: {2}, 3: {}}
	verifyAPITenants(t, name, tenants)
	before := snapshot(t, pool, table)
	policies := make([]string, len(tc.policies))
	for i, policy := range tc.policies {
		policies[i] = fmt.Sprintf(`"%s" ON "public"."%s"`, policy, name)
	}

	st, err := statement.ParseOne(fmt.Sprintf(tc.ddl, table))
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome, v.Detail)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, policies, v.DependentPolicies)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, before, snapshot(t, pool, table), "statement refusal must have no side effects")

	ds, err := statement.ParseDesired(fmt.Sprintf(tc.desired, name))
	require.NoError(t, err)
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: "public", Desired: ds})
	require.NoError(t, err)
	assert.Equal(t, router.DispositionRefuse, report.Disposition)
	require.Len(t, report.Statements, 1)
	require.Len(t, report.Statements[0].Decisions, 1)
	assert.Equal(t, planner.ReasonDependentPolicy, report.Statements[0].Decisions[0].Reason)
	assert.Equal(t, policies, report.Statements[0].Decisions[0].DependentPolicies)
	result, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: "public", Desired: ds}, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Empty(t, result.Verdicts, "a refused desired plan must not execute any statement")
	assert.Equal(t, before, snapshot(t, pool, table), "desired-schema refusal must have no side effects")
	verifyAPITenants(t, name, tenants)
}
