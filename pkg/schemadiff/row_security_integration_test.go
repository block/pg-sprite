package schemadiff_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

func rowSecurityTable(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
		id bigint PRIMARY KEY,
		owner_id bigint NOT NULL
	)`, schema))
	require.NoError(t, err)
	return pool, schema
}

func TestIntrospectRowSecurityDisabledWithoutPolicies(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, schemadiff.RowSecurity{}, m.RowSecurity)
	_, err = schemadiff.Render(m)
	require.NoError(t, err)
}

// Enabled RLS with no policies is meaningful: ordinary users get default deny.
// Export must refuse, rather than turn this into an unprotected table definition.
func TestIntrospectEnabledRowSecurityWithoutPoliciesRefusesExport(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, schemadiff.RowSecurity{Enabled: true}, m.RowSecurity)
	_, err = schemadiff.Render(m)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableRowSecurity)
}

// FORCE survives even while RLS is disabled; it must not disappear from a baseline.
func TestIntrospectForcedRowSecurityWhileDisabledRefusesExport(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents FORCE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, schemadiff.RowSecurity{Forced: true}, m.RowSecurity)
	_, err = schemadiff.Render(m)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableRowSecurity)
}

func TestIntrospectSelectPolicyPreservesPublicAndMissingWithCheck(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Read own documents"
		ON %s.documents
		FOR SELECT TO PUBLIC
		USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`COMMENT ON POLICY "Read own documents"
		ON %s.documents IS 'Readers see their own documents'`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.False(t, m.RowSecurity.Enabled, "policies can exist before RLS is enabled")
	require.Len(t, m.RowSecurity.Policies, 1)
	p := m.RowSecurity.Policies[0]
	assert.Equal(t, "Read own documents", p.Name)
	assert.Equal(t, schemadiff.PolicySelect, p.Command)
	assert.True(t, p.Permissive)
	assert.Equal(t, []string{"public"}, p.Roles)
	require.NotNil(t, p.Using)
	assert.Equal(t, "(owner_id = 7)", *p.Using)
	assert.Nil(t, p.WithCheck, "absence is not an explicit true expression")
	require.NotNil(t, p.Comment)
	assert.Equal(t, "Readers see their own documents", *p.Comment)
	_, err = schemadiff.Render(m)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableRowSecurity)
}

func TestIntrospectRestrictiveUpdatePolicyPreservesBothExpressions(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Update own documents"
		ON %s.documents AS RESTRICTIVE
		FOR UPDATE TO CURRENT_USER
		USING (owner_id = 7)
		WITH CHECK (owner_id = 8)`, schema))
	require.NoError(t, err)
	var role string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_user`).Scan(&role))
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, m.RowSecurity.Policies, 1)
	p := m.RowSecurity.Policies[0]
	assert.Equal(t, schemadiff.PolicyUpdate, p.Command)
	assert.False(t, p.Permissive)
	assert.Equal(t, []string{role}, p.Roles)
	require.NotNil(t, p.Using)
	require.NotNil(t, p.WithCheck)
	assert.Equal(t, "(owner_id = 7)", *p.Using)
	assert.Equal(t, "(owner_id = 8)", *p.WithCheck)
	assert.Nil(t, p.Comment)
}

func TestIntrospectInsertPolicyHasOnlyWithCheck(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Insert own documents"
		ON %s.documents
		FOR INSERT TO PUBLIC
		WITH CHECK (owner_id = 7)`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, m.RowSecurity.Policies, 1)
	p := m.RowSecurity.Policies[0]
	assert.Equal(t, schemadiff.PolicyInsert, p.Command)
	assert.Nil(t, p.Using)
	require.NotNil(t, p.WithCheck)
	assert.Equal(t, "(owner_id = 7)", *p.WithCheck)
}

func TestIntrospectDeletePolicyPreservesCommand(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Delete own documents"
		ON %s.documents
		FOR DELETE TO PUBLIC
		USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, m.RowSecurity.Policies, 1)
	assert.Equal(t, schemadiff.PolicyDelete, m.RowSecurity.Policies[0].Command)
	assert.Nil(t, m.RowSecurity.Policies[0].WithCheck)
}

func TestIntrospectAllPolicyPreservesImplicitWithCheck(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Own documents"
		ON %s.documents
		FOR ALL TO PUBLIC
		USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, m.RowSecurity.Policies, 1)
	p := m.RowSecurity.Policies[0]
	assert.Equal(t, schemadiff.PolicyAll, p.Command)
	assert.Nil(t, p.WithCheck, "PostgreSQL uses USING as the check; do not fabricate an explicit clause")
}

// A helper in the table's schema must remain qualified, just like auth.uid()
// in Supabase. Otherwise replay in a scratch schema could bind another function.
func TestIntrospectPolicyQualifiesExternalHelper(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.current_owner()
		RETURNS bigint LANGUAGE sql STABLE AS 'SELECT 7::bigint'`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Read own documents"
		ON %s.documents FOR SELECT TO PUBLIC
		USING (owner_id = %s.current_owner())`, schema, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, m.RowSecurity.Policies, 1)
	require.NotNil(t, m.RowSecurity.Policies[0].Using)
	assert.Equal(t, fmt.Sprintf("(owner_id = %s.current_owner())", schema), *m.RowSecurity.Policies[0].Using)
}

func TestIntrospectPoliciesAndRolesHaveStableNameOrder(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// Register roles before the schema so cleanup removes dependent policies first.
	firstRole := testutil.NewRole(t, pool, "NOLOGIN")
	secondRole := testutil.NewRole(t, pool, "NOLOGIN")
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
		id bigint PRIMARY KEY,
		owner_id bigint NOT NULL
	)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Z readers"
		ON %s.documents FOR SELECT TO %s, %s
		USING (owner_id = 7)`, schema,
		pgx.Identifier{secondRole}.Sanitize(), pgx.Identifier{firstRole}.Sanitize()))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "A readers"
		ON %s.documents FOR SELECT TO PUBLIC
		USING (owner_id = 8)`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, m.RowSecurity.Policies, 2)
	assert.Equal(t, "A readers", m.RowSecurity.Policies[0].Name)
	assert.Equal(t, "Z readers", m.RowSecurity.Policies[1].Name)
	roles := []string{firstRole, secondRole}
	slices.Sort(roles)
	assert.Equal(t, roles, m.RowSecurity.Policies[1].Roles)
}

// Table-only desired files still describe table structure, not access control.
// An observed policy must not turn omission into a request to remove that policy.
func TestRowSecurityOmittedFromDesiredFileDoesNotDerivePolicyChanges(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Read own documents"
		ON %s.documents FOR SELECT TO PUBLIC USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	ds, err := statement.ParseDesired(`CREATE TABLE documents (
		id bigint PRIMARY KEY,
		owner_id bigint NOT NULL
	)`)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, live.RowSecurity, after.RowSecurity)
}

// Library callers can construct models directly. A policy-bearing desired model
// must not be mistaken for a supported access-control plan, even when unchanged.
func TestRowSecurityInDesiredModelRefusesDiff(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	m, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, m, m)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, changes)
}
