package statement

import (
	"fmt"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// SourceStatement is one script statement located in its source: the
// verbatim text plus the position of its first token, so a consumer can
// point a finding back at the exact place in the file it came from.
type SourceStatement struct {
	// SQL is the statement's verbatim source text, without the trailing
	// semicolon, so it can be found in the source by exact match.
	SQL string
	// Line is the 1-based source line of the statement's first token.
	Line int
	// Column is the 1-based source column of the statement's first token.
	Column int
}

// Split parses a script with the PostgreSQL grammar and returns each
// statement with its verbatim text and source position, in input order.
// Every statement is also reprinted through the deparser as a validation
// gate: a statement the grammar cannot roundtrip is an error, never a
// guess. An empty script returns no statements; a parse failure anywhere
// in the script is surfaced to the caller, never guessed around.
func Split(sql string) ([]SourceStatement, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse script: %w", err)
	}
	scan, err := pgquery.Scan(sql)
	if err != nil {
		return nil, fmt.Errorf("scan script: %w", err)
	}
	stmts := make([]SourceStatement, 0, len(tree.GetStmts()))
	for i, raw := range tree.GetStmts() {
		if _, err := deparseOne(raw.GetStmt()); err != nil {
			return nil, fmt.Errorf("statement %d: %w", i+1, err)
		}
		stmts = append(stmts, locate(sql, scan.GetTokens(), raw))
	}
	return stmts, nil
}

// locate slices one statement's verbatim text out of the script and
// computes its position. The parser's statement range begins where the
// previous statement ended, so the first non-comment token inside the
// range — not the range start — is the statement's true beginning;
// leading comments and whitespace stay out of the reported text.
func locate(sql string, tokens []*pganalyze.ScanToken, raw *pganalyze.RawStmt) SourceStatement {
	bound := int(raw.GetStmtLocation()) + int(raw.GetStmtLen())
	// The parser marks the script's final statement with a zero length,
	// meaning "through the end of the input".
	if raw.GetStmtLen() == 0 {
		bound = len(sql)
	}
	// The statement's reported text runs from its first code token to its
	// last, so surrounding comments, whitespace, and the trailing
	// semicolon stay out of it.
	begin, end := int(raw.GetStmtLocation()), bound
	first := false
	for _, tok := range tokens {
		if int(tok.GetStart()) < begin || int(tok.GetEnd()) > bound ||
			isComment(tok) || tok.GetToken() == semicolonToken {
			continue
		}
		if !first {
			begin, first = int(tok.GetStart()), true
		}
		end = int(tok.GetEnd())
	}
	lastNewline := strings.LastIndexByte(sql[:begin], '\n')
	return SourceStatement{
		SQL:    sql[begin:end],
		Line:   1 + strings.Count(sql[:begin], "\n"),
		Column: begin - lastNewline,
	}
}

// semicolonToken is the lexer token for the statement separator ";".
const semicolonToken = pganalyze.Token_ASCII_59

// isComment reports whether a lexer token is an SQL (--) or C-style
// (/* */) comment.
func isComment(tok *pganalyze.ScanToken) bool {
	return tok.GetToken() == pganalyze.Token_SQL_COMMENT || tok.GetToken() == pganalyze.Token_C_COMMENT
}
