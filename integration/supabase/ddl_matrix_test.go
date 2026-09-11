package supabase_test

import (
	"fmt"
	"testing"

	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are independent catalog/data oracles, not comparisons of planner output.
func protections(t *testing.T, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var result string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'table',(SELECT jsonb_build_array(oid,relfilenode,relrowsecurity,relforcerowsecurity,relacl) FROM pg_class WHERE oid=$1::regclass),
 'column_grants',(SELECT jsonb_agg(jsonb_build_array(attname,attacl) ORDER BY attname) FROM pg_attribute WHERE attrelid=$1::regclass AND attacl IS NOT NULL),
 'policies',(SELECT jsonb_agg(jsonb_build_array(polname,polcmd,polpermissive,polroles,pg_get_expr(polqual,polrelid),pg_get_expr(polwithcheck,polrelid)) ORDER BY polname) FROM pg_policy WHERE polrelid=$1::regclass),
 'publications',(SELECT jsonb_agg(jsonb_build_array(prpubid,prattrs,pg_get_expr(prqual,prrelid)) ORDER BY prpubid) FROM pg_publication_rel WHERE prrelid=$1::regclass)
 )::text`, table).Scan(&result))
	return result
}

func snapshot(t *testing.T, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var result string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'columns',(SELECT jsonb_agg(jsonb_build_array(a.attnum,a.attname,a.atttypid,a.atttypmod,a.attnotnull,a.attidentity,a.attgenerated,a.attacl,pg_get_expr(d.adbin,d.adrelid)) ORDER BY a.attnum) FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped),
 'indexes',(SELECT jsonb_agg(jsonb_build_array(indexrelid,indisvalid,pg_get_indexdef(indexrelid)) ORDER BY indexrelid) FROM pg_index WHERE indrelid=$1::regclass),
 'constraints',(SELECT jsonb_agg(jsonb_build_array(conname,convalidated,pg_get_constraintdef(oid)) ORDER BY conname) FROM pg_constraint WHERE conrelid=$1::regclass),
 'rows',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM `+table+` r)
 )::text`, table).Scan(&result))
	return protections(t, pool, table) + result
}

func seedDDLTable(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	table := newTable(t, pool, name)
	execSQL(t, pool, "ALTER TABLE "+table+" ADD COLUMN label varchar(8), ADD COLUMN amount numeric(8,2), ADD COLUMN note text")
	execSQL(t, pool, "INSERT INTO "+table+" VALUES (1,'00000000-0000-0000-0000-000000000001','123','one',12.50,'ready'),(2,'00000000-0000-0000-0000-000000000002','456','two',25.25,'ready')")
	execSQL(t, pool, "ALTER PUBLICATION supabase_realtime ADD TABLE "+table)
	execSQL(t, pool, "GRANT SELECT (body) ON "+table+" TO authenticated")
	return table
}

func TestNativeDDLMatrix(t *testing.T) {
	pool := fixture(t)
	cases := []struct{ name, setup, ddl, proof, want string }{
		{"constant_default", "", "ADD COLUMN enabled boolean NOT NULL DEFAULT true", "SELECT bool_and(enabled)::text FROM %s", "true"},
		{"set_default", "", "ALTER COLUMN note SET DEFAULT 'draft'", "SELECT pg_get_expr(d.adbin,d.adrelid) FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid=d.adrelid AND a.attnum=d.adnum WHERE d.adrelid='%s'::regclass AND a.attname='note'", "'draft'::text"},
		{"drop_default", "ALTER TABLE %s ALTER COLUMN note SET DEFAULT 'draft'", "ALTER COLUMN note DROP DEFAULT", "SELECT count(*)::text FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid=d.adrelid AND a.attnum=d.adnum WHERE d.adrelid='%s'::regclass AND a.attname='note'", "0"},
		{"drop_not_null", "", "ALTER COLUMN body DROP NOT NULL", "SELECT attnotnull::text FROM pg_attribute WHERE attrelid='%s'::regclass AND attname='body'", "false"},
		{"set_not_null", "", "ALTER COLUMN note SET NOT NULL", "SELECT attnotnull::text FROM pg_attribute WHERE attrelid='%s'::regclass AND attname='note'", "true"},
		{"varchar_widen", "", "ALTER COLUMN label TYPE varchar(32)", "SELECT atttypmod::text FROM pg_attribute WHERE attrelid='%s'::regclass AND attname='label'", "36"},
		{"varchar_to_text", "", "ALTER COLUMN label TYPE text", "SELECT atttypid::regtype::text FROM pg_attribute WHERE attrelid='%s'::regclass AND attname='label'", "text"},
		{"check_constraint", "", "ADD CONSTRAINT positive CHECK (amount>0)", "SELECT convalidated::text FROM pg_constraint WHERE conrelid='%s'::regclass AND conname='positive'", "true"},
		{"check_not_valid", "", "ADD CONSTRAINT positive CHECK (amount>0) NOT VALID", "SELECT convalidated::text FROM pg_constraint WHERE conrelid='%s'::regclass AND conname='positive'", "false"},
		{"validate_check", "ALTER TABLE %s ADD CONSTRAINT positive CHECK (amount>0) NOT VALID", "VALIDATE CONSTRAINT positive", "SELECT convalidated::text FROM pg_constraint WHERE conrelid='%s'::regclass AND conname='positive'", "true"},
		{"unique_constraint", "", "ADD CONSTRAINT unique_body UNIQUE (body)", "SELECT convalidated::text FROM pg_constraint WHERE conrelid='%s'::regclass AND conname='unique_body'", "true"},
		{"rename_column", "", "RENAME COLUMN label TO caption", "SELECT count(*)::text FROM pg_attribute WHERE attrelid='%s'::regclass AND attname='caption' AND NOT attisdropped", "1"},
		{"drop_column", "", "DROP COLUMN note", "SELECT count(*)::text FROM pg_attribute WHERE attrelid='%s'::regclass AND attname='note' AND NOT attisdropped", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := seedDDLTable(t, pool, "pgsprite_native_"+tc.name)
			if tc.setup != "" {
				execSQL(t, pool, fmt.Sprintf(tc.setup, table))
			}
			verifyAPITenants(t, "pgsprite_native_"+tc.name, map[int][]int{1: {1}, 2: {2}, 3: {}})
			before := protections(t, pool, table)
			change(t, pool, "ALTER TABLE "+table+" "+tc.ddl)
			var got string
			require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(tc.proof, table)).Scan(&got))
			assert.Equal(t, tc.want, got)
			assert.Equal(t, before, protections(t, pool, table), "native DDL must preserve access policies, grants, table identity and publication")
			var bodies []string
			require.NoError(t, pool.QueryRow(t.Context(), "SELECT array_agg(body ORDER BY id) FROM "+table).Scan(&bodies))
			assert.Equal(t, []string{"123", "456"}, bodies)
			verifyAPITenants(t, "pgsprite_native_"+tc.name, map[int][]int{1: {1}, 2: {2}, 3: {}})
		})
	}
}

type rewriteCase struct{ name, ddl, idType, bodyType, labelType, amountType, extra string }

func rewriteCases() []rewriteCase {
	return []rewriteCase{
		{"integer_width", "ALTER COLUMN id TYPE bigint", "bigint", "text", "varchar(8)", "numeric(8,2)", ""},
		{"text_to_integer", "ALTER COLUMN body TYPE integer USING body::integer", "int", "integer", "varchar(8)", "numeric(8,2)", ""},
		{"varchar_shrink", "ALTER COLUMN label TYPE varchar(4)", "int", "text", "varchar(4)", "numeric(8,2)", ""},
		{"numeric_scale", "ALTER COLUMN amount TYPE numeric(8,1)", "int", "text", "varchar(8)", "numeric(8,1)", ""},
		{"volatile_uuid", "ADD COLUMN nonce uuid DEFAULT gen_random_uuid()", "int", "text", "varchar(8)", "numeric(8,2)", ", nonce uuid DEFAULT gen_random_uuid()"},
		{"volatile_timestamp", "ADD COLUMN stamp timestamptz DEFAULT clock_timestamp()", "int", "text", "varchar(8)", "numeric(8,2)", ", stamp timestamptz DEFAULT clock_timestamp()"},
	}
}

func desiredRewrite(t *testing.T, name string, tc rewriteCase) statement.DesiredSchema {
	t.Helper()
	// Include a harmless addition too: admission must reject the whole plan,
	// without first committing a safe prefix before discovering the rewrite.
	ds, err := statement.ParseDesired(fmt.Sprintf("CREATE TABLE %s (id %s PRIMARY KEY,owner_id uuid NOT NULL,body %s NOT NULL,label %s,amount %s,note text,safe_prefix text%s)", name, tc.idType, tc.bodyType, tc.labelType, tc.amountType, tc.extra))
	require.NoError(t, err)
	return ds
}

func refuseRewrite(t *testing.T, pool *pgxpool.Pool, table, name string, tc rewriteCase) {
	t.Helper()
	before := snapshot(t, pool, table)
	st, err := statement.ParseOne("ALTER TABLE " + table + " ADD COLUMN safe_prefix text, " + tc.ddl)
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome)
	require.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, before, snapshot(t, pool, table), "statement refusal must have no side effects")
	ds := desiredRewrite(t, name, tc)
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: "public", Desired: ds})
	require.NoError(t, err)
	assert.Equal(t, router.DispositionUnavailable, report.Disposition)
	require.GreaterOrEqual(t, len(report.Statements), 2)
	result, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: "public", Desired: ds}, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, result.Outcome)
	assert.Equal(t, verdict.ReasonBackendUnavailable, result.Reason)
	assert.Empty(t, result.Verdicts, "a refused desired plan must not execute its safe prefix")
	assert.Equal(t, before, snapshot(t, pool, table), "desired-schema refusal must have no side effects")
}

func TestCopySwapRefusalMatrix(t *testing.T) {
	pool := fixture(t)
	for _, tc := range rewriteCases() {
		t.Run(tc.name, func(t *testing.T) {
			name := "pgsprite_copy_" + tc.name
			table := seedDDLTable(t, pool, name)
			refuseRewrite(t, pool, table, name, tc)
		})
	}

}
