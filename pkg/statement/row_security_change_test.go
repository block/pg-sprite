package statement

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRowSecurityChangePreservesReplacementOrder(t *testing.T) {
	change, err := parseRowSecurityTestChange(t, []string{
		`DROP POLICY readers ON public.documents;`,
		`ALTER TABLE public.documents ENABLE ROW LEVEL SECURITY;`,
		`ALTER TABLE public.documents FORCE ROW LEVEL SECURITY;`,
		`CREATE POLICY readers ON public.documents FOR SELECT TO authenticated
   USING (owner_id = auth.uid());`,
		`COMMENT ON POLICY readers ON public.documents IS 'Your documents; only';`,
	})
	require.NoError(t, err)
	assert.Equal(t, "public", change.Schema())
	assert.Equal(t, "documents", change.Table())
	require.Len(t, change.Statements(), 5)
	same, err := parseRowSecurityTestChange(t, []string{
		`drop policy "readers" on "public"."documents";`,
		`alter table public.documents enable row level security;`,
		`alter table public.documents force row level security;`,
		`create policy readers on public.documents for select to authenticated using (owner_id=auth.uid());`,
		`comment on policy readers on public.documents is 'Your documents; only';`,
	})
	require.NoError(t, err)
	assert.Equal(t, change.CanonicalSQL(), same.CanonicalSQL())
	copy := change.Statements()
	copy[0] = "not SQL"
	assert.NotEqual(t, copy[0], change.Statements()[0], "callers cannot mutate the parsed operation")
}

func TestRowSecurityChangePreservesReviewDifferences(t *testing.T) {
	original, err := parseRowSecurityTestChange(t, []string{
		`DROP POLICY readers ON public.documents;`,
		`CREATE POLICY readers ON public.documents FOR SELECT TO alice USING (published);`,
	})
	require.NoError(t, err)
	for name, sql := range map[string][]string{
		"order": {
			`CREATE POLICY readers ON public.documents FOR SELECT TO alice USING (published);`,
			`DROP POLICY readers ON public.documents;`,
		},
		"predicate": {
			`DROP POLICY readers ON public.documents;`,
			`CREATE POLICY readers ON public.documents FOR SELECT TO alice USING (true);`,
		},
		"role": {
			`DROP POLICY readers ON public.documents;`,
			`CREATE POLICY readers ON public.documents FOR SELECT TO bob USING (published);`,
		},
		"duplicate": {
			`DROP POLICY readers ON public.documents;`,
			`CREATE POLICY readers ON public.documents FOR SELECT TO alice USING (published);`,
			`DROP POLICY readers ON public.documents;`,
		},
		"missing": {`CREATE POLICY readers ON public.documents FOR SELECT TO alice USING (published);`},
	} {
		t.Run(name, func(t *testing.T) {
			changed, err := parseRowSecurityTestChange(t, sql)
			require.NoError(t, err)
			assert.NotEqual(t, original.CanonicalSQL(), changed.CanonicalSQL())
		})
	}
}

func TestRowSecurityChangeAdmitsDisableAndNoForce(t *testing.T) {
	change, err := parseRowSecurityTestChange(t, []string{
		`ALTER TABLE public.documents NO FORCE ROW LEVEL SECURITY;`,
		`ALTER TABLE public.documents DISABLE ROW LEVEL SECURITY;`,
	})
	require.NoError(t, err)
	assert.Len(t, change.Statements(), 2)
}

func TestRowSecurityChangeRejectsOtherShapes(t *testing.T) {
	for name, sql := range map[string]string{
		"wrong schema":         `CREATE POLICY readers ON private.documents USING (true);`,
		"wrong table":          `DROP POLICY readers ON public.accounts;`,
		"unqualified":          `CREATE POLICY readers ON documents USING (true);`,
		"catalog qualifier":    `CREATE POLICY readers ON postgres.public.documents USING (true);`,
		"conditional drop":     `DROP POLICY IF EXISTS readers ON public.documents;`,
		"cascade":              `DROP POLICY readers ON public.documents CASCADE;`,
		"table DDL":            `ALTER TABLE public.documents ADD COLUMN title text;`,
		"mixed alter":          `ALTER TABLE public.documents ENABLE ROW LEVEL SECURITY, ADD COLUMN title text;`,
		"table comment":        `COMMENT ON TABLE public.documents IS 'documents';`,
		"other policy comment": `COMMENT ON POLICY readers ON private.documents IS 'private';`,
		"hidden statement":     `DROP POLICY readers ON public.documents; DELETE FROM public.documents;`,
		"DML":                  `DELETE FROM public.documents;`,
		"syntax error":         `CREATE POLICY`,
	} {
		t.Run(name, func(t *testing.T) {
			change, err := parseRowSecurityTestChange(t, []string{`DROP POLICY readers ON public.documents;`, sql})
			require.ErrorIs(t, err, ErrRowSecurityChange)
			assert.Equal(t, RowSecurityChange{}, change)
		})
	}
	_, err := parseRowSecurityTestChange(t, nil)
	require.ErrorIs(t, err, ErrRowSecurityChange)
}

func parseRowSecurityTestChange(t *testing.T, sql []string) (RowSecurityChange, error) {
	t.Helper()
	return ParseRowSecurityChange(strings.Join(sql, "\n"))
}

func TestRowSecurityChangeNamespaceComparison(t *testing.T) {
	first, err := ParseRowSecurityChange(`
  DROP POLICY readers ON staging.documents;
  ALTER TABLE staging.documents ENABLE ROW LEVEL SECURITY;
  CREATE POLICY readers ON staging.documents USING (owner_id = auth.uid());
  COMMENT ON POLICY readers ON staging.documents IS 'Access';
 `)
	require.NoError(t, err)
	second, err := ParseRowSecurityChange(`
  DROP POLICY readers ON production.documents;
  ALTER TABLE production.documents ENABLE ROW LEVEL SECURITY;
  CREATE POLICY readers ON production.documents USING (owner_id = auth.uid());
  COMMENT ON POLICY readers ON production.documents IS 'Access';
 `)
	require.NoError(t, err)
	originalStatements := first.Statements()
	originalCanonical := first.CanonicalSQL()
	a, err := first.CanonicalSQLForNamespace("staging", "documents")
	require.NoError(t, err)
	b, err := second.CanonicalSQLForNamespace("production", "documents")
	require.NoError(t, err)
	assert.Equal(t, a, b)
	assert.Contains(t, a, "auth.uid()")
	assert.Equal(t, originalStatements, first.Statements())
	assert.Equal(t, originalCanonical, first.CanonicalSQL())
	assert.NotEqual(t, first.CanonicalSQL(), second.CanonicalSQL())
	var empty RowSecurityChange
	_, err = empty.CanonicalSQLForNamespace("public", "documents")
	require.ErrorIs(t, err, ErrRowSecurityChange)
}

func TestRowSecurityNamespaceComparisonRejectsWrongTarget(t *testing.T) {
	change, err := ParseRowSecurityChange(`ALTER TABLE tenant_a.orders ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	for _, target := range []struct{ schema, table string }{{"tenant_b", "orders"}, {"tenant_a", "documents"}} {
		sql, err := change.CanonicalSQLForNamespace(target.schema, target.table)
		require.ErrorIs(t, err, ErrRowSecurityChange)
		assert.Empty(t, sql)
	}
}

func TestRowSecurityChangeSplitHandlesQuotedSemicolons(t *testing.T) {
	change, err := ParseRowSecurityChange(`
 -- a comment; not a statement
 CREATE POLICY "read;ers" ON public.documents USING (label = $$hello;world$$);
 COMMENT ON POLICY "read;ers" ON public.documents IS 'it''s; quoted';
 `)
	require.NoError(t, err)
	require.Len(t, change.Statements(), 2)
	again, err := ParseRowSecurityChange(strings.Join(change.Statements(), ";\n"))
	require.NoError(t, err)
	assert.Equal(t, change.CanonicalSQL(), again.CanonicalSQL())
}

func TestRowSecurityNamespaceComparisonRevalidatesCanonicalShape(t *testing.T) {
	for _, sql := range []string{
		`DROP POLICY readers ON other.documents`,
		`CREATE TABLE public.documents (id int)`,
		`COMMENT ON TABLE public.documents IS 'not a policy'`,
	} {
		t.Run(sql, func(t *testing.T) {
			change := RowSecurityChange{schema: "public", table: "documents", canonical: []string{sql}}
			_, err := change.CanonicalSQLForNamespace("public", "documents")
			require.ErrorIs(t, err, ErrRowSecurityChange)
		})
	}
}
