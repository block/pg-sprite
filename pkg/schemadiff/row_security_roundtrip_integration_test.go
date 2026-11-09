package schemadiff_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil/modeltest"
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
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
	require.NoError(t, err, sql)
	assert.Equal(t, live, modeltest.Comparable(model), "complete catalog state must round-trip")
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
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.NotErrorIs(t, err, schemadiff.ErrOnlyTableDiffers, "the row security differs")
	assert.Empty(t, changes, "never emit a partially executable plan")
}

// DiffWithRowSecurity emits only row security changes, so a table change is
// refused even when the row security matches. The error says that only the
// table differs, which is how diffplan.PlanWithRowSecurity knows to plan the
// table definition on its own, and it still wraps ErrUnsupportedChange, so a
// caller that treats every difference as a refusal keeps refusing.
func TestRowSecurityTableChangeIsRefusedEvenWithUnchangedPolicies(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL,
     title text
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	changes, err := schemadiff.DiffWithRowSecurity(schema, live, model)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.ErrorIs(t, err, schemadiff.ErrOnlyTableDiffers)
	assert.Empty(t, changes)
}

// teamMembers creates the table a membership policy reads, in schema.
func teamMembers(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.team_members (
     team_id bigint NOT NULL,
     user_id bigint NOT NULL
 )`, schema))
	require.NoError(t, err)
}

// A policy that reads another table exports with that table named
// schema-qualified, and the exported file materializes to the same model:
// the scratch policy binds the live table the applied policy reads.
func TestRowSecurityRoundTripPolicyReadingAnotherTable(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	teamMembers(t, pool, schema)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %[1]s.documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY members ON %[1]s.documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %[1]s.team_members m WHERE m.user_id = documents.owner_id));
 CREATE POLICY writers ON %[1]s.documents FOR INSERT
     WITH CHECK (owner_id IN (SELECT user_id FROM %[1]s.team_members))`, schema))
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	sql, err := schemadiff.RenderWithRowSecurity(live)
	require.NoError(t, err)
	assert.Contains(t, sql, "FROM "+schema+".team_members m\n")
	roundTripRowSecurity(t, pool, schema)
}

// Materializing a policy that reads a live table takes only the ACCESS SHARE
// lock a plain SELECT takes on it: a session holding EXCLUSIVE, which every
// stronger lock and every write conflicts with, does not hold the build up.
func TestRowSecurityPolicyRelationTakesOnlyAccessShare(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	teamMembers(t, pool, schema)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = holder.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.team_members IN EXCLUSIVE MODE", schema))
	require.NoError(t, err)

	desired, err := statement.ParseDesiredWithRowSecurity(fmt.Sprintf(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY members ON documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %s.team_members m WHERE m.user_id = documents.owner_id));`, schema))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(ctx, pool, schema, desired)
	require.NoError(t, err)
	require.Len(t, model.RowSecurity.Policies, 1)
	require.NotNil(t, model.RowSecurity.Policies[0].Using)
	assert.Equal(t, fmt.Sprintf("(EXISTS ( SELECT 1\n   FROM %s.team_members m\n  WHERE (m.user_id = documents.owner_id)))", schema),
		*model.RowSecurity.Policies[0].Using)

	// A query that locks the rows it reads would take ROW SHARE instead, and
	// PostgreSQL waits for it behind the same holder, so such a policy is
	// refused before anything runs.
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL lock_timeout = '100ms'")
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY locking ON %[1]s.documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %[1]s.team_members FOR SHARE))`, schema))
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "55P03", pgErr.Code, "building a row-locking policy waits behind EXCLUSIVE")
	_, err = statement.ParseDesiredWithRowSecurity(fmt.Sprintf(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY locking ON documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %s.team_members FOR SHARE));`, schema))
	require.ErrorIs(t, err, statement.ErrPolicyLocksRows)
}

// A live policy that reads a pg_catalog table introspects with the table
// unqualified, because pg_catalog is always searched, so it cannot be
// exported; a desired file that names it qualified is refused as well.
func TestRowSecurityPolicyReadingACatalogTableIsRefused(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %[1]s.documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON %[1]s.documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname = current_user))`, schema))
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, live.RowSecurity.Policies, 1)
	assert.Contains(t, *live.RowSecurity.Policies[0].Using, "FROM pg_namespace n")
	_, err = schemadiff.RenderWithRowSecurity(live)
	require.ErrorIs(t, err, statement.ErrPolicyRelationDependency)

	_, err = statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname = current_user));`)
	require.ErrorIs(t, err, statement.ErrPolicyReadsCatalog)
}

// A live policy that reads its own table cannot be exported: a desired file
// would bind the name to the live table rather than the desired one.
func TestRowSecurityExportRefusesAPolicyReadingItsOwnTable(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE POLICY readers
     ON %s.documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %s.documents visible WHERE visible.owner_id = 7))`, schema, schema))
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	_, err = schemadiff.RenderWithRowSecurity(live)
	require.ErrorIs(t, err, statement.ErrPolicyReadsOwnTable)
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
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
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
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
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

// A row-security table can carry a column whose type its own schema
// defines. Export prints the type bare, as live introspection resolves it,
// and the scratch build resolves the bare name in the target schema, so the
// file round-trips without qualifying it.
func TestRowSecurityRoundTripsATargetSchemaType(t *testing.T) {
	pool, schema := rowSecurityTable(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %[1]s.mood AS ENUM ('calm', 'busy');
     ALTER TABLE %[1]s.documents ADD COLUMN m %[1]s.mood;
     ALTER TABLE %[1]s.documents ENABLE ROW LEVEL SECURITY;
     CREATE POLICY readers ON %[1]s.documents FOR SELECT USING (true)`, schema))
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
	_, err = schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42883", pgErr.Code, "unqualified public helper must not resolve")
	// The same existing helper resolves when the file states its identity.
	qualified := strings.Replace(sql, "USING ("+name, "USING (public."+name, 1)
	desired, err = statement.ParseDesiredWithRowSecurity(qualified)
	require.NoError(t, err)
	model, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), pool, schema, desired)
	require.NoError(t, err)
	require.Len(t, model.RowSecurity.Policies, 1)
}
