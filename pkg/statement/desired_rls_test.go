package statement

import (
	"testing"

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
