package statement

import (
	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// IndexKeyColumns returns the table columns sql, one CREATE INDEX, names as
// key elements, in key order. PostgreSQL treats a key element written as a
// parenthesized column reference, with or without COLLATE, as that column, so
// it is listed too. Any other expression element is not a column and is not
// listed, and INCLUDE columns are not part of the key, so neither are they.
func IndexKeyColumns(sql string) ([]string, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return nil, err
	}
	idx := node.GetIndexStmt()
	if idx == nil {
		return nil, ErrNotCreateIndex
	}
	var columns []string
	for _, param := range idx.GetIndexParams() {
		elem := param.GetIndexElem()
		if elem.GetName() != "" {
			columns = append(columns, elem.GetName())
			continue
		}
		if column, ok := columnOfExpression(elem.GetExpr()); ok {
			columns = append(columns, column)
		}
	}
	return columns, nil
}

// columnOfExpression returns the column an index element's expression names
// when the expression is only a column reference, optionally under COLLATE.
func columnOfExpression(expr *pganalyze.Node) (string, bool) {
	if collate := expr.GetCollateClause(); collate != nil {
		expr = collate.GetArg()
	}
	ref := expr.GetColumnRef()
	if ref == nil || len(ref.GetFields()) == 0 {
		return "", false
	}
	// A qualified reference names the indexed table's column in its last
	// field; a star is no column at all.
	for _, field := range ref.GetFields() {
		if field.GetString_() == nil {
			return "", false
		}
	}
	fields := ref.GetFields()
	return fields[len(fields)-1].GetString_().GetSval(), true
}
