package statement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func statementSQL(statements []Statement) []string {
	out := make([]string, 0, len(statements))
	for _, st := range statements {
		out = append(out, st.SQL())
	}
	return out
}

// A table with its indexes and no row security is admitted through
// ParseDesired's front door, with the same statements ParseDesired returns.
func TestParseDesiredFileAdmitsATableThroughTheOrdinaryFrontDoor(t *testing.T) {
	sql := `
CREATE INDEX documents_owner_idx ON documents (owner_id);
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    owner_id uuid NOT NULL
);
`
	file, err := ParseDesiredFile(sql)
	require.NoError(t, err)

	want, err := ParseDesired(sql)
	require.NoError(t, err)
	schema, ok := file.Schema()
	require.True(t, ok)
	_, isRowSecurity := file.RowSecurity()
	assert.False(t, isRowSecurity)
	assert.Equal(t, "documents", file.Table())
	assert.Equal(t, statementSQL(want.Statements()), statementSQL(schema.Statements()))
}

// A table that declares row security is admitted through
// ParseDesiredWithRowSecurity's front door, with the same statements it returns.
func TestParseDesiredFileAdmitsRowSecurityThroughItsFrontDoor(t *testing.T) {
	sql := `
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    owner_id uuid NOT NULL
);
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
CREATE POLICY "Read own documents" ON documents
    FOR SELECT TO authenticated
    USING (owner_id = (SELECT auth.uid()));
COMMENT ON POLICY "Read own documents" ON documents IS 'Owner access';
`
	file, err := ParseDesiredFile(sql)
	require.NoError(t, err)

	want, err := ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	rowSecurity, ok := file.RowSecurity()
	require.True(t, ok)
	_, isSchema := file.Schema()
	assert.False(t, isSchema)
	assert.Equal(t, "documents", file.Table())
	assert.Equal(t, statementSQL(want.Statements()), statementSQL(rowSecurity.Statements()))
}

// Each refusal comes from the front door the file selects, so a row-security
// file is judged by the row-security rules and an ordinary file by
// ParseDesired's.
func TestParseDesiredFileRefusesWithTheSelectedFrontDoorsError(t *testing.T) {
	t.Run("NO FORCE without ENABLE or DISABLE is an incomplete row-security declaration", func(t *testing.T) {
		// ParseDesired would refuse the ALTER as a disallowed statement; the
		// row-security rules name the actual problem.
		_, err := ParseDesiredFile(`
CREATE TABLE documents (id bigint PRIMARY KEY);
ALTER TABLE documents NO FORCE ROW LEVEL SECURITY;
`)
		require.ErrorIs(t, err, ErrRowSecurityDeclaration)
		assert.NotErrorIs(t, err, ErrDisallowedStatement)
	})

	t.Run("a policy comment without its table needs a table definition", func(t *testing.T) {
		_, err := ParseDesiredFile(`COMMENT ON POLICY "Read own documents" ON documents IS 'Owner access';`)
		require.ErrorIs(t, err, ErrRowSecurityTableDefinitionRequired)
	})

	t.Run("an ALTER that is not about row security is refused by the ordinary front door", func(t *testing.T) {
		_, err := ParseDesiredFile(`
CREATE TABLE documents (id bigint PRIMARY KEY);
ALTER TABLE documents ADD COLUMN title text;
`)
		require.ErrorIs(t, err, ErrDisallowedStatement)
	})

	t.Run("invalid SQL is a parse error", func(t *testing.T) {
		_, err := ParseDesiredFile(`CREATE TABLE documents (`)
		require.ErrorContains(t, err, "parse desired schema")
	})
}

// The zero DesiredFile admitted nothing, so it offers no planner input.
func TestZeroDesiredFileOffersNoPlannerInput(t *testing.T) {
	var file DesiredFile
	_, isSchema := file.Schema()
	_, isRowSecurity := file.RowSecurity()
	assert.False(t, isSchema)
	assert.False(t, isRowSecurity)
}
