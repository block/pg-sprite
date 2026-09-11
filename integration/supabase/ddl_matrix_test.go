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
		'table', (
			SELECT jsonb_build_array(
				oid, relfilenode, relrowsecurity, relforcerowsecurity, relacl
			)
			FROM pg_class
			WHERE oid = $1::regclass
		),
		'column_grants', (
			SELECT jsonb_agg(jsonb_build_array(attname, attacl) ORDER BY attname)
			FROM pg_attribute
			WHERE attrelid = $1::regclass AND attacl IS NOT NULL
		),
		'policies', (
			SELECT jsonb_agg(jsonb_build_array(
				polname, polcmd, polpermissive, polroles,
				pg_get_expr(polqual, polrelid),
				pg_get_expr(polwithcheck, polrelid)
			) ORDER BY polname)
			FROM pg_policy
			WHERE polrelid = $1::regclass
		),
		'publications', (
			SELECT jsonb_agg(jsonb_build_array(
				prpubid, prattrs, pg_get_expr(prqual, prrelid)
			) ORDER BY prpubid)
			FROM pg_publication_rel
			WHERE prrelid = $1::regclass
		)
	)::text`, table).Scan(&result))
	return result
}

func snapshot(t *testing.T, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var result string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
		'columns', (
			SELECT jsonb_agg(jsonb_build_array(
				a.attnum, a.attname, a.atttypid, a.atttypmod, a.attnotnull,
				a.attidentity, a.attgenerated, a.attacl,
				pg_get_expr(d.adbin, d.adrelid)
			) ORDER BY a.attnum)
			FROM pg_attribute a
			LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			WHERE a.attrelid = $1::regclass
				AND a.attnum > 0
				AND NOT a.attisdropped
		),
		'indexes', (
			SELECT jsonb_agg(jsonb_build_array(
				indexrelid, indisvalid, pg_get_indexdef(indexrelid)
			) ORDER BY indexrelid)
			FROM pg_index
			WHERE indrelid = $1::regclass
		),
		'constraints', (
			SELECT jsonb_agg(jsonb_build_array(
				conname, convalidated, pg_get_constraintdef(oid)
			) ORDER BY conname)
			FROM pg_constraint
			WHERE conrelid = $1::regclass
		),
		'rows', (
			SELECT jsonb_agg(to_jsonb(r) ORDER BY id)
			FROM `+table+` r
		)
	)::text`, table).Scan(&result))
	return protections(t, pool, table) + result
}

func seedDDLTable(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	table := newTable(t, pool, name)
	execSQL(t, pool, "ALTER TABLE "+table+`
		ADD COLUMN label varchar(8),
		ADD COLUMN amount numeric(8,2),
		ADD COLUMN note text`)
	execSQL(t, pool, "INSERT INTO "+table+` VALUES
		(1,'00000000-0000-0000-0000-000000000001','123','one',12.50,'ready'),
		(2,'00000000-0000-0000-0000-000000000002','456','two',25.25,'ready')`)
	execSQL(t, pool, "ALTER PUBLICATION supabase_realtime ADD TABLE "+table)
	execSQL(t, pool, "GRANT SELECT (body) ON "+table+" TO authenticated")
	return table
}

type nativeDDLCase struct{ name, setup, ddl, proof, want string }

// Adding a constant default fills existing rows without replacing the table.
func TestAddColumnWithConstantDefaultSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name:  "constant_default",
		ddl:   "ALTER TABLE %s ADD COLUMN enabled boolean NOT NULL DEFAULT true",
		proof: "SELECT bool_and(enabled)::text FROM %s",
		want:  "true",
	})
}

// Setting a default updates the stored expression while preserving existing rows.
func TestSetColumnDefaultSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "set_default",
		ddl:  "ALTER TABLE %s ALTER COLUMN note SET DEFAULT 'draft'",
		proof: `SELECT pg_get_expr(d.adbin, d.adrelid)
			FROM pg_attrdef d
			JOIN pg_attribute a ON a.attrelid = d.adrelid
			AND a.attnum = d.adnum
			WHERE d.adrelid='%s'::regclass
			AND a.attname='note'`,
		want: "'draft'::text",
	})
}

// Dropping a default removes the expression while preserving existing rows.
func TestDropColumnDefaultSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name:  "drop_default",
		setup: "ALTER TABLE %s ALTER COLUMN note SET DEFAULT 'draft'",
		ddl:   "ALTER TABLE %s ALTER COLUMN note DROP DEFAULT",
		proof: `SELECT count(*)::text
			FROM pg_attrdef d
			JOIN pg_attribute a ON a.attrelid = d.adrelid
			AND a.attnum = d.adnum
			WHERE d.adrelid='%s'::regclass
			AND a.attname='note'`,
		want: "0",
	})
}

// Relaxing nullability removes NOT NULL without changing existing data.
func TestDropNotNullSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "drop_not_null",
		ddl:  "ALTER TABLE %s ALTER COLUMN body DROP NOT NULL",
		proof: `SELECT attnotnull::text
			FROM pg_attribute
			WHERE attrelid='%s'::regclass
			AND attname='body'`,
		want: "false",
	})
}

// Existing non-null data passes validation and the column becomes NOT NULL.
func TestSetNotNullOnValidDataSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "set_not_null",
		ddl:  "ALTER TABLE %s ALTER COLUMN note SET NOT NULL",
		proof: `SELECT attnotnull::text
			FROM pg_attribute
			WHERE attrelid='%s'::regclass
			AND attname='note'`,
		want: "true",
	})
}

// Widening varchar preserves the table and its data; typmod includes four header bytes.
func TestWidenVarcharSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "varchar_widen",
		ddl:  "ALTER TABLE %s ALTER COLUMN label TYPE varchar(32)",
		proof: `SELECT atttypmod::text
			FROM pg_attribute
			WHERE attrelid='%s'::regclass
			AND attname='label'`,
		want: "36",
	})
}

// Converting varchar to text changes the type without replacing the table.
func TestVarcharToTextSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "varchar_to_text",
		ddl:  "ALTER TABLE %s ALTER COLUMN label TYPE text",
		proof: `SELECT atttypid::regtype::text
			FROM pg_attribute
			WHERE attrelid='%s'::regclass
			AND attname='label'`,
		want: "text",
	})
}

// A check constraint is added and validated against the existing rows.
func TestAddCheckConstraintSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "check_constraint",
		ddl:  "ALTER TABLE %s ADD CONSTRAINT positive CHECK (amount>0)",
		proof: `SELECT convalidated::text
			FROM pg_constraint
			WHERE conrelid='%s'::regclass
			AND conname='positive'`,
		want: "true",
	})
}

// NOT VALID adds the check without marking existing rows as validated.
func TestAddNotValidCheckSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "check_not_valid",
		ddl:  "ALTER TABLE %s ADD CONSTRAINT positive CHECK (amount>0) NOT VALID",
		proof: `SELECT convalidated::text
			FROM pg_constraint
			WHERE conrelid='%s'::regclass
			AND conname='positive'`,
		want: "false",
	})
}

// Validating an existing check marks it valid after checking the rows.
func TestValidateExistingCheckSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name:  "validate_check",
		setup: "ALTER TABLE %s ADD CONSTRAINT positive CHECK (amount>0) NOT VALID",
		ddl:   "ALTER TABLE %s VALIDATE CONSTRAINT positive",
		proof: `SELECT convalidated::text
			FROM pg_constraint
			WHERE conrelid='%s'::regclass
			AND conname='positive'`,
		want: "true",
	})
}

// Distinct existing values allow a valid unique constraint to be installed.
func TestAddUniqueConstraintSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "unique_constraint",
		ddl:  "ALTER TABLE %s ADD CONSTRAINT unique_body UNIQUE (body)",
		proof: `SELECT convalidated::text
			FROM pg_constraint
			WHERE conrelid='%s'::regclass
			AND conname='unique_body'`,
		want: "true",
	})
}

// Renaming an application column preserves the table and tenant access.
func TestRenameColumnSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "rename_column",
		ddl:  "ALTER TABLE %s RENAME COLUMN label TO caption",
		proof: `SELECT count(*)::text
			FROM pg_attribute
			WHERE attrelid='%s'::regclass
			AND attname='caption'
			AND NOT attisdropped`,
		want: "1",
	})
}

// The explicit DROP COLUMN statement removes the column; this is not desired-plan consent.
func TestDropColumnStatementSucceeds(t *testing.T) {
	runNativeDDL(t, nativeDDLCase{
		name: "drop_column",
		ddl:  "ALTER TABLE %s DROP COLUMN note",
		proof: `SELECT count(*)::text
			FROM pg_attribute
			WHERE attrelid='%s'::regclass
			AND attname='note'
			AND NOT attisdropped`,
		want: "0",
	})
}

func runNativeDDL(t *testing.T, tc nativeDDLCase) {
	t.Helper()
	pool := fixture(t)
	table := seedDDLTable(t, pool, "pgsprite_native_"+tc.name)
	if tc.setup != "" {
		execSQL(t, pool, fmt.Sprintf(tc.setup, table))
	}
	verifyAPITenants(t, "pgsprite_native_"+tc.name, map[int][]int{1: {1}, 2: {2}, 3: {}})
	before := protections(t, pool, table)
	change(t, pool, fmt.Sprintf(tc.ddl, table))
	var got string
	require.NoError(t, pool.QueryRow(t.Context(), fmt.Sprintf(tc.proof, table)).Scan(&got))
	assert.Equal(t, tc.want, got)
	assert.Equal(t, before, protections(t, pool, table), "native DDL must preserve access policies, grants, table identity and publication")
	var bodies []string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT array_agg(body ORDER BY id) FROM "+table).Scan(&bodies))
	assert.Equal(t, []string{"123", "456"}, bodies)
	verifyAPITenants(t, "pgsprite_native_"+tc.name, map[int][]int{1: {1}, 2: {2}, 3: {}})
}

// The safe_prefix addition proves whole-plan refusal: even a supported change
// must remain unapplied when another change requires unavailable copy-and-swap.
type copySwapCase struct {
	ddl     string
	desired string
}

// Integer widening needs copy-and-swap, so both entry points must refuse before applying any DDL.
func TestIntegerWideningRequiresUnavailableCopySwap(t *testing.T) {
	runServiceRefusal(t, copySwapCase{
		ddl: `ALTER TABLE %s
			ADD COLUMN safe_prefix text,
			ALTER COLUMN id TYPE bigint`,
		desired: `CREATE TABLE %s (
			id bigint PRIMARY KEY,
			owner_id uuid NOT NULL,
			body text NOT NULL,
			label varchar(8),
			amount numeric(8, 2),
			note text,
			safe_prefix text
		)`,
	})
}

// Converting text to integer needs copy-and-swap, even when all fixture values can be cast.
func TestTextToIntegerRequiresUnavailableCopySwap(t *testing.T) {
	runServiceRefusal(t, copySwapCase{
		ddl: `ALTER TABLE %s
			ADD COLUMN safe_prefix text,
			ALTER COLUMN body TYPE integer USING body::integer`,
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id uuid NOT NULL,
			body integer NOT NULL,
			label varchar(8),
			amount numeric(8, 2),
			note text,
			safe_prefix text
		)`,
	})
}

// Narrowing varchar is refused through the unavailable copy-and-swap path.
func TestVarcharShrinkRequiresUnavailableCopySwap(t *testing.T) {
	runServiceRefusal(t, copySwapCase{
		ddl: `ALTER TABLE %s
			ADD COLUMN safe_prefix text,
			ALTER COLUMN label TYPE varchar(4)`,
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id uuid NOT NULL,
			body text NOT NULL,
			label varchar(4),
			amount numeric(8, 2),
			note text,
			safe_prefix text
		)`,
	})
}

// Changing numeric scale is refused through the unavailable copy-and-swap path.
func TestNumericScaleChangeRequiresUnavailableCopySwap(t *testing.T) {
	runServiceRefusal(t, copySwapCase{
		ddl: `ALTER TABLE %s
			ADD COLUMN safe_prefix text,
			ALTER COLUMN amount TYPE numeric(8, 1)`,
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id uuid NOT NULL,
			body text NOT NULL,
			label varchar(8),
			amount numeric(8, 1),
			note text,
			safe_prefix text
		)`,
	})
}

// A volatile UUID default needs copy-and-swap rather than a metadata-only column addition.
func TestVolatileUUIDDefaultRequiresUnavailableCopySwap(t *testing.T) {
	runServiceRefusal(t, copySwapCase{
		ddl: `ALTER TABLE %s
			ADD COLUMN safe_prefix text,
			ADD COLUMN nonce uuid DEFAULT gen_random_uuid()`,
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id uuid NOT NULL,
			body text NOT NULL,
			label varchar(8),
			amount numeric(8, 2),
			note text,
			safe_prefix text,
			nonce uuid DEFAULT gen_random_uuid()
		)`,
	})
}

// A volatile timestamp default needs copy-and-swap rather than a metadata-only column addition.
func TestVolatileTimestampDefaultRequiresUnavailableCopySwap(t *testing.T) {
	runServiceRefusal(t, copySwapCase{
		ddl: `ALTER TABLE %s
			ADD COLUMN safe_prefix text,
			ADD COLUMN stamp timestamptz DEFAULT clock_timestamp()`,
		desired: `CREATE TABLE %s (
			id int PRIMARY KEY,
			owner_id uuid NOT NULL,
			body text NOT NULL,
			label varchar(8),
			amount numeric(8, 2),
			note text,
			safe_prefix text,
			stamp timestamptz DEFAULT clock_timestamp()
		)`,
	})
}

func refuseCopySwap(t *testing.T, pool *pgxpool.Pool, table, name string, tc copySwapCase) {
	t.Helper()
	before := snapshot(t, pool, table)
	st, err := statement.ParseOne(fmt.Sprintf(tc.ddl, table))
	require.NoError(t, err)
	v, err := migrate.Run(t.Context(), pool, st, migrate.DefaultOptions())
	require.NoError(t, err)
	require.Equal(t, verdict.OutcomeRefused, v.Outcome)
	require.Equal(t, verdict.ReasonBackendUnavailable, v.Reason)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, before, snapshot(t, pool, table), "statement refusal must have no side effects")
	ds, err := statement.ParseDesired(fmt.Sprintf(tc.desired, name))
	require.NoError(t, err)
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
