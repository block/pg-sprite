package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestSplitReturnsCanonicalStatementsInOrder(t *testing.T) {
	stmts, err := statement.Split(`
		-- a comment the grammar discards
		create table t (id int);
		ALTER TABLE t
			ADD COLUMN c text;
	`)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"CREATE TABLE t (id int)",
		"ALTER TABLE t ADD COLUMN c text",
	}, stmts)
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
