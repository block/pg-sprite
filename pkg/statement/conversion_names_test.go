package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func unqualified(names ...string) []statement.QualifiedName {
	out := make([]statement.QualifiedName, 0, len(names))
	for _, n := range names {
		out = append(out, statement.QualifiedName{Name: n})
	}
	return out
}

func inSchema(schema string, names ...string) []statement.QualifiedName {
	out := make([]statement.QualifiedName, 0, len(names))
	for _, n := range names {
		out = append(out, statement.QualifiedName{Schema: schema, Name: n})
	}
	return out
}

// Each case is one USING expression as ColumnConversions renders it and the
// names the catalog is left to resolve for it. The expected names are what
// the grammar wrote down: integer is pg_catalog.int4 because that is how
// the parser spells the built-in, an unqualified function or operator has
// an empty schema, and a BETWEEN is the two comparisons it rewrites to.
func TestConversionNamesOf(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want statement.ConversionNames
	}{
		{
			name: "cast to a built-in type",
			expr: `qty::int`,
			want: statement.ConversionNames{Types: inSchema("pg_catalog", "int4")},
		},
		{
			name: "cast to a schema-local type",
			expr: `status::status_kind`,
			want: statement.ConversionNames{Types: unqualified("status_kind")},
		},
		{
			name: "unqualified function call",
			expr: `to_cents(price)`,
			want: statement.ConversionNames{Functions: unqualified("to_cents")},
		},
		{
			name: "qualified function call",
			expr: `public.lower(code)`,
			want: statement.ConversionNames{Functions: inSchema("public", "lower")},
		},
		{
			name: "operator between two casts",
			expr: `(price::numeric * 100)::int`,
			want: statement.ConversionNames{
				Operators: unqualified("*"),
				Types:     inSchema("pg_catalog", "int4", "numeric"),
			},
		},
		{
			name: "schema-qualified operator",
			expr: `price OPERATOR(app.*) 100`,
			want: statement.ConversionNames{Operators: inSchema("app", "*")},
		},
		{
			name: "CASE with an operand compares with =",
			expr: `CASE status WHEN 'a' THEN 1 ELSE 0 END`,
			want: statement.ConversionNames{Operators: unqualified("=")},
		},
		{
			name: "searched CASE resolves only the operators it spells",
			expr: `CASE WHEN qty > 0 THEN 1 ELSE 0 END`,
			want: statement.ConversionNames{Operators: unqualified(">")},
		},
		{
			name: "BETWEEN is the two comparisons the server rewrites it to",
			expr: `qty BETWEEN 1 AND 10`,
			want: statement.ConversionNames{Operators: unqualified("<=", ">=")},
		},
		{
			name: "BETWEEN SYMMETRIC is the same two comparisons, tried both ways round",
			expr: `qty BETWEEN SYMMETRIC 10 AND 1`,
			want: statement.ConversionNames{Operators: unqualified("<=", ">=")},
		},
		{
			name: "NOT BETWEEN is the strict comparisons the server rewrites it to",
			expr: `qty NOT BETWEEN 1 AND 10`,
			want: statement.ConversionNames{Operators: unqualified("<", ">")},
		},
		{
			name: "NOT BETWEEN SYMMETRIC is the same strict comparisons, tried both ways round",
			expr: `qty NOT BETWEEN SYMMETRIC 10 AND 1`,
			want: statement.ConversionNames{Operators: unqualified("<", ">")},
		},
		{
			name: "COLLATE names the collation",
			expr: `code COLLATE "C"`,
			want: statement.ConversionNames{Collations: unqualified("C")},
		},
		{
			name: "repeated names are reported once, sorted",
			expr: `upper(a) || upper(b) || lower(c)`,
			want: statement.ConversionNames{
				Functions: unqualified("lower", "upper"),
				Operators: unqualified("||"),
			},
		},
		{
			name: "a bare column spells no name",
			expr: `qty`,
			want: statement.ConversionNames{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := statement.ConversionNamesOf(tc.expr)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want.Empty(), got.Empty())
		})
	}
}

func TestConversionNamesOfRejectsAnUnparsableExpression(t *testing.T) {
	_, err := statement.ConversionNamesOf(`qty::`)
	require.Error(t, err)
	assert.ErrorContains(t, err, "parse USING expression")
}

func TestQualifiedNameString(t *testing.T) {
	assert.Equal(t, "to_cents", statement.QualifiedName{Name: "to_cents"}.String())
	assert.Equal(t, "app.to_cents", statement.QualifiedName{Schema: "app", Name: "to_cents"}.String())
}
