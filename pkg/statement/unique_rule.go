package statement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
)

// ErrNotUniqueKey is returned when a definition is not a unique index, a
// unique constraint, or a primary key.
var ErrNotUniqueKey = errors.New("definition is not a unique index, unique constraint, or primary key")

// UniqueKey is the rule a unique index, unique constraint, or primary key
// enforces: no two rows that its predicate covers have equal values in its
// key.
type UniqueKey struct {
	// Columns are the key's columns in key order. INCLUDE columns are not
	// part of the key and are left out.
	Columns []UniqueKeyColumn
	// Method is the index access method.
	Method string
	// Predicate is the deparsed WHERE clause of a partial index, empty for
	// every other key.
	Predicate string
	// NullsNotDistinct reports a key under which NULLs count as equal.
	NullsNotDistinct bool
	// Primary reports a primary key, whose columns are all NOT NULL.
	Primary bool
	// Deferrable reports a key the server may check at commit instead of
	// at each statement.
	Deferrable bool
}

// UniqueKeyColumn is one element of a unique key.
type UniqueKeyColumn struct {
	// Column is the column a plain element names, empty for an expression.
	Column string
	// Collation is the element's explicit collation, empty when it has none.
	Collation string
	// OpClass is the element's explicit operator class, empty when it has
	// none.
	OpClass string
}

// ParseUniqueIndex reads the unique key of a CREATE UNIQUE INDEX statement.
func ParseUniqueIndex(def string) (UniqueKey, error) {
	idx, _, err := parseCreateIndex(def)
	if err != nil {
		return UniqueKey{}, fmt.Errorf("parse unique index %q: %w", def, err)
	}
	if !idx.GetUnique() {
		return UniqueKey{}, fmt.Errorf("%w: %q", ErrNotUniqueKey, def)
	}
	key := UniqueKey{
		Method:           idx.GetAccessMethod(),
		NullsNotDistinct: idx.GetNullsNotDistinct(),
	}
	if key.Method == "" {
		key.Method = "btree"
	}
	for _, p := range idx.GetIndexParams() {
		elem := p.GetIndexElem()
		key.Columns = append(key.Columns, UniqueKeyColumn{
			Column:    elem.GetName(),
			Collation: joinNames(elem.GetCollation()),
			OpClass:   joinNames(elem.GetOpclass()),
		})
	}
	if where := idx.GetWhereClause(); where != nil {
		predicate, err := deparseOne(&pganalyze.Node{Node: &pganalyze.Node_SelectStmt{
			SelectStmt: &pganalyze.SelectStmt{WhereClause: where},
		}})
		if err != nil {
			return UniqueKey{}, fmt.Errorf("render predicate of unique index %q: %w", def, err)
		}
		key.Predicate = predicate
	}
	return key, nil
}

// ParseUniqueConstraint reads the unique key of a unique constraint or a
// primary key from its definition as pg_get_constraintdef prints it, such
// as "UNIQUE (sku)" or "PRIMARY KEY (id)".
func ParseUniqueConstraint(def string) (UniqueKey, error) {
	node, err := parseSingle("ALTER TABLE t ADD " + def)
	if err != nil {
		return UniqueKey{}, fmt.Errorf("parse constraint %q: %w", def, err)
	}
	cmds := node.GetAlterTableStmt().GetCmds()
	if len(cmds) != 1 {
		return UniqueKey{}, fmt.Errorf("%w: %q", ErrNotUniqueKey, def)
	}
	con := cmds[0].GetAlterTableCmd().GetDef().GetConstraint()
	switch con.GetContype() {
	case pganalyze.ConstrType_CONSTR_UNIQUE, pganalyze.ConstrType_CONSTR_PRIMARY:
	default:
		return UniqueKey{}, fmt.Errorf("%w: %q", ErrNotUniqueKey, def)
	}
	key := UniqueKey{
		Method:           "btree",
		NullsNotDistinct: con.GetNullsNotDistinct(),
		Primary:          con.GetContype() == pganalyze.ConstrType_CONSTR_PRIMARY,
		Deferrable:       con.GetDeferrable(),
	}
	for _, k := range con.GetKeys() {
		key.Columns = append(key.Columns, UniqueKeyColumn{Column: k.GetString_().GetSval()})
	}
	return key, nil
}

// Enforces reports whether k rejects every pair of rows that dropped
// rejects, so dropping dropped while k stays in place lets no duplicate
// in. That holds when k's key is plain columns that dropped's key also
// compares, with the same collation and operator class; k covers at least
// the rows dropped covers; k treats NULLs as equal wherever dropped does;
// and k is checked no later than dropped is.
func (k UniqueKey) Enforces(dropped UniqueKey) bool {
	if len(k.Columns) == 0 || k.Method != dropped.Method {
		return false
	}
	for _, col := range k.Columns {
		if col.Column == "" || !dropped.comparesColumn(col) {
			return false
		}
	}
	if k.Predicate != "" && k.Predicate != dropped.Predicate {
		return false
	}
	if dropped.NullsNotDistinct && !k.NullsNotDistinct && !k.Primary {
		return false
	}
	return !k.Deferrable || dropped.Deferrable
}

// comparesColumn reports whether k's key has col as a plain element with
// the same collation and operator class.
func (k UniqueKey) comparesColumn(col UniqueKeyColumn) bool {
	return slices.Contains(k.Columns, col)
}

// joinNames renders a possibly qualified name list as dotted text.
func joinNames(nodes []*pganalyze.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, n.GetString_().GetSval())
	}
	return strings.Join(parts, ".")
}
