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

	t.Run("invalid SQL is a parse error, not an admission refusal", func(t *testing.T) {
		_, err := ParseDesiredFile(`CREATE TABLE documents (`)
		require.ErrorIs(t, err, ErrParseDesired)
		assert.NotErrorIs(t, err, ErrDisallowedStatement)
	})
}

// Every desired-file entry point marks a syntax error with the same sentinel,
// so a caller can branch on it whichever function it calls.
func TestDesiredEntryPointsMarkSyntaxErrorsWithErrParseDesired(t *testing.T) {
	const invalid = `CREATE TABLE documents (`
	_, err := ParseDesired(invalid)
	assert.ErrorIs(t, err, ErrParseDesired, "ParseDesired")
	_, err = ParseDesiredWithRowSecurity(invalid)
	assert.ErrorIs(t, err, ErrParseDesired, "ParseDesiredWithRowSecurity")
	_, err = HasRowSecurityDeclaration(invalid)
	assert.ErrorIs(t, err, ErrParseDesired, "HasRowSecurityDeclaration")
	_, err = ParseDesiredFile(invalid)
	assert.ErrorIs(t, err, ErrParseDesired, "ParseDesiredFile")
}

// An admission refusal carries no parse sentinel, so a caller can tell a file
// that is not SQL from a file whose SQL a front door refuses.
func TestParseDesiredFileAdmissionRefusalIsNotAParseError(t *testing.T) {
	_, err := ParseDesiredFile(`SELECT 1`)
	require.ErrorIs(t, err, ErrDisallowedStatement)
	assert.NotErrorIs(t, err, ErrParseDesired)
}

// Statements reports the admitted statements of whichever front door the file
// selected, so a caller that needs only the statements makes one call.
func TestDesiredFileStatementsComeFromTheSelectedFrontDoor(t *testing.T) {
	t.Run("ordinary file", func(t *testing.T) {
		sql := `
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    owner_id uuid NOT NULL
);
CREATE INDEX documents_owner_idx ON documents (owner_id);
`
		file, err := ParseDesiredFile(sql)
		require.NoError(t, err)
		want, err := ParseDesired(sql)
		require.NoError(t, err)
		assert.Equal(t, statementSQL(want.Statements()), statementSQL(file.Statements()))
	})

	t.Run("row-security file", func(t *testing.T) {
		sql := `
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    owner_id uuid NOT NULL
);
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
CREATE POLICY "Read own documents" ON documents
    FOR SELECT TO authenticated
    USING (owner_id = (SELECT auth.uid()));
`
		file, err := ParseDesiredFile(sql)
		require.NoError(t, err)
		want, err := ParseDesiredWithRowSecurity(sql)
		require.NoError(t, err)
		assert.Equal(t, statementSQL(want.Statements()), statementSQL(file.Statements()))
	})

	t.Run("the returned slice is a copy", func(t *testing.T) {
		file, err := ParseDesiredFile(`CREATE TABLE documents (id bigint PRIMARY KEY)`)
		require.NoError(t, err)
		statements := file.Statements()
		statements[0] = Statement{}
		assert.Equal(t, "CREATE TABLE documents (id bigint PRIMARY KEY)", file.Statements()[0].SQL())
	})
}

// routeViaTwoDoors is the dispatch ParseDesiredFile replaces: try the ordinary
// front door, and fall back to the row-security door only when the file
// declares row security. It is the oracle for ParseDesiredFile's routing.
func routeViaTwoDoors(sql string) (rowSecurity bool, table string, statements []string, err error) {
	ds, err := ParseDesired(sql)
	if err == nil {
		return false, ds.Table(), statementSQL(ds.Statements()), nil
	}
	if has, detectErr := HasRowSecurityDeclaration(sql); detectErr == nil && has {
		rs, rsErr := ParseDesiredWithRowSecurity(sql)
		if rsErr != nil {
			return false, "", nil, rsErr
		}
		return true, rs.Table(), statementSQL(rs.Statements()), nil
	}
	return false, "", nil, err
}

// ParseDesiredFile routes every desired-file shape exactly as trying
// ParseDesired and then the row-security door does: the same admitted
// statements, or the same refusal. Empty, comment-only, non-DDL and mixed
// files must be refused here, not returned as a zero DesiredFile, because a
// library caller has no fallback that would catch a nil error with no table.
func TestParseDesiredFileRoutesLikeTheTwoDoors(t *testing.T) {
	const enable = "ALTER TABLE t ENABLE ROW LEVEL SECURITY"
	for name, sql := range map[string]string{
		"empty":                        "",
		"comment only":                 "-- nothing here\n/* nor here */",
		"table":                        "CREATE TABLE t (id bigint PRIMARY KEY)",
		"index before table":           "CREATE INDEX i ON t (a); CREATE TABLE t (id bigint, a int)",
		"alter only":                   "ALTER TABLE t ADD COLUMN a int",
		"table then alter":             "CREATE TABLE t (id int); ALTER TABLE t ADD COLUMN a int",
		"non-DDL":                      "SELECT 1",
		"two tables":                   "CREATE TABLE a (id int); CREATE TABLE b (id int)",
		"index without table":          "CREATE INDEX i ON t (a)",
		"row security":                 "CREATE TABLE t (id int); " + enable + "; CREATE POLICY p ON t USING (true)",
		"row security first":           enable + "; CREATE TABLE t (id int)",
		"policy without enable":        "CREATE TABLE t (id int); CREATE POLICY p ON t USING (true)",
		"row security and alter":       "CREATE TABLE t (id int); " + enable + "; ALTER TABLE t ADD COLUMN a int",
		"row security and view":        "CREATE TABLE t (id int); " + enable + "; CREATE VIEW v AS SELECT 1",
		"row security and two tables":  "CREATE TABLE t (id int); CREATE TABLE u (id int); " + enable,
		"row security and foreign key": "CREATE TABLE t (id int REFERENCES u (id)); " + enable,
		"row security without table":   enable,
		"policy comment only":          "COMMENT ON POLICY p ON t IS 'x'",
		"syntax error":                 "CREATE TABLE t (",
	} {
		t.Run(name, func(t *testing.T) {
			wantRLS, wantTable, wantStatements, wantErr := routeViaTwoDoors(sql)
			file, err := ParseDesiredFile(sql)
			if wantErr != nil {
				require.EqualError(t, err, wantErr.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, wantTable, file.Table())
			if wantRLS {
				rs, ok := file.RowSecurity()
				require.True(t, ok)
				assert.Equal(t, wantStatements, statementSQL(rs.Statements()))
				return
			}
			ds, ok := file.Schema()
			require.True(t, ok)
			assert.Equal(t, wantStatements, statementSQL(ds.Statements()))
		})
	}
}

// The zero DesiredFile admitted nothing, so it offers no planner input.
func TestZeroDesiredFileOffersNoPlannerInput(t *testing.T) {
	var file DesiredFile
	_, isSchema := file.Schema()
	_, isRowSecurity := file.RowSecurity()
	_, isEnumType := file.EnumType()
	assert.False(t, isSchema)
	assert.False(t, isRowSecurity)
	assert.False(t, isEnumType)
	assert.Empty(t, file.Statements())
}

// FormatDesired refuses a commented row-security file with ErrCommentLoss and
// no text. ParseDesiredFile alone admits this file and drops the comment, so
// the refusal comes from FormatDesired's own comment check: an importer that
// formats through it cannot lose the comment.
func TestFormatDesiredRefusesACommentedRowSecurityFile(t *testing.T) {
	sql := `
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    team_id bigint NOT NULL -- owning team
);
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
CREATE POLICY documents_team_read ON documents
    FOR SELECT
    USING (team_id = 1);
`
	formatted, err := FormatDesired(sql)
	require.ErrorIs(t, err, ErrCommentLoss)
	assert.Empty(t, formatted)
}

// Input that is not valid SQL is a parse refusal, as ParseDesiredFile reports
// it, even when the lexer is the first to see it: an unterminated quote or
// comment fails the comment check's scan before the parser runs.
func TestFormatDesiredReportsUnterminatedInputAsAParseRefusal(t *testing.T) {
	for _, sql := range []string{
		"CREATE TABLE t (note text DEFAULT 'oops);",
		"CREATE TABLE t (id int) /* oops",
	} {
		_, parseErr := ParseDesiredFile(sql)
		require.ErrorIs(t, parseErr, ErrParseDesired)
		formatted, err := FormatDesired(sql)
		assert.ErrorIs(t, err, ErrParseDesired, sql)
		assert.Empty(t, formatted)
	}
}

// formatDesiredRowSecurityFile declares a table, an index, a policy and its
// comment, and row security, in an order FormatDesired does not keep.
const formatDesiredRowSecurityFile = `
CREATE TABLE documents (
    id bigint PRIMARY KEY,
    team_id bigint NOT NULL
);
CREATE INDEX documents_team_id_idx ON documents (team_id);
CREATE POLICY documents_team_read ON documents
    FOR SELECT
    USING (team_id = 1);
COMMENT ON POLICY documents_team_read ON documents IS 'team reads';
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
`

// FormatDesired prints the admitted statements in the order Statements
// reports them, each followed by ";\n", keeps the policy's COMMENT ON
// statement as content, and its output formats to itself.
func TestFormatDesiredPrintsCanonicalTextAndIsIdempotent(t *testing.T) {
	want := "CREATE TABLE documents (id bigint PRIMARY KEY, team_id bigint NOT NULL);\n" +
		"CREATE INDEX documents_team_id_idx ON documents USING btree (team_id);\n" +
		"CREATE POLICY documents_team_read ON documents FOR SELECT TO public USING (team_id = 1);\n" +
		"ALTER TABLE documents ENABLE ROW LEVEL SECURITY;\n" +
		"COMMENT ON POLICY documents_team_read ON documents IS 'team reads';\n"
	first, err := FormatDesired(formatDesiredRowSecurityFile)
	require.NoError(t, err)
	assert.Equal(t, want, first)

	second, err := FormatDesired(first)
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

// The comment check runs before any admission rule: a commented file the
// front door would also refuse, for its second CREATE TABLE, is reported as
// comment loss.
func TestFormatDesiredChecksCommentsBeforeAdmission(t *testing.T) {
	formatted, err := FormatDesired(`
CREATE TABLE a (id int); -- two tables
CREATE TABLE b (id int);
`)
	require.ErrorIs(t, err, ErrCommentLoss)
	assert.Empty(t, formatted)
}
