package schemadiff

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrDesiredStatementRejected is returned when the server rejects one of
// the desired file's own statements while the planner builds the desired
// table on the scratch schema: the statement names a column, function,
// type, role, schema, or collation that does not exist, puts an expression
// where the server does not accept it, collides with another name once the
// server truncates it, and the like. Each is fixed by editing the file, and
// planning the same file again gets the same answer. A failure that is not
// the statement's, such as a lost connection, a cancelled or timed-out
// statement, a missing privilege, or an exhausted resource, is returned as
// it came, without this sentinel.
var ErrDesiredStatementRejected = errors.New("the server rejected a statement of the desired file")

// DesiredStatementError carries a desired statement the server rejected on
// the scratch schema, and the server's error. It wraps both
// ErrDesiredStatementRejected and the *pgconn.PgError.
type DesiredStatementError struct {
	// Statement is the desired statement as it ran.
	Statement string
	// Err is the server's error.
	Err *pgconn.PgError
}

func (e *DesiredStatementError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("%s, in: %s", ErrDesiredStatementRejected, statementExcerpt(e.Statement, 0))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (SQLSTATE %s)", ErrDesiredStatementRejected, e.Err.Message, e.Err.Code)
	if e.Err.Hint != "" {
		fmt.Fprintf(&b, "; hint: %s", e.Err.Hint)
	}
	fmt.Fprintf(&b, ", in: %s", statementExcerpt(e.Statement, int(e.Err.Position)))
	return b.String()
}

func (e *DesiredStatementError) Unwrap() []error {
	return []error{ErrDesiredStatementRejected, e.Err}
}

// desiredStatementRejection returns err as a *DesiredStatementError when the
// server rejected the desired statement sql for what the statement says, and
// nil for any other failure.
func desiredStatementRejection(sql string, err error) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || !rejectsTheStatement(pgErr.Code) {
		return nil
	}
	return &DesiredStatementError{Statement: sql, Err: pgErr}
}

// rejectsTheStatement reports whether a SQLSTATE is the server's answer to
// what a statement says rather than to the session it ran in: a feature the
// server does not support (0A), a value it cannot take (22), a schema that
// does not exist (3F), a syntax or access rule the statement breaks (42), or
// a limit the statement exceeds (54). A missing privilege is in class 42 but
// is fixed by a grant, not by editing the file, so it is not one.
func rejectsTheStatement(code string) bool {
	if len(code) != 5 || code == sqlstateInsufficientPrivilege {
		return false
	}
	switch code[:2] {
	case "0A", "22", "3F", "42", "54":
		return true
	}
	return false
}

// statementExcerptRunes bounds the statement text a rejection carries, so a
// long CREATE TABLE does not crowd out the server's message.
const statementExcerptRunes = 160

// statementExcerpt returns the statement on one line, with runs of
// whitespace collapsed. A statement longer than statementExcerptRunes is cut
// to that many runes around position, the server's 1-based character offset
// of the error in the statement, or from the start when position is 0, and
// each cut end is marked with an ellipsis.
func statementExcerpt(sql string, position int) string {
	runes := []rune(strings.Join(strings.Fields(sql), " "))
	if len(runes) <= statementExcerptRunes {
		return string(runes)
	}
	// Collapsing whitespace moves the server's offset, so the window is
	// placed on the original text and collapsed afterwards.
	original := []rune(sql)
	start := 0
	if position > 0 {
		start = max(0, min(position-1-statementExcerptRunes/2, len(original)-statementExcerptRunes))
	}
	end := min(len(original), start+statementExcerptRunes)
	excerpt := strings.Join(strings.Fields(string(original[start:end])), " ")
	if start > 0 {
		excerpt = "…" + excerpt
	}
	if end < len(original) {
		excerpt += "…"
	}
	return excerpt
}
