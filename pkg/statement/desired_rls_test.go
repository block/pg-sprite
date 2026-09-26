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

func TestParseDesiredWithRowSecurityRejectsPolicyRelationDependency(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`CREATE TABLE documents (id bigint);
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM public.memberships WHERE active));`)
	require.ErrorIs(t, err, ErrPolicyRelationDependency)
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
	assert.False(t, messageReadsRelation(value.ProtoReflect()))
}

func TestDesiredRowSecurityRequiresTableDefinition(t *testing.T) {
	_, err := ParseDesiredWithRowSecurity(`ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (true);`)
	require.ErrorIs(t, err, ErrRowSecurityDeclaration)
	require.ErrorIs(t, err, ErrRowSecurityTableDefinitionRequired)
}
