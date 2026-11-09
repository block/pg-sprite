package statement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pganalyze "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
)

// Typed refusals for desired-state schema files. Each names one rule of the
// declarative front door; the caller branches with errors.Is, never on text.
var (
	// ErrParseDesired is returned when the input is not valid PostgreSQL
	// syntax, before any admission rule runs. It separates a syntax error
	// from an admission refusal; the parser's own message is joined to it.
	ErrParseDesired = errors.New("parse desired schema")
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
	// ErrWrongIndexTarget is returned when an index targets a table other
	// than the desired CREATE TABLE.
	ErrWrongIndexTarget = errors.New("index must target the desired table")
	// ErrExtensionSchema is returned by [DesiredSchema.WithExtensionSchemas]
	// for a name that cannot hold extension objects the file resolves: an
	// empty name, a duplicate, or a system schema PostgreSQL searches on its
	// own terms.
	ErrExtensionSchema = errors.New("invalid extension schema")
)

// DesiredSchema proves a desired-state schema file passed the set-level
// admission rules: exactly one unqualified CREATE TABLE plus any number of
// CREATE INDEX statements on that table, none of them CONCURRENTLY, and every
// table a foreign key references named unqualified. Only [ParseDesired]
// produces a non-zero value.
//
// Statement SQL is canonical (parsed and deparsed through the PostgreSQL
// grammar), one statement per entry, held in execution order: the CREATE
// TABLE first, the indexes in input order after it.
type DesiredSchema struct {
	table            string
	foreignKeys      bool
	referenced       []string
	statements       []Statement
	extensionSchemas []string
}

// Table returns the unqualified name of the single CREATE TABLE target.
func (ds DesiredSchema) Table() string { return ds.table }

// HasForeignKeys reports whether the CREATE TABLE declares a foreign key,
// a self-reference included.
func (ds DesiredSchema) HasForeignKeys() bool { return ds.foreignKeys }

// ReferencedTables returns the unqualified names of the other tables the
// CREATE TABLE's foreign keys reference, sorted and distinct; a
// self-reference is not listed. The slice is a copy.
func (ds DesiredSchema) ReferencedTables() []string { return slices.Clone(ds.referenced) }

// Statements returns the admitted statements in execution order: the
// CREATE TABLE first, the indexes in input order after it — an index
// cannot be built before its table exists, and every consumer replays
// this order verbatim. The slice is a copy: mutating it cannot invalidate
// the admission proof the value carries.
func (ds DesiredSchema) Statements() []Statement { return slices.Clone(ds.statements) }

// ExtensionSchemas returns the schemas, in search order, that the file's
// unqualified type, function, and operator names also resolve against
// after the target schema and public. It is empty unless
// [DesiredSchema.WithExtensionSchemas] set it. The slice is a copy.
func (ds DesiredSchema) ExtensionSchemas() []string { return slices.Clone(ds.extensionSchemas) }

// WithExtensionSchemas returns a copy of ds whose unqualified type,
// function, and operator names also resolve against schemas, in order,
// after the target schema and public: `email citext` then finds the type
// an extension installed in a schema such as extensions. The schemas only
// widen what the file's names resolve to when pg-sprite builds and creates
// the table; introspection still prints each such name qualified, so a
// plan states exactly which object it means. Unqualified names are a
// choice the caller makes explicitly, because with more schemas searched
// an unqualified function call can resolve to a different overload.
//
// A name must be non-empty, listed once, and not a system schema
// (pg_catalog, information_schema, or any pg_ name): pg_catalog is
// searched first regardless, and the others never hold an extension the
// file should name. An empty schemas clears the list.
func (ds DesiredSchema) WithExtensionSchemas(schemas ...string) (DesiredSchema, error) {
	seen := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		switch {
		case schema == "":
			return DesiredSchema{}, fmt.Errorf("%w: the name is empty", ErrExtensionSchema)
		case seen[schema]:
			return DesiredSchema{}, fmt.Errorf("%w: %q is listed twice", ErrExtensionSchema, schema)
		case schema == "information_schema" || strings.HasPrefix(schema, "pg_"):
			return DesiredSchema{}, fmt.Errorf("%w: %q is a system schema", ErrExtensionSchema, schema)
		}
		seen[schema] = true
	}
	ds.extensionSchemas = slices.Clone(schemas)
	return ds, nil
}

// ParseDesired parses a desired-state schema file and admits only what the
// declarative front door can execute on a scratch schema: one unqualified
// CREATE TABLE and unqualified, non-concurrent CREATE INDEX statements on
// it. Foreign keys are admitted for planning when the table they reference
// is unqualified; desired-state execution refuses every foreign-key change
// (see docs/limitations.md). Anything else is refused with a typed error.
func ParseDesired(sql string) (DesiredSchema, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredSchema{}, fmt.Errorf("%w: %w", ErrParseDesired, err)
	}
	return parseDesiredTree(tree)
}

// parseDesiredTree applies ParseDesired's admission rules to a parsed file.
func parseDesiredTree(tree *pganalyze.ParseResult) (DesiredSchema, error) {
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
			ds.foreignKeys = len(foreignKeyConstraints(raw.GetStmt().GetCreateStmt())) > 0
			if ds.referenced, err = referencedTables(raw.GetStmt().GetCreateStmt()); err != nil {
				return DesiredSchema{}, fmt.Errorf("statement %d: %w", i+1, err)
			}
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
	ds.statements = executionOrder(ds.statements)
	return ds, nil
}

// executionOrder hoists the CREATE TABLE to the front, keeping the indexes
// in input order after it. Ordering once at admission — rather than at each
// replay site — means every consumer of the proof (the scratch-schema
// introspection, the greenfield plan, the create path's steps) executes an
// index-before-table file correctly by construction.
func executionOrder(statements []Statement) []Statement {
	ordered := make([]Statement, 0, len(statements))
	for _, st := range statements {
		if st.kind == KindCreateTable {
			ordered = append(ordered, st)
		}
	}
	for _, st := range statements {
		if st.kind != KindCreateTable {
			ordered = append(ordered, st)
		}
	}
	return ordered
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

// Qualify returns sql with its target relation qualified by schema; an
// empty schema strips an existing qualification instead. It supports exactly
// one CREATE TABLE, CREATE INDEX, or ALTER TABLE statement. This touches
// qualification only — no semantics are ever derived or transformed at the
// AST level (that is execute-and-introspect's job).
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
