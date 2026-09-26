package supabase_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These requests go through PostgREST as application roles, never as the owner.
func TestAtomicRLSAPIEnforcesUserBoundaries(t *testing.T) {
	const name = "pgsprite_rls_api_boundaries"
	pool := rlsAPITable(t, name)
	sql := `CREATE TABLE pgsprite_rls_api_boundaries (
     id int PRIMARY KEY,
     owner_id uuid NOT NULL,
     body text NOT NULL
 );
 ALTER TABLE pgsprite_rls_api_boundaries ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON pgsprite_rls_api_boundaries
     FOR SELECT TO authenticated USING ((SELECT auth.uid()) = owner_id);
 CREATE POLICY writers ON pgsprite_rls_api_boundaries
     FOR INSERT TO authenticated WITH CHECK ((SELECT auth.uid()) = owner_id);
 CREATE POLICY editors ON pgsprite_rls_api_boundaries
     FOR UPDATE TO authenticated
     USING ((SELECT auth.uid()) = owner_id)
     WITH CHECK ((SELECT auth.uid()) = owner_id);
 CREATE POLICY removers ON pgsprite_rls_api_boundaries
     FOR DELETE TO authenticated USING ((SELECT auth.uid()) = owner_id);`
	report := applyAPIRLS(t, pool, sql)
	// The API behavior alone cannot distinguish own_rows from these four policies.
	// Check the complete replacement and its order as well as authorization below.
	assert.Equal(t, []string{
		`DROP POLICY "own_rows" ON "public"."pgsprite_rls_api_boundaries"`,
		`ALTER TABLE "public"."pgsprite_rls_api_boundaries" ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE "public"."pgsprite_rls_api_boundaries" NO FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY "editors" ON "public"."pgsprite_rls_api_boundaries"
    AS PERMISSIVE FOR UPDATE TO "authenticated"
    USING ((( SELECT auth.uid() AS uid) = owner_id))
    WITH CHECK ((( SELECT auth.uid() AS uid) = owner_id))`,
		`CREATE POLICY "readers" ON "public"."pgsprite_rls_api_boundaries"
    AS PERMISSIVE FOR SELECT TO "authenticated"
    USING ((( SELECT auth.uid() AS uid) = owner_id))`,
		`CREATE POLICY "removers" ON "public"."pgsprite_rls_api_boundaries"
    AS PERMISSIVE FOR DELETE TO "authenticated"
    USING ((( SELECT auth.uid() AS uid) = owner_id))`,
		`CREATE POLICY "writers" ON "public"."pgsprite_rls_api_boundaries"
    AS PERMISSIVE FOR INSERT TO "authenticated"
    WITH CHECK ((( SELECT auth.uid() AS uid) = owner_id))`,
	}, report.Statements)
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	assertRLSRead(t, name, 0)
	assertRLSInsertDenied(t, name, 0, 90, 1)
	assertRLSInsertDenied(t, name, 1, 91, 2)
	assertRLSInsertDenied(t, name, 2, 92, 1)
	status, body := rlsRequest(t, http.MethodPost, name+"?select=id", 1, fmt.Sprintf(`{"id":3,"owner_id":%q,"body":"new"}`, tenantID(1)))
	assertRLSRows(t, status, body, http.StatusCreated, 3)
	status, body = rlsRequest(t, http.MethodPatch, name+"?id=eq.3&select=id", 1, `{"body":"edited"}`)
	assertRLSRows(t, status, body, http.StatusOK, 3)
	var edited string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT body FROM public.pgsprite_rls_api_boundaries WHERE id=3").Scan(&edited))
	assert.Equal(t, "edited", edited)
	// WITH CHECK prevents a user from transferring their row to another owner.
	status, body = rlsRequest(t, http.MethodPatch, name+"?id=eq.3&select=id", 1, fmt.Sprintf(`{"owner_id":%q}`, tenantID(2)))
	assertRLSDenied(t, status, body, http.StatusForbidden)

	status, body = rlsRequest(t, http.MethodPatch, name+"?id=eq.2&select=id", 1, `{"body":"intrusion"}`)
	assertRLSRows(t, status, body, http.StatusOK)
	status, body = rlsRequest(t, http.MethodDelete, name+"?id=eq.2&select=id", 1, "")
	assertRLSRows(t, status, body, http.StatusOK)
	status, body = rlsRequest(t, http.MethodDelete, name+"?id=eq.3&select=id", 1, "")
	assertRLSRows(t, status, body, http.StatusOK, 3)
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	var remaining []string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT array_agg(body ORDER BY id) FROM public.pgsprite_rls_api_boundaries").Scan(&remaining))
	assert.Equal(t, []string{"first", "second"}, remaining)
	// A repeated declaration performs no live DDL and preserves authorization.
	again := applyAPIRLS(t, pool, sql)
	assert.Empty(t, again.Statements)
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	assertRLSRead(t, name, 0)
}

func TestAtomicRLSAPIChangesVisibleRows(t *testing.T) {
	const name = "pgsprite_rls_api_visibility"
	pool := rlsAPITable(t, name)
	execSQL(t, pool, `INSERT INTO public.pgsprite_rls_api_visibility VALUES
     (3, '00000000-0000-0000-0000-000000000001', 'private')`)
	assertRLSRead(t, name, 1, 1, 3)
	assertRLSRead(t, name, 2, 2)
	applyAPIRLS(t, pool, `CREATE TABLE pgsprite_rls_api_visibility (
     id int PRIMARY KEY,
     owner_id uuid NOT NULL,
     body text NOT NULL
 );
 ALTER TABLE pgsprite_rls_api_visibility ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON pgsprite_rls_api_visibility FOR SELECT TO authenticated
     USING ((SELECT auth.uid()) = owner_id AND body <> 'private');`)
	// No cache refresh: policy enforcement changes as soon as the transaction commits.
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	assertRLSRead(t, name, 0)
	assertRLSInsertDenied(t, name, 1, 90, 1)
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM public.pgsprite_rls_api_visibility").Scan(&count))
	assert.Equal(t, 3, count, "hidden rows still exist")
}

func TestAtomicRLSAPIRemovingLastPolicyDeniesAccess(t *testing.T) {
	const name = "pgsprite_rls_api_default_deny"
	pool := rlsAPITable(t, name)
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	applyAPIRLS(t, pool, `CREATE TABLE pgsprite_rls_api_default_deny (
     id int PRIMARY KEY,
     owner_id uuid NOT NULL,
     body text NOT NULL
 );
 ALTER TABLE pgsprite_rls_api_default_deny ENABLE ROW LEVEL SECURITY;`)
	assertRLSRead(t, name, 1)
	assertRLSRead(t, name, 2)
	assertRLSRead(t, name, 0)
	assertRLSInsertDenied(t, name, 1, 90, 1)
	assertRLSInsertDenied(t, name, 2, 91, 2)
	var count int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM public.pgsprite_rls_api_default_deny").Scan(&count))
	assert.Equal(t, 2, count, "default deny does not delete data")
}
