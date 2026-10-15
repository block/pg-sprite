package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

// Each case names one shape of USING expression and the text the copy's
// SELECT list carries for it: a cast, a function call, a CASE, an operator
// expression, and identifiers that need quoting. The expected text is the
// deparser's rendering, not the user's spelling: a cast spelled
// CAST(... AS ...) comes back in :: form and integer comes back as int.
func TestColumnConversions(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want map[string]string
	}{
		{
			name: "no USING",
			sql:  `ALTER TABLE t ALTER COLUMN qty TYPE bigint`,
			want: map[string]string{},
		},
		{
			name: "cast",
			sql:  `ALTER TABLE t ALTER COLUMN qty TYPE integer USING qty::integer`,
			want: map[string]string{"qty": "qty::int"},
		},
		{
			name: "CAST spelling renders in :: form",
			sql:  `ALTER TABLE t ALTER COLUMN doc TYPE jsonb USING CAST(doc AS jsonb)`,
			want: map[string]string{"doc": "doc::jsonb"},
		},
		{
			name: "enum cast through a schema-local type",
			sql:  `ALTER TABLE t ALTER COLUMN status TYPE status_kind USING status::status_kind`,
			want: map[string]string{"status": "status::status_kind"},
		},
		{
			name: "function call",
			sql:  `ALTER TABLE t ALTER COLUMN ts TYPE timestamptz USING to_timestamp(ts)`,
			want: map[string]string{"ts": "to_timestamp(ts)"},
		},
		{
			name: "CASE",
			sql:  `ALTER TABLE t ALTER COLUMN ts TYPE timestamptz USING CASE WHEN ts IS NULL THEN now() ELSE ts::timestamptz END`,
			want: map[string]string{"ts": "CASE WHEN ts IS NULL THEN now() ELSE ts::timestamptz END"},
		},
		{
			name: "operator expression and a quoted column",
			sql:  `ALTER TABLE t ALTER COLUMN "Select" TYPE integer USING length("Select") * 2`,
			want: map[string]string{"Select": `length("Select") * 2`},
		},
		{
			name: "two retyped columns, one with USING",
			sql:  `ALTER TABLE t ALTER COLUMN a TYPE text USING a::text, ALTER COLUMN b TYPE bigint`,
			want: map[string]string{"a": "a::text"},
		},
		{
			name: "USING beside other subcommands",
			sql:  `ALTER TABLE t DROP COLUMN notes, ALTER COLUMN qty TYPE integer USING qty::integer, ALTER COLUMN qty SET NOT NULL`,
			want: map[string]string{"qty": "qty::int"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := statement.ColumnConversions(tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestColumnConversionsRefusesOtherShapes(t *testing.T) {
	_, err := statement.ColumnConversions(`SELECT 1`)
	require.ErrorIs(t, err, statement.ErrNotAlterTable)
	_, err = statement.ColumnConversions(`ALTER TABLE t ADD COLUMN a int; ALTER TABLE t ADD COLUMN b int`)
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
	_, err = statement.ColumnConversions(`ALTER TABLE t ALTER COLUMN a TYPE text USING a::text, ALTER COLUMN a TYPE varchar USING a::varchar`)
	require.ErrorIs(t, err, statement.ErrConversionRepeated)
}

// An expression the deparser renders in a form that does not parse back
// is refused rather than handed to the copy: the deparser drops the
// parentheses around an array constructor that is then subscripted, so the
// rendering cannot be proven to be the user's expression. The sentinel
// lets the shadow builder type the refusal without reading the message.
func TestColumnConversionsRefusesAnExpressionThatDoesNotRoundTrip(t *testing.T) {
	_, err := statement.ColumnConversions(`
		ALTER TABLE t
			ALTER COLUMN a TYPE text USING (ARRAY[a, a])[1]`)
	require.ErrorIs(t, err, statement.ErrConversionRendering)
}

// A USING expression may name only the column it retypes, and only bare:
// the catch-up converts a column value on its own, with no neighbouring
// column, table alias, or whole row to resolve anything else against. The
// refusal names the reference as the user spelled it, wherever it sits in
// the expression.
func TestColumnConversionsRefuseAReferenceBeyondTheRetypedColumn(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		ref  string
	}{
		{
			name: "another column as an operand",
			sql:  `ALTER TABLE t ALTER COLUMN total TYPE numeric USING qty * price`,
			ref:  "qty",
		},
		{
			name: "another column nested in a CASE",
			sql:  `ALTER TABLE t ALTER COLUMN ts TYPE timestamptz USING CASE WHEN ts IS NULL THEN created_at ELSE ts::timestamptz END`,
			ref:  "created_at",
		},
		{
			name: "another column inside a function call",
			sql:  `ALTER TABLE t ALTER COLUMN qty TYPE integer USING coalesce(qty, fallback)::integer`,
			ref:  "fallback",
		},
		{
			name: "the retyped column qualified by the table",
			sql:  `ALTER TABLE t ALTER COLUMN qty TYPE integer USING t.qty::integer`,
			ref:  "t.qty",
		},
		{
			name: "a whole-row reference",
			sql:  `ALTER TABLE t ALTER COLUMN doc TYPE jsonb USING to_jsonb(t.*)`,
			ref:  "t.*",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := statement.ColumnConversions(tc.sql)
			require.ErrorIs(t, err, statement.ErrConversionForeignColumn)
			assert.ErrorContains(t, err, "refers to "+tc.ref)
		})
	}
}

// A constant expression and one that names the retyped column several
// times both pass: neither reaches past the column.
func TestColumnConversionsAllowTheRetypedColumnAlone(t *testing.T) {
	got, err := statement.ColumnConversions(`ALTER TABLE t ALTER COLUMN qty TYPE integer USING CASE WHEN qty ~ '^[0-9]+$' THEN qty::integer ELSE 0 END`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"qty": "CASE WHEN qty ~ '^[0-9]+$' THEN qty::int ELSE 0 END"}, got)
	got, err = statement.ColumnConversions(`ALTER TABLE t ALTER COLUMN flag TYPE boolean USING true`)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"flag": "true"}, got)
}

// The rendering the copy carries is the gated statement's expression as
// the retargeted statement carries it too: retargeting changes the
// relation and nothing else, so both yield the same conversions.
func TestColumnConversionsSurviveRetarget(t *testing.T) {
	sql := `ALTER TABLE app.t ALTER COLUMN qty TYPE integer USING nullif(trim(qty), '')::integer`
	retargeted, err := statement.RetargetRelation(sql, "app", "_pgsprite_t_new")
	require.NoError(t, err)
	gated, err := statement.ColumnConversions(sql)
	require.NoError(t, err)
	shadow, err := statement.ColumnConversions(retargeted)
	require.NoError(t, err)
	assert.Equal(t, gated, shadow)
	assert.Equal(t, map[string]string{"qty": "(NULLIF(TRIM (BOTH  FROM qty), ''))::int"}, gated)
}
