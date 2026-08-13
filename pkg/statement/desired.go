package statement

import (
	"errors"
	"fmt"
	"slices"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// Typed refusals for desired-state schema files. Each names one rule of the
// declarative front door; the caller branches with errors.Is, never on text.
var (
	// ErrEmptyDesired is returned when the input contains no statements.
	ErrEmptyDesired = errors.New("desired schema contains no statements")
	// ErrNoCreateTable is returned when the input has no CREATE TABLE.
	ErrNoCreateTable = errors.New("desired schema must contain a CREATE TABLE")
	// ErrMultipleCreateTables is returned for more than one CREATE TABLE:
	// the engine is single-table scoped.
	ErrMultipleCreateTables = errors.New("desired schema must contain exactly one CREATE TABLE")
	// ErrDisallowedStatement is returned for any statement kind other than
	// CREATE TABLE / CREATE INDEX. The desired file is executed verbatim on
	// a scratch schema, so only pure schema definition is admitted.
	ErrDisallowedStatement = errors.New("statement kind not allowed in a desired schema")
	// ErrQualifiedName is returned when a statement schema-qualifies its
	// target. Desired files are schema-relative; the live schema comes from
	// the caller, and qualification could escape the scratch schema.
	ErrQualifiedName = errors.New("desired schema statements must use unqualified names")
	// ErrConcurrentIndex is returned for CREATE INDEX CONCURRENTLY, which
	// cannot run inside the scratch transaction.
	ErrConcurrentIndex = errors.New("CONCURRENTLY cannot be used in a desired schema")
	// ErrForeignKey is returned when the CREATE TABLE carries a REFERENCES
	// clause. The scratch transaction cannot faithfully execute a foreign
	// key: an unqualified reference resolves against the scratch
	// search_path, not the target schema, so it either fails or silently
	// binds to the wrong table. Foreign-key support needs its own design
	// (cross-file ordering, lock behavior, qualification policy); until
	// then the admission gate refuses it.
	ErrForeignKey = errors.New("foreign keys are not supported in a desired schema")
	// ErrWrongIndexTarget is returned when an index targets a table other
	// than the desired CREATE TABLE.
	ErrWrongIndexTarget = errors.New("index must target the desired table")
)

// DesiredSchema is a validated desired-state schema file: exactly one
// CREATE TABLE plus any number of CREATE INDEX statements on that table.
// Statement SQL is canonical (parsed and deparsed through the PostgreSQL
// grammar), in input order, one statement per entry.
//
// Only [ParseDesired] produces a non-zero value, so holding one is proof
// the set-level admission rules held: a single unqualified CREATE TABLE,
// every index on that table, none of them CONCURRENTLY.
type DesiredSchema struct {
	table      string
	statements []Statement
}

// Table returns the unqualified name of the single CREATE TABLE target.
func (ds DesiredSchema) Table() string { return ds.table }

// Statements returns the admitted statements in input order, the CREATE
// TABLE among them. The slice is a copy: mutating it cannot invalidate the
// admission proof the value carries.
func (ds DesiredSchema) Statements() []Statement { return slices.Clone(ds.statements) }

// ParseDesired parses a desired-state schema file and admits only what the
// declarative front door can execute on a scratch schema: one unqualified
// CREATE TABLE and unqualified, non-concurrent CREATE INDEX statements on
// it. Anything else is refused with a typed error.
func ParseDesired(sql string) (DesiredSchema, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredSchema{}, fmt.Errorf("parse desired schema: %w", err)
	}
	if len(tree.GetStmts()) == 0 {
		return DesiredSchema{}, ErrEmptyDesired
	}
	var ds DesiredSchema
	for i, raw := range tree.GetStmts() {
		st, err := admitDesiredStatement(raw.GetStmt(), ds.table)
		if err != nil {
			return DesiredSchema{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		if st.kind == KindCreateTable {
			ds.table = st.table
		}
		if st.sql, err = deparseOne(raw.GetStmt()); err != nil {
			return DesiredSchema{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		ds.statements = append(ds.statements, st)
	}
	if ds.table == "" {
		return DesiredSchema{}, ErrNoCreateTable
	}
	for _, st := range ds.statements {
		if st.kind == KindCreateIndex && st.table != ds.table {
			return DesiredSchema{}, fmt.Errorf("%w: index on %q, desired table is %q",
				ErrWrongIndexTarget, st.table, ds.table)
		}
	}
	return ds, nil
}

// admitDesiredStatement applies the per-statement admission rules and
// returns the statement's kind and target. seenTable is the CREATE TABLE
// target admitted so far, empty when none.
func admitDesiredStatement(node *pganalyze.Node, seenTable string) (Statement, error) {
	switch {
	case node.GetCreateStmt() != nil:
		create := node.GetCreateStmt()
		rel := create.GetRelation()
		if rel.GetSchemaname() != "" {
			return Statement{}, fmt.Errorf("%w: %s.%s", ErrQualifiedName, rel.GetSchemaname(), rel.GetRelname())
		}
		if seenTable != "" {
			return Statement{}, ErrMultipleCreateTables
		}
		if err := refuseForeignKeys(create); err != nil {
			return Statement{}, err
		}
		return Statement{kind: KindCreateTable, table: rel.GetRelname()}, nil
	case node.GetIndexStmt() != nil:
		idx := node.GetIndexStmt()
		if idx.GetConcurrent() {
			return Statement{}, ErrConcurrentIndex
		}
		rel := idx.GetRelation()
		if rel.GetSchemaname() != "" {
			return Statement{}, fmt.Errorf("%w: %s.%s", ErrQualifiedName, rel.GetSchemaname(), rel.GetRelname())
		}
		return Statement{kind: KindCreateIndex, table: rel.GetRelname()}, nil
	default:
		return Statement{}, ErrDisallowedStatement
	}
}

// refuseForeignKeys returns ErrForeignKey when the CREATE TABLE carries a
// REFERENCES clause, in either its column-constraint or table-constraint
// form. This inspects constraint kinds only — no semantics are derived.
func refuseForeignKeys(create *pganalyze.CreateStmt) error {
	for _, elt := range create.GetTableElts() {
		if con := elt.GetConstraint(); con != nil && con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN {
			return fmt.Errorf("%w: table constraint on %q", ErrForeignKey, create.GetRelation().GetRelname())
		}
		col := elt.GetColumnDef()
		if col == nil {
			continue
		}
		for _, c := range col.GetConstraints() {
			if con := c.GetConstraint(); con != nil && con.GetContype() == pganalyze.ConstrType_CONSTR_FOREIGN {
				return fmt.Errorf("%w: column %q", ErrForeignKey, col.GetColname())
			}
		}
	}
	return nil
}

// Qualify returns sql with its target relation qualified by schema; an
// empty schema strips an existing qualification instead. It supports exactly
// one CREATE TABLE, CREATE INDEX, or ALTER TABLE statement. This touches
// qualification only — no semantics are ever derived or transformed at the
// AST level (that is the scratch database's job).
func Qualify(sql, schema string) (string, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("parse statement to qualify: %w", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return "", fmt.Errorf("%w: got %d", ErrNotOneStatement, n)
	}
	node := tree.GetStmts()[0].GetStmt()
	var rel *pganalyze.RangeVar
	switch {
	case node.GetCreateStmt() != nil:
		rel = node.GetCreateStmt().GetRelation()
	case node.GetIndexStmt() != nil:
		rel = node.GetIndexStmt().GetRelation()
	case node.GetAlterTableStmt() != nil:
		rel = node.GetAlterTableStmt().GetRelation()
	default:
		return "", ErrDisallowedStatement
	}
	if rel == nil {
		return "", ErrDisallowedStatement
	}
	rel.Schemaname = schema
	return deparseOne(node)
}

// deparseOne renders a single parsed statement back to canonical SQL through
// the PostgreSQL deparser.
func deparseOne(node *pganalyze.Node) (string, error) {
	out, err := pgquery.Deparse(&pganalyze.ParseResult{
		Stmts: []*pganalyze.RawStmt{{Stmt: node}},
	})
	if err != nil {
		return "", fmt.Errorf("deparse statement: %w", err)
	}
	return out, nil
}
