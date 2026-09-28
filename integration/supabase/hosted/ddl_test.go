package hosted_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *fixture) apply(t *testing.T, sql string) verdict.Verdict {
	t.Helper()
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	result, err := migrate.Run(t.Context(), f.pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeExecuted, result.Outcome)
	return result
}

// Volatile defaults require copy-and-swap; refusal must preserve the live table.
func realtimeRefuseUUID(t *testing.T, f *fixture, first, second *stream) {
	f.refuse(t, first, second, "ALTER TABLE "+f.table+" ADD COLUMN token uuid DEFAULT gen_random_uuid()", fmt.Sprintf(`CREATE TABLE %s (
 id integer PRIMARY KEY,
 owner_id uuid NOT NULL,
 body text NOT NULL,
 safe_prefix text,
 token uuid DEFAULT gen_random_uuid()
 )`, pgx.Identifier{f.name}.Sanitize()))
}
func realtimeRefuseTimestamp(t *testing.T, f *fixture, first, second *stream) {
	f.refuse(t, first, second, "ALTER TABLE "+f.table+" ADD COLUMN created_at timestamptz DEFAULT clock_timestamp()", fmt.Sprintf(`CREATE TABLE %s (
 id integer PRIMARY KEY,
 owner_id uuid NOT NULL,
 body text NOT NULL,
 safe_prefix text,
 created_at timestamptz DEFAULT clock_timestamp()
 )`, pgx.Identifier{f.name}.Sanitize()))
}

func (f *fixture) snapshot(t *testing.T) string {
	t.Helper()
	var result string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'columns',(SELECT jsonb_agg(to_jsonb(a) ORDER BY attnum) FROM pg_attribute a WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped),
 'policies',(SELECT jsonb_agg(to_jsonb(p) ORDER BY polname) FROM pg_policy p WHERE polrelid=$1::regclass),
 'table',(SELECT jsonb_build_array(oid,relfilenode,relrowsecurity,relacl) FROM pg_class WHERE oid=$1::regclass),
 'rows',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM `+f.table+` r)
 )::text`, f.table).Scan(&result))
	return result
}
func (f *fixture) refuse(t *testing.T, first, second *stream, sql, desired string) {
	t.Helper()
	before := f.snapshot(t)
	st, err := statement.ParseOne(sql)
	require.NoError(t, err)
	result, err := migrate.Run(t.Context(), f.pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Equal(t, verdict.ReasonBackendUnavailable, result.Reason)
	assert.Empty(t, result.ExecutedSQL)
	ds, err := statement.ParseDesired(desired)
	require.NoError(t, err)
	plan, err := diffplan.Plan(t.Context(), f.pool, diffplan.Request{Schema: "public", Desired: ds})
	require.NoError(t, err)
	require.Equal(t, router.DispositionUnavailable, plan.Disposition)
	report, err := migrate.RunDesired(t.Context(), f.pool, migrate.DesiredRequest{Schema: "public", Desired: ds}, migrate.DefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, report.Outcome)
	assert.Equal(t, verdict.ReasonBackendUnavailable, report.Reason)
	assert.Empty(t, report.Verdicts)
	assert.Equal(t, before, f.snapshot(t))
	f.assertAccess(t)
	f.exec(t, "UPDATE "+f.table+" SET body=$1", "after "+t.Name())
	first.exactRow(t, "UPDATE", 1)
	second.exactRow(t, "UPDATE", 2)
}
