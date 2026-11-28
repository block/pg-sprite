package statement

import (
	"errors"
	"strings"
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

// An enum type file holds its CREATE TYPE and nothing else. A statement
// another desired file holds, such as row security or a second type, is
// refused as belonging in a file of its own. A statement no desired file
// holds is refused as a table file refuses it, naming the object it
// declares, so moving it to a file of its own never meets a second refusal.
// A qualified name or a label the plan could not name is refused too.
func TestParseDesiredFileRefusesAnEnumTypeFileThatHoldsMore(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		err    error
		stmt   string
		object DeclaredObject
	}{
		{
			name:   "a second type",
			sql:    "CREATE TYPE a AS ENUM ('x'); CREATE TYPE b AS ENUM ('y')",
			err:    ErrEnumTypeAlone,
			stmt:   "CREATE TYPE b AS ENUM ('y')",
			object: DeclaredEnumType,
		},
		{
			name:   "row security",
			sql:    "CREATE TYPE status AS ENUM ('a'); ALTER TABLE t ENABLE ROW LEVEL SECURITY",
			err:    ErrEnumTypeAlone,
			stmt:   "ALTER TABLE t ENABLE ROW LEVEL SECURITY",
			object: DeclaredRowSecurity,
		},
		{
			name:   "a label rename",
			sql:    "CREATE TYPE mood AS ENUM ('a'); ALTER TYPE mood RENAME VALUE 'a' TO 'c'",
			err:    ErrDisallowedStatement,
			stmt:   "ALTER TYPE mood RENAME VALUE 'a' TO 'c'",
			object: DeclaredEnumLabel,
		},
		{
			// The parse tree spells the empty old label as no label, so
			// the canonical text would name an ADD VALUE.
			name:   "a rename from the empty label",
			sql:    "CREATE TYPE mood AS ENUM ('a');\nALTER TYPE mood RENAME VALUE '' TO 'blue';\n",
			err:    ErrDisallowedStatement,
			stmt:   "ALTER TYPE mood RENAME VALUE '' TO 'blue'",
			object: DeclaredEnumLabel,
		},
		{
			name:   "a comment on the type",
			sql:    "CREATE TYPE mood AS ENUM ('a'); COMMENT ON TYPE mood IS 'how it went'",
			err:    ErrDisallowedStatement,
			stmt:   "COMMENT ON TYPE mood IS 'how it went'",
			object: DeclaredComment,
		},
		{
			name:   "a view",
			sql:    "CREATE TYPE mood AS ENUM ('a'); CREATE VIEW moods AS SELECT 1",
			err:    ErrDisallowedStatement,
			stmt:   "CREATE VIEW moods AS SELECT 1",
			object: DeclaredView,
		},
		{name: "a qualified name", sql: "CREATE TYPE app.status AS ENUM ('a')", err: ErrQualifiedName},
		{name: "a label listed twice", sql: "CREATE TYPE status AS ENUM ('a', 'b', 'a')", err: ErrEnumLabel},
		{name: "the empty label", sql: "CREATE TYPE status AS ENUM ('a', '')", err: ErrEnumLabel},
		{name: "a label longer than 63 bytes", sql: "CREATE TYPE status AS ENUM ('a', '" + strings.Repeat("x", 64) + "')", err: ErrEnumLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDesiredFile(tc.sql)
			require.ErrorIs(t, err, tc.err)
			assert.NotErrorIs(t, err, ErrParseDesired)
			if tc.stmt == "" {
				return
			}
			if errors.Is(tc.err, ErrDisallowedStatement) {
				disallowed, ok := errors.AsType[*DisallowedStatementError](err)
				require.True(t, ok, err.Error())
				assert.Equal(t, tc.stmt, disallowed.SQL)
				assert.Equal(t, tc.object, disallowed.Object)
				assert.NotErrorIs(t, err, ErrEnumTypeAlone)
				return
			}
			extra, ok := errors.AsType[*EnumTypeFileError](err)
			require.True(t, ok, err.Error())
			assert.Equal(t, tc.stmt, extra.SQL)
			assert.Equal(t, tc.object, extra.Object)
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
