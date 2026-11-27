package statement

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	pgquery "github.com/wasilibs/go-pgquery"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// Refusal is one statement of a desired file that the front door
// does not admit.
type Refusal struct {
	// Statement is the statement's 1-based position in the file.
	Statement int
	// Line and Column locate the statement's first token in the file, both
	// 1-based, so a caller can point at the statement the way an editor does.
	Line, Column int
	// Err is why the statement is refused. It matches the sentinels and
	// typed errors a refusal of that statement always has, such as
	// [ErrDisallowedStatement] and [*DisallowedStatementError].
	Err error
}

func (r Refusal) Error() string {
	return fmt.Sprintf("statement %d: %v", r.Statement, r.Err)
}

// Unwrap returns the refusal's cause.
func (r Refusal) Unwrap() error { return r.Err }

// RefusedStatementsError reports every statement of a desired file that the
// front door refuses, in file order, so the file's author can fix them all in
// one edit instead of finding them one parse at a time. It matches each
// refusal's sentinels and typed errors, so a caller that only asks whether a
// file was refused, and why, reads it the way it reads a single refusal.
type RefusedStatementsError struct {
	Refusals []Refusal
}

func (e *RefusedStatementsError) Error() string {
	messages := make([]string, len(e.Refusals))
	for i, refusal := range e.Refusals {
		messages[i] = refusal.Error()
	}
	return strings.Join(messages, "\n")
}

// Unwrap returns each refusal, in file order.
func (e *RefusedStatementsError) Unwrap() []error {
	errs := make([]error, len(e.Refusals))
	for i, refusal := range e.Refusals {
		errs[i] = refusal
	}
	return errs
}

// desiredSource is a desired file's text and its lexer tokens. The text is
// scanned at most once, and only when a refusal needs to locate a statement,
// since a file with nothing refused never does. Every refusal of the file
// shares the one scan, however many statements it refuses.
type desiredSource struct {
	text    string
	tokens  []*pganalyze.ScanToken
	scanErr error
	scanned bool
}

func newDesiredSource(text string) *desiredSource {
	return &desiredSource{text: text}
}

// locate returns raw's verbatim text and where it starts in the file.
func (s *desiredSource) locate(raw *pganalyze.RawStmt) (SourceStatement, error) {
	if !s.scanned {
		s.scanned = true
		scan, err := pgquery.Scan(s.text)
		if err != nil {
			s.scanErr = fmt.Errorf("scan desired file: %w", err)
		} else {
			s.tokens = scan.GetTokens()
		}
	}
	if s.scanErr != nil {
		return SourceStatement{}, s.scanErr
	}
	return locate(s.text, s.tokens, raw), nil
}

// fileStatement is one statement of a desired file with its 0-based position
// among the file's statements, so a front door that admits only some of a
// file's statements still names each refusal where the file has it.
type fileStatement struct {
	index int
	raw   *pganalyze.RawStmt
}

// fileStatements returns every statement of tree, the parse of a desired
// file, at its position in the file.
func fileStatements(tree *pganalyze.ParseResult) []fileStatement {
	stmts := make([]fileStatement, len(tree.GetStmts()))
	for i, raw := range tree.GetStmts() {
		stmts[i] = fileStatement{index: i, raw: raw}
	}
	return stmts
}

// refusalCollector gathers the refusals of one desired file's statements,
// locating each in the file's source.
type refusalCollector struct {
	source   *desiredSource
	refusals []Refusal
}

func newRefusalCollector(source *desiredSource) *refusalCollector {
	return &refusalCollector{source: source}
}

// refuse records why the statement at index (0-based) of the file, raw, is
// refused. It fails only when the source cannot be scanned to locate it, and
// the failure still matches the refusal's cause, so a caller asking why the
// file was refused gets the same answer.
func (c *refusalCollector) refuse(index int, raw *pganalyze.RawStmt, cause error) error {
	at, err := c.source.locate(raw)
	if err != nil {
		return fmt.Errorf("statement %d: %w (locate the refused statement: %w)", index+1, cause, err)
	}
	c.refusals = append(c.refusals, Refusal{
		Statement: index + 1,
		Line:      at.Line,
		Column:    at.Column,
		Err:       cause,
	})
	return nil
}

// err returns the refusals as one error in file order, or nil when nothing
// was refused.
func (c *refusalCollector) err() error {
	if len(c.refusals) == 0 {
		return nil
	}
	slices.SortStableFunc(c.refusals, func(a, b Refusal) int {
		return cmp.Compare(a.Statement, b.Statement)
	})
	return &RefusedStatementsError{Refusals: c.refusals}
}
