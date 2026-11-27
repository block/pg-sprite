package statement

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wasilibs/go-pgquery/parser"
)

// SyntaxError is a desired file the PostgreSQL parser could not read, located
// at the point the parser stopped, so a caller can point the file's author at
// the line and column to fix. It is always wrapped with [ErrParseDesired].
type SyntaxError struct {
	// Message is the parser's own description, such as
	// `syntax error at or near "expires_at"`.
	Message string
	// Line and Column locate where the parser stopped, both 1-based, the way an
	// editor counts: Column counts characters, not bytes. At the end of the
	// input they point just past its last character.
	Line, Column int
	// SourceLine is the text of that line, without its line break.
	SourceLine string

	err error
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Message)
}

// Unwrap returns the parser's error.
func (e *SyntaxError) Unwrap() error { return e.err }

// parseFailure is the error for source the parser rejected with err: a
// [SyntaxError] located in source when the parser said where it stopped, and
// the parser's error alone when it did not, both under [ErrParseDesired].
func parseFailure(source string, err error) error {
	parsed, ok := errors.AsType[*parser.Error](err)
	if !ok || parsed.Cursorpos < 1 {
		return fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	line, column, text := cursorLocation(source, parsed.Cursorpos)
	return fmt.Errorf("%w: %w", ErrParseDesired, &SyntaxError{
		Message:    parsed.Message,
		Line:       line,
		Column:     column,
		SourceLine: text,
		err:        err,
	})
}

// cursorLocation turns the parser's cursor, a 1-based character offset into
// source, into the 1-based line and column it falls on and that line's text.
// A cursor past the end, as "syntax error at end of input" reports, lands just
// after the last character.
func cursorLocation(source string, cursor int) (line, column int, text string) {
	runes := []rune(source)
	at := min(cursor-1, len(runes))
	line, start := 1, 0
	for i := range at {
		if runes[i] == '\n' {
			line++
			start = i + 1
		}
	}
	end := start
	for end < len(runes) && runes[end] != '\n' {
		end++
	}
	return line, at - start + 1, strings.TrimRight(string(runes[start:end]), "\r")
}
