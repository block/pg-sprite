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

// refusalCollector gathers the refusals of one desired file's statements,
// locating each in the file's source. The source is scanned once, on the
// first refusal, since a file with nothing refused never needs it.
type refusalCollector struct {
	source   string
	tokens   []*pganalyze.ScanToken
	scanned  bool
	refusals []Refusal
}

func newRefusalCollector(source string) *refusalCollector {
	return &refusalCollector{source: source}
}

// refuse records why the statement at index (0-based) of the file, raw, is
// refused. It fails only when the source cannot be scanned to locate it.
func (c *refusalCollector) refuse(index int, raw *pganalyze.RawStmt, cause error) error {
	if !c.scanned {
		scan, err := pgquery.Scan(c.source)
		if err != nil {
			return fmt.Errorf("statement %d: scan desired file to locate a refused statement: %w", index+1, err)
		}
		c.tokens, c.scanned = scan.GetTokens(), true
	}
	at := locate(c.source, c.tokens, raw)
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
