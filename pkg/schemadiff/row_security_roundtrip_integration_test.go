package schemadiff_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

func roundTripRowSecurity(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	sql, err := schemadiff.RenderWithRowSecurity(live)
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err, sql)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err, sql)
	assert.Equal(t, live, model, "complete catalog state must round-trip")
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.NoError(t, err)
	assert.Empty(t, changes)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, live, after, "scratch inspection must leave the live table untouched")
}

func TestRowSecurityRoundTripEnabledWithoutPolicies(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripDisabledWithoutPolicies(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripDisabledWithForcedSelectPolicyAndComment(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents FORCE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY "Reader's documents"
     ON %s.documents FOR SELECT TO PUBLIC
     USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`COMMENT ON POLICY "Reader's documents"
     ON %s.documents IS E'Reader''s rule\\path'`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripRestrictiveUpdateWithBothClauses(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY updates
     ON %s.documents AS RESTRICTIVE FOR UPDATE TO CURRENT_USER
     USING (owner_id = 7) WITH CHECK (owner_id = 8)`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripInsertWithCheck(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY inserts
     ON %s.documents FOR INSERT TO PUBLIC WITH CHECK (owner_id = 7)`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripDeleteUsing(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY deletes
     ON %s.documents FOR DELETE TO PUBLIC USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripAllWithImplicitCheck(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY owns
     ON %s.documents FOR ALL TO PUBLIC USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRoundTripQualifiedScalarHelper(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.current_owner()
     RETURNS bigint LANGUAGE sql STABLE AS 'SELECT 7::bigint'`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers
     ON %s.documents FOR SELECT TO PUBLIC
     USING (owner_id = (SELECT %s.current_owner()))`, schema, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityRemovalOfLastPolicyIsRefused(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers
     ON %s.documents FOR SELECT TO PUBLIC USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, changes, "never emit a partially executable plan")
}

func TestRowSecurityTableChangeIsRefusedEvenWithUnchangedPolicies(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL,
     title text
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, changes)
}

func TestRowSecurityExportRefusesRelationDependencies(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers
     ON %s.documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %s.documents visible WHERE visible.owner_id = 7))`, schema, schema))
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	_, err = schemadiff.RenderWithRowSecurity(live)
	require.ErrorIs(t, err, statement.ErrPolicyRelationDependency)
}

func TestRowSecurityForceChangeIsRefused(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 ALTER TABLE documents FORCE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY`, schema))
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, changes)
}

func TestRowSecurityChangedPredicateIsRefused(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers
     ON %s.documents FOR SELECT TO PUBLIC USING (owner_id = 7)`, schema))
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT TO PUBLIC USING (true);`)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Empty(t, changes, "a wider predicate must not become an empty plan")
}

func TestRowSecurityQualifiedEnumCastRoundTrips(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.doc_status AS ENUM ('active', 'hidden')`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY active_documents
     ON %[1]s.documents FOR SELECT
     USING ('active'::%[1]s.doc_status = 'active'::%[1]s.doc_status)`, schema))
	require.NoError(t, err)
	roundTripRowSecurity(t, pool, schema)
}

func TestRowSecurityDoesNotBindUnqualifiedPublicHelper(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	name := pgx.Identifier{"rls_helper_" + schema}.Sanitize()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION public.%s()
     RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'`, name))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP FUNCTION public."+name+"()")
		assert.NoError(t, err)
	})
	sql := fmt.Sprintf(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (%s());`, name)
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	_, err = schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42883", pgErr.Code, "unqualified public helper must not resolve")
	// The same existing helper resolves when the file states its identity.
	qualified := strings.Replace(sql, "USING ("+name, "USING (public."+name, 1)
	desired, err = statement.ParseDesiredWithRowSecurity(qualified)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, desired)
	require.NoError(t, err)
	require.Len(t, model.RowSecurity.Policies, 1)
}
