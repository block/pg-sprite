package statement

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A file the parser cannot read names the line and column the parser stopped
// at, and that line's text, so its author goes straight to the typo instead
// of searching the file for the token the message quotes.
func TestParseDesiredFileLocatesASyntaxError(t *testing.T) {
	sql := `CREATE TABLE coupons (
  id bigint PRIMARY KEY,
  code text NOT NULL
  expires_at timestamptz
);
`
	_, err := ParseDesiredFile(sql)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrParseDesired)

	syntax, ok := errors.AsType[*SyntaxError](err)
	require.True(t, ok, "the error locates the syntax error: %v", err)
	assert.Equal(t, `syntax error at or near "expires_at"`, syntax.Message)
	assert.Equal(t, 4, syntax.Line)
	assert.Equal(t, 3, syntax.Column)
	assert.Equal(t, "  expires_at timestamptz", syntax.SourceLine)
	assert.Equal(t, `parse desired schema: line 4, column 3: syntax error at or near "expires_at"`, err.Error())
}

// Every desired-file entry point locates a syntax error the same way.
func TestEveryDesiredEntryPointLocatesASyntaxError(t *testing.T) {
	const sql = "CREATE TABLE t (\n  id int,,\n);"
	parse := map[string]func(string) error{
		"ParseDesiredFile":            func(s string) error { _, err := ParseDesiredFile(s); return err },
		"ParseDesired":                func(s string) error { _, err := ParseDesired(s); return err },
		"ParseDesiredWithRowSecurity": func(s string) error { _, err := ParseDesiredWithRowSecurity(s); return err },
		"HasRowSecurityDeclaration":   func(s string) error { _, err := HasRowSecurityDeclaration(s); return err },
	}
	for name, fn := range parse {
		t.Run(name, func(t *testing.T) {
			err := fn(sql)
			assert.ErrorIs(t, err, ErrParseDesired)
			syntax, ok := errors.AsType[*SyntaxError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 2, syntax.Line)
			assert.Equal(t, 10, syntax.Column)
		})
	}
}

func TestCursorLocation(t *testing.T) {
	testCases := []struct {
		name         string
		source       string
		cursor       int
		line, column int
		text         string
	}{
		{"first character", "SELECT $", 1, 1, 1, "SELECT $"},
		{"on the first line", "SELECT $", 8, 1, 8, "SELECT $"},
		{"after a line break", "CREATE TABLE t (\n  id int,,\n);", 27, 2, 10, "  id int,,"},
		{"past the end of the input", "CREATE TABLE t (", 17, 1, 17, "CREATE TABLE t ("},
		{"past the end after a trailing line break", "CREATE TABLE t (\n", 18, 2, 1, ""},
		{"counts characters, not bytes", "-- café\nSELECT $", 16, 2, 8, "SELECT $"},
		{"a CRLF line keeps no carriage return", "SELECT 1,\r\n,", 12, 2, 1, ","},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			line, column, text := cursorLocation(tc.source, tc.cursor)
			assert.Equal(t, tc.line, line)
			assert.Equal(t, tc.column, column)
			assert.Equal(t, tc.text, text)
		})
	}
}

// The parser counts its cursor in characters, so a line after multibyte text
// still reports the column an editor shows.
func TestParseDesiredFileSyntaxErrorColumnCountsCharacters(t *testing.T) {
	_, err := ParseDesiredFile("CREATE TABLE t (note text DEFAULT 'café ünïcode', id int,,);")
	syntax, ok := errors.AsType[*SyntaxError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, 1, syntax.Line)
	assert.Equal(t, 58, syntax.Column)
}
