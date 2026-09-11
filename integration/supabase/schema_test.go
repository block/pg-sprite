package supabase_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDesiredAuthDefault(t *testing.T) {
	pool := fixture(t)
	name := "pgsprite_desired_auth_default"
	table := newTable(t, pool, name)
	ds, err := statement.ParseDesired(fmt.Sprintf(`CREATE TABLE %s (
		id int PRIMARY KEY,
		owner_id uuid NOT NULL DEFAULT auth.uid(),
		body text NOT NULL
	)`, name))
	require.NoError(t, err)
	request := diffplan.Request{Schema: "public", Desired: ds}
	report, err := diffplan.Plan(t.Context(), pool, request)
	require.NoError(t, err)
	require.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	result, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: "public", Desired: ds, ExpectedFingerprint: report.Fingerprint}, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	again, err := diffplan.Plan(t.Context(), pool, request)
	require.NoError(t, err)
	assert.Empty(t, again.Statements, "applying the desired file must converge")
	var enabled bool
	var policies int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT relrowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&enabled))
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_policy WHERE polrelid=$1::regclass", table).Scan(&policies))
	assert.True(t, enabled)
	assert.Equal(t, 1, policies)

	var expression string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT column_default
		FROM information_schema.columns
		WHERE table_schema='public'
		AND table_name = $1
		AND column_name='owner_id'`, name).Scan(&expression))
	assert.Equal(t, "auth.uid()", expression)
}

func TestDesiredExtensionType(t *testing.T) {
	pool := fixture(t)
	execSQL(t, pool, "CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA extensions")
	name := "pgsprite_desired_extension_type"
	table := newTable(t, pool, name)
	ds, err := statement.ParseDesired(fmt.Sprintf(`CREATE TABLE %s (
		id int PRIMARY KEY,
		owner_id uuid NOT NULL,
		body text NOT NULL,
		email extensions.citext
	)`, name))
	require.NoError(t, err)
	request := diffplan.Request{Schema: "public", Desired: ds}
	report, err := diffplan.Plan(t.Context(), pool, request)
	require.NoError(t, err)
	require.Equal(t, router.DispositionExecute, report.Disposition)
	require.Len(t, report.Statements, 1)
	result, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: "public", Desired: ds, ExpectedFingerprint: report.Fingerprint}, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	again, err := diffplan.Plan(t.Context(), pool, request)
	require.NoError(t, err)
	assert.Empty(t, again.Statements, "applying the desired file must converge")
	var enabled bool
	var policies int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT relrowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&enabled))
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_policy WHERE polrelid=$1::regclass", table).Scan(&policies))
	assert.True(t, enabled)
	assert.Equal(t, 1, policies)

	var schema, typ string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT udt_schema, udt_name
		FROM information_schema.columns
		WHERE table_schema='public'
		AND table_name = $1
		AND column_name='email'`, name).Scan(&schema, &typ))
	assert.Equal(t, "extensions", schema)
	assert.Equal(t, "citext", typ)
}

func TestTextRewriteLeavesSchemaUnchanged(t *testing.T) {
	pool := fixture(t)
	table := newTable(t, pool, "pgsprite_refused_rewrite")
	execSQL(t, pool, "INSERT INTO "+table+" VALUES (1,'00000000-0000-0000-0000-000000000001','123')")
	st, err := statement.ParseOne("ALTER TABLE " + table + " ALTER COLUMN body TYPE integer USING body::integer")
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)
	var typ, body string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT pg_typeof(body)::text,body FROM "+table+" WHERE id=1").Scan(&typ, &body))
	assert.Equal(t, "text", typ)
	assert.Equal(t, "123", body)
}

func TestEnableRLSLeavesSchemaUnchanged(t *testing.T) {
	pool := fixture(t)
	table := newTable(t, pool, "pgsprite_refused_rls")
	execSQL(t, pool, "ALTER TABLE "+table+" DISABLE ROW LEVEL SECURITY")
	st, err := statement.ParseOne("ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY")
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	var enabled bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT relrowsecurity FROM pg_class WHERE oid=$1::regclass", table).Scan(&enabled))
	assert.False(t, enabled, "a refused statement must not enable RLS")
}
