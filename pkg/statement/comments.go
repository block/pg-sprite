package statement

import (
	"errors"
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// ErrCommentLoss is returned when an operation that reprints SQL through the
// deparser would silently discard comments. The parser drops comments at
// parse time, so a formatter cannot carry them; refusing is the fail-closed
// alternative to destroying documentation in a source-of-truth file.
var ErrCommentLoss = errors.New("input contains comments, which formatting would discard")

// CheckNoComments scans sql with the PostgreSQL lexer and returns
// ErrCommentLoss when it contains any SQL (--) or C-style (/* */) comment.
// A scan failure is surfaced to the caller, never guessed around.
func CheckNoComments(sql string) error {
	scan, err := pgquery.Scan(sql)
	if err != nil {
		return fmt.Errorf("scan statement: %w", err)
	}
	for _, tok := range scan.GetTokens() {
		if tok.GetToken() == pganalyze.Token_SQL_COMMENT || tok.GetToken() == pganalyze.Token_C_COMMENT {
			return ErrCommentLoss
		}
	}
	return nil
}
