package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestSplitReturnsVerbatimStatementsWithPositions(t *testing.T) {
	stmts, err := statement.Split(`-- a leading comment
create table t (id int);
ALTER TABLE t
	ADD COLUMN c text;`)
	require.NoError(t, err)
	assert.Equal(t, []statement.SourceStatement{
		{SQL: "create table t (id int)", Line: 2, Column: 1},
		{SQL: "ALTER TABLE t\n\tADD COLUMN c text", Line: 3, Column: 1},
	}, stmts)
}

// A statement's position is its first code token, not the start of the
// parser's statement range — leading comments and whitespace between
// statements stay out of the reported text and position.
func TestSplitPositionSkipsLeadingComments(t *testing.T) {
	stmts, err := statement.Split(
		"CREATE TABLE ok (id int);\n\n-- why we drop it\n/* twice */ DROP TABLE old;")
	require.NoError(t, err)
	require.Len(t, stmts, 2)
	assert.Equal(t, statement.SourceStatement{
		SQL: "DROP TABLE old", Line: 4, Column: 13,
	}, stmts[1])
}

// The script's final statement carries no length in the parse tree; its
// text still ends at its last code token, not at the end of the input.
func TestSplitFinalStatementExcludesTrailingNoise(t *testing.T) {
	stmts, err := statement.Split("DROP TABLE old ; -- gone")
	require.NoError(t, err)
	require.Len(t, stmts, 1)
	assert.Equal(t, statement.SourceStatement{
		SQL: "DROP TABLE old", Line: 1, Column: 1,
	}, stmts[0])
}

func TestSplitEmptyScriptReturnsNoStatements(t *testing.T) {
	stmts, err := statement.Split("  \n\t")
	require.NoError(t, err)
	assert.Empty(t, stmts)
}

func TestSplitParseFailureIsError(t *testing.T) {
	_, err := statement.Split("CREATE TABLE t (id int); CREATE TABEL u (id int)")
	require.Error(t, err)
}
