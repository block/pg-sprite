// This file predicts the relation names PostgreSQL invents for a CREATE
// TABLE's index-backed constraints and column-owned sequences. The create
// path's admission gate claims every relation name a desired set will
// occupy, and an implicit constraint index occupies one just as an
// explicit CREATE INDEX does —
// a set whose explicit index name collides with a constraint's index
// would otherwise pass admission and fail mid-run after the table
// committed. The prediction mirrors the server's first choice
// (makeObjectName in the PostgreSQL sources): when that first choice is
// already occupied on the server, PostgreSQL appends a numeric suffix
// instead, so a predicted name is where the server *starts*, not a
// guarantee of the final catalog name — exactly the right meaning for a
// duplicate-claim check inside one desired set.

package statement

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// ErrNotCreateTable is returned when the statement handed to
// ImplicitRelationNames is not a single CREATE TABLE.
var ErrNotCreateTable = errors.New("statement is not a CREATE TABLE")

// nameDataLen is PostgreSQL's NAMEDATALEN - 1: the byte budget an
// identifier is truncated to.
const nameDataLen = 63

// ImplicitRelationNames returns the first-choice relation names PostgreSQL
// will use for one CREATE TABLE statement: indexes for PRIMARY KEY, UNIQUE,
// and EXCLUDE constraints, and sequences for serial and identity columns.
// A named constraint's index takes the constraint name verbatim; an unnamed
// one takes the server's generated name
// (`<table>_pkey`, `<table>_<cols>_key`, `<table>_<cols>_excl`, truncated
// to the identifier byte budget the way the server truncates). A sequence
// takes `<table>_<column>_seq` under the same truncation rules. Names are
// returned in definition order and are not de-duplicated: two constraints
// whose first choices coincide both appear, so a claim map sees the
// conflict.
func ImplicitRelationNames(sql string) ([]string, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse statement: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return nil, fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	create := tree.GetStmts()[0].GetStmt().GetCreateStmt()
	if create == nil {
		return nil, ErrNotCreateTable
	}
	table := create.GetRelation().GetRelname()
	var names []string
	for _, elt := range create.GetTableElts() {
		if con := elt.GetConstraint(); con != nil {
			if name, ok := constraintIndexName(table, con); ok {
				names = append(names, name)
			}
			continue
		}
		col := elt.GetColumnDef()
		if col == nil {
			continue
		}
		if columnOwnsSequence(col) {
			names = append(names, makeObjectName(table, col.GetColname(), "seq"))
		}
		for _, c := range col.GetConstraints() {
			con := c.GetConstraint()
			if con == nil {
				continue
			}
			if name, ok := inlineConstraintIndexName(table, col.GetColname(), con); ok {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// columnOwnsSequence reports whether the column definition creates a
// sequence whose name is chosen from the table and column names.
func columnOwnsSequence(col *pganalyze.ColumnDef) bool {
	typeName, _ := typeRef(col.GetTypeName())
	if isSerialType(typeName) {
		return true
	}
	for _, node := range col.GetConstraints() {
		if node.GetConstraint().GetContype() == pganalyze.ConstrType_CONSTR_IDENTITY {
			return true
		}
	}
	return false
}

// constraintIndexName returns the index name a table-level constraint will
// claim, or ok=false when the constraint builds no index.
func constraintIndexName(table string, con *pganalyze.Constraint) (string, bool) {
	if name := con.GetConname(); name != "" && constraintBuildsIndex(con) {
		return name, true
	}
	switch con.GetContype() {
	case pganalyze.ConstrType_CONSTR_PRIMARY:
		return makeObjectName(table, "", "pkey"), true
	case pganalyze.ConstrType_CONSTR_UNIQUE:
		return makeObjectName(table, strings.Join(constraintKeys(con), "_"), "key"), true
	case pganalyze.ConstrType_CONSTR_EXCLUSION:
		return makeObjectName(table, strings.Join(exclusionKeys(con), "_"), "excl"), true
	default:
		return "", false
	}
}

// inlineConstraintIndexName returns the index name a column-inline
// constraint will claim, or ok=false when the constraint builds no index.
// An inline PRIMARY KEY names its index after the table alone, exactly as
// the table-constraint form does; an inline UNIQUE names it after the one
// column it covers.
func inlineConstraintIndexName(table, column string, con *pganalyze.Constraint) (string, bool) {
	if name := con.GetConname(); name != "" && constraintBuildsIndex(con) {
		return name, true
	}
	switch con.GetContype() {
	case pganalyze.ConstrType_CONSTR_PRIMARY:
		return makeObjectName(table, "", "pkey"), true
	case pganalyze.ConstrType_CONSTR_UNIQUE:
		return makeObjectName(table, column, "key"), true
	default:
		return "", false
	}
}

// constraintKeys returns the plain key column names of a PRIMARY KEY or
// UNIQUE table constraint.
func constraintKeys(con *pganalyze.Constraint) []string {
	keys := make([]string, 0, len(con.GetKeys()))
	for _, k := range con.GetKeys() {
		keys = append(keys, k.GetString_().GetSval())
	}
	return keys
}

// exclusionKeys returns the name contribution of each EXCLUDE element: the
// column name for a plain column, the literal "expr" for an expression —
// the same substitution the server makes when it builds the name.
func exclusionKeys(con *pganalyze.Constraint) []string {
	keys := make([]string, 0, len(con.GetExclusions()))
	for _, ex := range con.GetExclusions() {
		elem := ex.GetList().GetItems()[0].GetIndexElem()
		if name := elem.GetName(); name != "" {
			keys = append(keys, name)
			continue
		}
		keys = append(keys, "expr")
	}
	return keys
}

// makeObjectName mirrors PostgreSQL's makeObjectName: join name1, an
// optional name2, and the label with underscores, shrinking the longer of
// name1/name2 one byte at a time until the whole fits the identifier byte
// budget, never splitting a multibyte character.
func makeObjectName(name1, name2, label string) string {
	overhead := len(label) + 1
	if name2 != "" {
		overhead++
	}
	avail := nameDataLen - overhead
	n1, n2 := len(name1), len(name2)
	for n1+n2 > avail {
		if n1 > n2 {
			n1--
		} else {
			n2--
		}
	}
	name1 = clipToRuneBoundary(name1, n1)
	if name2 == "" {
		return name1 + "_" + label
	}
	return name1 + "_" + clipToRuneBoundary(name2, n2) + "_" + label
}

// clipToRuneBoundary truncates s to at most n bytes, backing off to the
// nearest rune boundary so a multibyte character is never split.
func clipToRuneBoundary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
