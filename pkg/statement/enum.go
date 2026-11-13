package statement

import (
	"fmt"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// addsEnumValue reports whether sql, which parsed as enum, is an ALTER
// TYPE ... ADD VALUE that its parse tree describes in full. The tree cannot
// tell an empty label from an absent one: RENAME VALUE ” TO 'b' leaves the
// old value empty, exactly as ADD VALUE does, and BEFORE ” leaves the
// neighbor empty, exactly as a value with no position does, so the
// deparser would reprint either as a different statement. The lexer tells
// them apart. The keywords that follow the type name are ADD VALUE only in
// the form that adds a value, and a BEFORE or AFTER keyword marks a
// position whatever its label. A form that adds a value next to the empty
// label is not admitted, since no rendering of it would say so.
func addsEnumValue(sql string, enum *pganalyze.AlterEnumStmt) (bool, error) {
	if enum.GetOldVal() != "" {
		return false, nil
	}
	parts := len(enum.GetTypeName())
	if parts == 0 {
		return false, nil
	}
	scan, err := pgquery.Scan(sql)
	if err != nil {
		return false, fmt.Errorf("scan statement: %w", err)
	}
	var tokens []pganalyze.Token
	for _, tok := range scan.GetTokens() {
		if !isComment(tok) {
			tokens = append(tokens, tok.GetToken())
		}
	}
	// ALTER TYPE, then each part of the type name with a dot between
	// parts, then the action.
	action := 2 + 2*parts - 1
	if len(tokens) < action+2 ||
		tokens[0] != pganalyze.Token_ALTER || tokens[1] != pganalyze.Token_TYPE_P ||
		tokens[action] != pganalyze.Token_ADD_P || tokens[action+1] != pganalyze.Token_VALUE_P {
		return false, nil
	}
	for _, tok := range tokens[action+2:] {
		if (tok == pganalyze.Token_BEFORE || tok == pganalyze.Token_AFTER) && enum.GetNewValNeighbor() == "" {
			return false, nil
		}
	}
	return true, nil
}
