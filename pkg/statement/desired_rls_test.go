package statement

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDesiredWithRowSecurityRequiresExplicitSetting(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint PRIMARY KEY)`)
	require.ErrorIs(t, err, ErrRowSecurityDeclaration)
}

func TestParseDesiredWithRowSecurityAdmitsCompleteDefinition(t *testing.T) {
	ds, err := ParseDesiredWithRowSecurity(`
 CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id uuid NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 ALTER TABLE documents FORCE ROW LEVEL SECURITY;
 CREATE POLICY "Read own documents" ON documents
     FOR SELECT TO authenticated
     USING (owner_id = (SELECT auth.uid()));
 COMMENT ON POLICY "Read own documents" ON documents IS 'Owner access';
 `)
	require.NoError(t, err)
	assert.Equal(t, "documents", ds.Table())
	require.Len(t, ds.Statements(), 5)
	// The executable desired-schema parser must keep rejecting this definition.
	_, err = ParseDesired(`CREATE TABLE documents (id bigint PRIMARY KEY);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, ErrDisallowedStatement)
}

func TestParseDesiredWithRowSecurityRejectsOtherTarget(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE other ENABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, ErrRowSecurityDeclaration)
}

func TestParseDesiredWithRowSecurityRejectsQualifiedTarget(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE public.documents ENABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, ErrQualifiedName)
}

func TestParseDesiredWithRowSecurityRejectsConflictingSettings(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, ErrRowSecurityDeclaration)
}

func TestParseDesiredWithRowSecurityRejectsUnrelatedAlter(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 ALTER TABLE documents ADD COLUMN secret text;`)
	require.ErrorIs(t, err, ErrDisallowedStatement)
}

// A policy may read another table when it names it schema-qualified, in a
// subquery, a join, or WITH CHECK alike; the statement keeps the qualified
// name, which is what live introspection prints.
func TestParseDesiredWithRowSecurityAdmitsQualifiedPolicyRelations(t *testing.T) {
	ds, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint, team_id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY members ON documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM public.team_members m JOIN public.teams t ON t.id = m.team_id
                    WHERE m.team_id = documents.team_id AND m.user_id = (SELECT auth.uid())));
 CREATE POLICY writers ON documents FOR INSERT
     WITH CHECK (team_id IN (SELECT team_id FROM public.team_members WHERE user_id = (SELECT auth.uid())));`)
	require.NoError(t, err)
	statements := ds.Statements()
	require.Len(t, statements, 4)
	assert.Contains(t, statements[2].SQL(), "FROM public.team_members m JOIN public.teams t")
	assert.Contains(t, statements[3].SQL(), "FROM public.team_members WHERE")
}

func TestParseDesiredWithRowSecurityRefusesPolicyRelations(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		want error
		read string
	}{
		"unqualified table": {
			expr: "USING (EXISTS (SELECT 1 FROM memberships WHERE active))",
			want: ErrPolicyRelationDependency, read: `"memberships"`,
		},
		"unqualified table in WITH CHECK": {
			expr: "WITH CHECK (EXISTS (SELECT 1 FROM public.teams JOIN memberships USING (id)))",
			want: ErrPolicyRelationDependency, read: `"memberships"`,
		},
		"CTE name": {
			expr: "USING (EXISTS (WITH m AS (SELECT 1 FROM public.memberships) SELECT 1 FROM m))",
			want: ErrPolicyRelationDependency, read: `"m"`,
		},
		"database-qualified table": {
			expr: "USING (EXISTS (SELECT 1 FROM app.public.memberships))",
			want: ErrPolicyRelationDependency, read: `"app.public.memberships"`,
		},
		"its own table": {
			expr: "USING (EXISTS (SELECT 1 FROM public.documents d WHERE d.id = documents.id))",
			want: ErrPolicyReadsOwnTable, read: `"public.documents"`,
		},
		"its own name in another schema": {
			expr: "USING (EXISTS (SELECT 1 FROM archive.documents))",
			want: ErrPolicyReadsOwnTable, read: `"archive.documents"`,
		},
		"a catalog table": {
			expr: "USING (EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname = 'app'))",
			want: ErrPolicyReadsCatalog, read: `"pg_catalog.pg_namespace"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents ` + tc.expr + `;`)
			require.ErrorIs(t, err, tc.want)
			assert.ErrorContains(t, err, `policy "readers" reads `+tc.read)
		})
	}
}

// A policy whose query locks the rows it reads is refused, whichever lock
// strength it names and however deep the query sits, because building it
// takes ROW SHARE on the table rather than the ACCESS SHARE of a plain read.
func TestParseDesiredWithRowSecurityRefusesPolicyRowLocks(t *testing.T) {
	for name, expr := range map[string]string{
		"FOR SHARE":         "USING (EXISTS (SELECT 1 FROM public.team_members FOR SHARE))",
		"FOR UPDATE":        "USING (EXISTS (SELECT 1 FROM public.team_members FOR UPDATE))",
		"FOR NO KEY UPDATE": "USING (EXISTS (SELECT 1 FROM public.team_members FOR NO KEY UPDATE))",
		"FOR KEY SHARE":     "USING (EXISTS (SELECT 1 FROM public.team_members FOR KEY SHARE))",
		"nested in WITH CHECK": "WITH CHECK (id IN (SELECT id FROM public.teams WHERE EXISTS " +
			"(SELECT 1 FROM public.team_members FOR SHARE OF team_members)))",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents ` + expr + `;`)
			require.ErrorIs(t, err, ErrPolicyLocksRows)
			assert.ErrorContains(t, err, `policy "readers" locks the rows it reads`)
		})
	}
}

func TestParseDesiredWithRowSecurityRetainsExplicitDisabledScope(t *testing.T) {
	ds, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	require.Len(t, ds.Statements(), 2)
	copy := ds.Statements()
	copy[0] = Statement{}
	assert.Equal(t, KindCreateTable, ds.Statements()[0].Kind())
}

func TestHasRowSecurityDeclarationUsesGrammar(t *testing.T) {
	for _, sql := range []string{
		`CREATE TABLE t (id bigint); CREATE VIEW v AS SELECT 1;`,
		`CREATE TABLE t (id bigint); GRANT SELECT ON t TO PUBLIC;`,
		`CREATE TABLE t (id bigint); DROP TABLE t;`,
		`CREATE TABLE t (id bigint); ALTER TABLE t ADD COLUMN title text;`,
		`CREATE TABLE t (title text DEFAULT 'ENABLE ROW LEVEL SECURITY');`,
	} {
		present, err := HasRowSecurityDeclaration(sql)
		require.NoError(t, err)
		assert.False(t, present, sql)
	}
	for _, sql := range []string{
		`ALTER TABLE t ENABLE ROW LEVEL SECURITY;`,
		`ALTER TABLE t DISABLE ROW LEVEL SECURITY;`,
		`ALTER TABLE t FORCE ROW LEVEL SECURITY;`,
		`ALTER TABLE t NO FORCE ROW LEVEL SECURITY;`,
		`CREATE POLICY readers ON t USING (true);`,
		`COMMENT ON POLICY readers ON t IS 'Readers';`,
	} {
		present, err := HasRowSecurityDeclaration(sql)
		require.NoError(t, err)
		assert.True(t, present, sql)
	}
}

func TestRelationWalkHandlesMessageMaps(t *testing.T) {
	value, err := structpb.NewStruct(map[string]any{"nested": map[string]any{"value": "no relation"}})
	require.NoError(t, err)
	assert.Empty(t, appendRelations(nil, value.ProtoReflect()))
}

func TestDesiredRowSecurityRequiresTableDefinition(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (true);`)
	require.ErrorIs(t, err, ErrRowSecurityDeclaration)
	require.ErrorIs(t, err, ErrRowSecurityTableDefinitionRequired)
}
