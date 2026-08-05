package statement

import (
	"fmt"

	pgquery "github.com/wasilibs/go-pgquery"
)

// Split parses a script with the PostgreSQL grammar and returns each
// statement's canonical SQL, in input order, without trailing semicolons.
// An empty script returns no statements; a parse failure anywhere in the
// script is surfaced to the caller, never guessed around.
func Split(sql string) ([]string, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse script: %w", err)
	}
	stmts := make([]string, 0, len(tree.GetStmts()))
	for i, raw := range tree.GetStmts() {
		out, err := deparseOne(raw.GetStmt())
		if err != nil {
			return nil, fmt.Errorf("statement %d: %w", i+1, err)
		}
		stmts = append(stmts, out)
	}
	return stmts, nil
}
