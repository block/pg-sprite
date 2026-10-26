package statement

import (
	"errors"
	"fmt"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	// ErrConversionRendering is returned when a USING expression, rendered
	// through the deparser, does not come back as the expression it was
	// rendered from: the rendering cannot be proven to be the user's
	// expression, so it is not handed to anything that would run it.
	ErrConversionRendering = errors.New("USING expression does not round-trip through the deparser")
	// ErrConversionRepeated is returned when one ALTER TABLE retypes the
	// same column with USING more than once; the copy can carry only one
	// expression per column, and the server itself refuses the statement.
	ErrConversionRepeated = errors.New("column is retyped with USING more than once")
	// ErrConversionForeignColumn is returned when a USING expression refers
	// to anything but the bare name of the column it retypes: another
	// column, a qualified name, or a whole-row reference. The copy runs the
	// expression over the source row, where every column is in reach, but
	// the stream's catch-up writes one column value at a time, and a value
	// that depends on a neighbouring column cannot be converted from the
	// column alone.
	ErrConversionForeignColumn = errors.New("USING expression refers to a column other than the one it retypes")
)

// ColumnConversions returns, for each column that sql's ALTER TABLE retypes
// with a USING clause, the USING expression as the deparser renders it:
// the SQL text a SELECT list can carry in the column's place. The map is
// keyed by column name and is empty when no subcommand carries USING.
//
// PostgreSQL keeps no record of a USING expression once ALTER COLUMN TYPE
// has run: the catalog describes the column's new type, not how the old
// values became it. The copy of a retyped column therefore has nowhere to
// take the conversion from but the statement itself, and this is the one
// place that reads it. The expression is extracted, not edited: the parse
// tree's node is rendered as the single target of a SELECT and the SELECT
// keyword removed, and the result is proven by parsing it back as a SELECT
// target and rendering it again to the same text. Nothing is derived from
// the expression's meaning, but one thing is read off its shape: every
// column reference in it must be the bare name of the column it retypes,
// because the catch-up that follows the copy converts one column value at
// a time and has no neighbouring column to hand the expression
// (ErrConversionForeignColumn). A statement that is not a single ALTER
// TABLE is ErrNotAlterTable.
func ColumnConversions(sql string) (map[string]string, error) {
	node, err := parseSingle(sql)
	if err != nil {
		return nil, err
	}
	alter := node.GetAlterTableStmt()
	if alter == nil {
		return nil, ErrNotAlterTable
	}
	conversions := map[string]string{}
	for _, cmd := range alter.GetCmds() {
		cmd := cmd.GetAlterTableCmd()
		if cmd.GetSubtype() != pganalyze.AlterTableType_AT_AlterColumnType {
			continue
		}
		// For ALTER COLUMN TYPE the grammar carries the USING expression
		// in the column definition's raw default slot.
		using := cmd.GetDef().GetColumnDef().GetRawDefault()
		if using == nil {
			continue
		}
		if _, seen := conversions[cmd.GetName()]; seen {
			return nil, fmt.Errorf("%w: %s", ErrConversionRepeated, cmd.GetName())
		}
		if foreign, ok := foreignColumnReference(using, cmd.GetName()); ok {
			return nil, fmt.Errorf("%w: USING of column %s refers to %s; the expression may name only %s, unqualified", ErrConversionForeignColumn, cmd.GetName(), foreign, cmd.GetName())
		}
		expr, err := renderExpression(using)
		if err != nil {
			return nil, fmt.Errorf("render USING expression of column %s: %w", cmd.GetName(), err)
		}
		conversions[cmd.GetName()] = expr
	}
	return conversions, nil
}

// renderExpression renders an expression node as SQL text by deparsing it
// as the one target of a SELECT, then proves the text by parsing it back
// the same way and rendering it again: the two renderings must agree.
func renderExpression(expr *pganalyze.Node) (string, error) {
	rendered, err := deparseOne(selectOf(expr))
	if err != nil {
		return "", err
	}
	text, ok := strings.CutPrefix(rendered, selectPrefix)
	if !ok {
		return "", fmt.Errorf("%w: rendered as %q", ErrConversionRendering, rendered)
	}
	reparsed, err := parseSingle(selectPrefix + text)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrConversionRendering, err)
	}
	again, err := deparseOne(reparsed)
	if err != nil {
		return "", err
	}
	if again != rendered {
		return "", fmt.Errorf("%w: %q re-renders as %q", ErrConversionRendering, rendered, again)
	}
	return text, nil
}

// selectPrefix is how the deparser begins a SELECT with no FROM clause.
const selectPrefix = "SELECT "

// selectOf wraps an expression as the single target of a SELECT with no
// FROM clause, the smallest statement the deparser renders an expression
// inside.
func selectOf(expr *pganalyze.Node) *pganalyze.Node {
	return &pganalyze.Node{Node: &pganalyze.Node_SelectStmt{SelectStmt: &pganalyze.SelectStmt{
		TargetList: []*pganalyze.Node{{Node: &pganalyze.Node_ResTarget{ResTarget: &pganalyze.ResTarget{Val: expr}}}},
		Op:         pganalyze.SetOperation_SETOP_NONE,
	}}}
}

// foreignColumnReference walks a USING expression's parse tree for the
// first column reference that is not the bare name of column, and returns
// how that reference was spelled: another column's name, a qualified name
// such as t.qty, or a whole-row star. The walk is over the tree's shape
// only — every message field, list, and map — so a reference nested in a
// CASE, a function call, or an operator's operand is found the same way as
// one at the top.
func foreignColumnReference(expr *pganalyze.Node, column string) (string, bool) {
	var foreign string
	found := false
	walkMessages(expr.ProtoReflect(), func(msg protoreflect.Message) bool {
		if ref, ok := msg.Interface().(*pganalyze.ColumnRef); ok {
			foreign, found = columnRefSpelling(ref, column)
		}
		return !found
	})
	return foreign, found
}

// columnRefSpelling reports a column reference that is anything but the
// bare name of column, spelled as the reference was written.
func columnRefSpelling(ref *pganalyze.ColumnRef, column string) (string, bool) {
	fields := ref.GetFields()
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.GetAStar() != nil {
			parts = append(parts, "*")
			continue
		}
		parts = append(parts, f.GetString_().GetSval())
	}
	if len(parts) == 1 && parts[0] == column {
		return "", false
	}
	return strings.Join(parts, "."), true
}
