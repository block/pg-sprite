package statement

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file holding one CREATE TYPE ... AS ENUM is admitted through the enum
// type front door: it offers the type as planner input, its labels in
// declared order, and no table.
func TestParseDesiredFileAdmitsAnEnumTypeFile(t *testing.T) {
	file, err := ParseDesiredFile("-- states a run moves through\nCREATE TYPE run_status AS ENUM ('pending', 'running', 'it''s done');\n")
	require.NoError(t, err)

	enumType, ok := file.EnumType()
	require.True(t, ok)
	_, isSchema := file.Schema()
	_, isRowSecurity := file.RowSecurity()
	assert.False(t, isSchema)
	assert.False(t, isRowSecurity)
	assert.Empty(t, file.Table())
	assert.Equal(t, "run_status", enumType.Name())
	assert.Equal(t, []string{"pending", "running", "it's done"}, enumType.Labels())
	want := []string{"CREATE TYPE run_status AS ENUM ('pending', 'running', 'it''s done')"}
	assert.Equal(t, want, statementSQL(enumType.Statements()))
	assert.Equal(t, want, statementSQL(file.Statements()))

	labels := enumType.Labels()
	labels[0] = "mutated"
	assert.Equal(t, "pending", enumType.Labels()[0], "Labels returns a copy")
}

// An enum type with no labels is a valid type, so its file is admitted.
func TestParseDesiredFileAdmitsAnEnumTypeWithNoLabels(t *testing.T) {
	file, err := ParseDesiredFile("CREATE TYPE placeholder AS ENUM ()")
	require.NoError(t, err)
	enumType, ok := file.EnumType()
	require.True(t, ok)
	assert.Empty(t, enumType.Labels())
}

// An enum type file holds its CREATE TYPE and nothing else. Anything beside
// it, a table, row security, or a second type, is refused with the
// statement it names, as is a qualified name or a label the plan could not
// name.
func TestParseDesiredFileRefusesAnEnumTypeFileThatHoldsMore(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		err  error
		stmt string
	}{
		{
			name: "a table after the type",
			sql:  "CREATE TYPE status AS ENUM ('a'); CREATE TABLE t (s status)",
			err:  ErrEnumTypeAlone,
			stmt: "CREATE TABLE t (s status)",
		},
		{
			name: "a table before the type",
			sql:  "CREATE TABLE t (id int); CREATE TYPE status AS ENUM ('a')",
			err:  ErrEnumTypeAlone,
			stmt: "CREATE TABLE t (id int)",
		},
		{
			name: "a second type",
			sql:  "CREATE TYPE a AS ENUM ('x'); CREATE TYPE b AS ENUM ('y')",
			err:  ErrEnumTypeAlone,
			stmt: "CREATE TYPE b AS ENUM ('y')",
		},
		{
			name: "row security",
			sql:  "CREATE TYPE status AS ENUM ('a'); ALTER TABLE t ENABLE ROW LEVEL SECURITY",
			err:  ErrEnumTypeAlone,
			stmt: "ALTER TABLE t ENABLE ROW LEVEL SECURITY",
		},
		{name: "a qualified name", sql: "CREATE TYPE app.status AS ENUM ('a')", err: ErrQualifiedName},
		{name: "a label listed twice", sql: "CREATE TYPE status AS ENUM ('a', 'b', 'a')", err: ErrEnumLabel},
		{name: "the empty label", sql: "CREATE TYPE status AS ENUM ('a', '')", err: ErrEnumLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDesiredFile(tc.sql)
			require.ErrorIs(t, err, tc.err)
			assert.NotErrorIs(t, err, ErrParseDesired)
			if tc.stmt == "" {
				return
			}
			extra, ok := errors.AsType[*EnumTypeFileError](err)
			require.True(t, ok, err.Error())
			assert.Equal(t, tc.stmt, extra.SQL)
		})
	}
}

// FormatDesired prints an enum type file's one statement canonically, and
// the text formats to itself.
func TestFormatDesiredPrintsAnEnumTypeFile(t *testing.T) {
	out, err := FormatDesired("create type   delivery_method as enum ('webhook','websocket');")
	require.NoError(t, err)
	assert.Equal(t, "CREATE TYPE delivery_method AS ENUM ('webhook', 'websocket');\n", out)
	again, err := FormatDesired(out)
	require.NoError(t, err)
	assert.Equal(t, out, again)
}
