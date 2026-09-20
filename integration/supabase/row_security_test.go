package supabase_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This proves catalog round-trip fidelity with Supabase's real role and auth
// helper. It does not claim support for applying policies or hosted projects.
func TestRowSecurityRoundTripWithSupabaseAuth(t *testing.T) {
	pool := fixture(t)
	name := "pgsprite_rls_roundtrip"
	table := newTable(t, pool, name)
	execSQL(t, pool, "DROP POLICY own_rows ON "+table)
	execSQL(t, pool, fmt.Sprintf(`CREATE POLICY readers ON %s
     FOR SELECT TO authenticated
     USING ((SELECT auth.uid()) = owner_id)`, table))
	execSQL(t, pool, fmt.Sprintf(`CREATE POLICY writers ON %s
     FOR INSERT TO authenticated
     WITH CHECK ((SELECT auth.uid()) = owner_id)`, table))
	live, err := schemadiff.Introspect(t.Context(), pool, "public", name)
	require.NoError(t, err)
	sql, err := schemadiff.RenderWithRowSecurity(live)
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	assert.Equal(t, live, model)
	require.Len(t, model.RowSecurity.Policies, 2)
	assert.Equal(t, []string{"authenticated"}, model.RowSecurity.Policies[0].Roles)
	assert.Nil(t, model.RowSecurity.Policies[0].WithCheck)
	assert.Nil(t, model.RowSecurity.Policies[1].Using)
	report, err := diffplan.PlanWithRowSecurity(t.Context(), pool, "public", desired)
	require.NoError(t, err)
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, "public", name)
	require.NoError(t, err)
	assert.Equal(t, live, after)
}
