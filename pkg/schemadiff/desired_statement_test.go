package schemadiff

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A failure the statement itself causes is a rejection of the file; a
// failure of the session it ran in is not, and a missing privilege is fixed
// by a grant rather than by editing the file.
func TestRejectsTheStatement(t *testing.T) {
	for code, want := range map[string]bool{
		"42703": true,  // undefined column
		"42883": true,  // undefined function
		"42704": true,  // undefined object, such as a role
		"42P17": true,  // invalid object definition
		"42P07": true,  // duplicate table
		"22P02": true,  // invalid text representation
		"0A000": true,  // feature not supported
		"3F000": true,  // invalid schema name
		"54011": true,  // too many columns
		"42501": false, // insufficient privilege
		"57014": false, // query cancelled
		"55P03": false, // lock not available
		"53100": false, // disk full
		"08006": false, // connection failure
		"40P01": false, // deadlock detected
		"XX000": false, // internal error
		"":      false,
	} {
		assert.Equal(t, want, rejectsTheStatement(code), code)
	}
}

// The rejection wraps both its sentinel and the server's error, and reads as
// the server's message, its hint, and the statement that drew it.
func TestDesiredStatementRejection(t *testing.T) {
	sql := "CREATE INDEX events_legacy_idx\n    ON events (legacy)"
	pgErr := &pgconn.PgError{Code: "42703", Message: `column "legacy" does not exist`}
	err := desiredStatementRejection(sql, fmt.Errorf("exec: %w", pgErr))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDesiredStatementRejected)
	var server *pgconn.PgError
	require.True(t, errors.As(err, &server))
	assert.Equal(t, "42703", server.Code)
	var rejected *DesiredStatementError
	require.True(t, errors.As(err, &rejected))
	assert.Equal(t, sql, rejected.Statement)
	assert.Equal(t, `the server rejected a statement of the desired file: column "legacy" does not exist (SQLSTATE 42703), in: CREATE INDEX events_legacy_idx ON events (legacy)`, err.Error())

	pgErr.Hint = "Perhaps you meant to reference the column \"events.legacy_id\"."
	assert.Contains(t, err.Error(), `(SQLSTATE 42703); hint: Perhaps you meant to reference the column "events.legacy_id"., in: CREATE INDEX`)

	assert.NoError(t, desiredStatementRejection(sql, &pgconn.PgError{Code: "57014"}))
	assert.NoError(t, desiredStatementRejection(sql, errors.New("conn closed")))
}

// A long statement is cut around the server's error position, or from its
// start when the server gives none, and each cut end is marked.
func TestStatementExcerpt(t *testing.T) {
	short := "CREATE TABLE events (\n  id bigint\n)"
	assert.Equal(t, "CREATE TABLE events ( id bigint )", statementExcerpt(short, 0))

	var b strings.Builder
	b.WriteString("CREATE TABLE events (\n")
	for i := range 40 {
		fmt.Fprintf(&b, "  col_%02d text,\n", i)
	}
	b.WriteString("  note text COLLATE \"en_XX\"\n)")
	long := b.String()

	head := statementExcerpt(long, 0)
	assert.True(t, strings.HasPrefix(head, "CREATE TABLE events ( col_00 text,"), head)
	assert.True(t, strings.HasSuffix(head, "…"), head)

	position := strings.Index(long, `"en_XX"`) + 1
	around := statementExcerpt(long, position)
	assert.True(t, strings.HasPrefix(around, "…"), around)
	assert.Contains(t, around, `note text COLLATE "en_XX" )`)
	assert.False(t, strings.HasSuffix(around, "…"), around)
	assert.LessOrEqual(t, len([]rune(around)), statementExcerptRunes+2)
}

// A DesiredStatementError built without the server's error, as a caller of
// the exported type can, still reads as a rejection of its statement.
func TestDesiredStatementErrorWithoutTheServersError(t *testing.T) {
	err := &DesiredStatementError{Statement: "CREATE INDEX t_idx ON t (gone)"}
	assert.Equal(t, "the server rejected a statement of the desired file, in: CREATE INDEX t_idx ON t (gone)", err.Error())
	assert.ErrorIs(t, err, ErrDesiredStatementRejected)
}
