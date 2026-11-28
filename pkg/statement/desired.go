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
	// the engine is single-table scoped. The parser returns it inside a
	// [*SecondTableError], which names the refused CREATE TABLE.
	ErrMultipleCreateTables = errors.New("desired schema must contain exactly one CREATE TABLE")
	// ErrDesiredPartitions is returned alongside ErrMultipleCreateTables
	// (both inside a [*SecondTableError]) for a file whose CREATE TABLEs
	// declare partitions of a table: a partition is a table of its own, and
	// the declarative model does not plan partitions.
	ErrDesiredPartitions = errors.New("desired schema declares partitions of a table, which the declarative model does not plan")
	// ErrDisallowedStatement is returned for any statement kind other than
	// CREATE TABLE / CREATE INDEX. The desired file is executed verbatim on
	// a scratch schema, so only pure schema definition is admitted.
	// The parser returns it inside a [*DisallowedStatementError], which
	// names the statement and its kind.
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

// DisallowedStatementError is [ErrDisallowedStatement] for one statement of
// a desired file. It carries the statement's SQL and the kind [ParseOne]
// gives the statement's source text, so a caller can classify the refusal
// the way the imperative front door classifies the same statement, and the
// object the statement declares, so it can say where the statement belongs.
type DisallowedStatementError struct {
	// SQL is the refused statement in the deparser's canonical form, or its
	// source text when the canonical form would classify as a different
	// kind: the parse tree drops some distinctions [ParseOne] reads from the
	// statement's keywords, such as an empty enum label.
	SQL string
	// Kind is the kind [ParseOne] gives the statement's source text.
	Kind Kind
	// Object is the kind of object the statement declares, changes, or
	// annotates, finer than Kind, so a caller can tell the file's author
	// where the statement belongs instead.
	Object DeclaredObject
}

func (e *DisallowedStatementError) Error() string {
	return fmt.Sprintf("%v: %s", ErrDisallowedStatement, e.SQL)
}

// Unwrap returns [ErrDisallowedStatement].
func (e *DisallowedStatementError) Unwrap() error { return ErrDisallowedStatement }

// SecondTableError is [ErrMultipleCreateTables] for the second CREATE TABLE
// of a desired file. It also matches [ErrDesiredPartitions] when either
// CREATE TABLE is a partition.
type SecondTableError struct {
	// SQL is the refused CREATE TABLE in the deparser's canonical form.
	SQL string
	// Partition reports whether the file declares a partition.
	Partition bool
}

func (e *SecondTableError) Error() string {
	if e.Partition {
		return fmt.Sprintf("%v: %v: %s", ErrMultipleCreateTables, ErrDesiredPartitions, e.SQL)
	}
	return fmt.Sprintf("%v: %s", ErrMultipleCreateTables, e.SQL)
}

// Unwrap returns [ErrMultipleCreateTables], and [ErrDesiredPartitions] when
// the file declares a partition.
func (e *SecondTableError) Unwrap() []error {
	if e.Partition {
		return []error{ErrMultipleCreateTables, ErrDesiredPartitions}
	}
	return []error{ErrMultipleCreateTables}
}

// disallowedStatement returns the refusal for raw, a statement of the
// desired file source that the front door does not admit.
func disallowedStatement(source *desiredSource, raw *pganalyze.RawStmt) error {
	sql, kind, err := refusedStatement(source, raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDisallowedStatement, err)
	}
	return &DisallowedStatementError{SQL: sql, Kind: kind, Object: declaredObject(raw.GetStmt())}
}

// refusedStatement returns the text a refusal names for raw, a statement of
// source, and the kind [ParseOne] gives it. The kind comes from the source
// text, not the canonical text, because the deparse can lose what tells two
// statements apart: a RENAME VALUE whose old label is empty deparses as an
// ADD VALUE. The text is the canonical one unless it classifies differently,
// in which case it is the source text.
func refusedStatement(source *desiredSource, raw *pganalyze.RawStmt) (string, Kind, error) {
	at, err := source.locate(raw)
	if err != nil {
		return "", KindOther, err
	}
	verbatim := at.SQL
	written, err := ParseOne(verbatim)
	if err != nil {
		return "", KindOther, fmt.Errorf("classify %s: %w", verbatim, err)
	}
	canonical, err := deparseOne(raw.GetStmt())
	if err != nil {
		return "", KindOther, err
	}
	reprinted, err := ParseOne(canonical)
	if err != nil {
		return "", KindOther, fmt.Errorf("classify %s: %w", canonical, err)
	}
	if reprinted.Kind() != written.Kind() {
		return verbatim, written.Kind(), nil
	}
	return canonical, written.Kind(), nil
}

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
// is unqualified, or is the table itself; desired-state execution adds,
// drops, and changes them (see docs/limitations.md). Anything else is
// refused with a typed error. A file that refuses statements and declares
// no table beside statements that need one returns an error matching both
// RefusedStatementsError and ErrNoCreateTable.
func ParseDesired(sql string) (DesiredSchema, error) {
	tree, err := pgquery.Parse(sql)
	if err != nil {
		return DesiredSchema{}, parseFailure(sql, err)
	}
	return parseDesiredTree(newDesiredSource(sql), fileStatements(tree))
}

// parseDesiredTree applies ParseDesired's admission rules to stmts, statements
// of source, and names each refusal at the statement's place in source.
func parseDesiredTree(source *desiredSource, stmts []fileStatement) (DesiredSchema, error) {
	if len(stmts) == 0 {
		return DesiredSchema{}, ErrEmptyDesired
	}
	var ds DesiredSchema
	partition := false
	refused := newRefusalCollector(source)
	// indexes holds each admitted CREATE INDEX's position in the file, so an
	// index on another table is refused where it stands once the file's
	// table is known: an index may precede its table.
	var indexes []admittedIndex
	for _, stmt := range stmts {
		i, raw := stmt.index, stmt.raw
		st, err := admitDesiredStatement(source, raw, ds.table, partition)
		if err != nil {
			if refusedFirstTable(ds.table, raw, err) {
				// The file's table is refused, but it is still the file's
				// table: a later CREATE TABLE is a second one, and the
				// table's own indexes are not on another table.
				ds.table = raw.GetStmt().GetCreateStmt().GetRelation().GetRelname()
			}
			if err := refused.refuse(i, raw, err); err != nil {
				return DesiredSchema{}, err
			}
			continue
		}
		if st.kind == KindCreateTable {
			ds.table = st.table
			partition = isPartition(raw.GetStmt().GetCreateStmt())
			ds.foreignKeys = len(foreignKeyConstraints(raw.GetStmt().GetCreateStmt())) > 0
			if ds.referenced, err = referencedTables(raw.GetStmt().GetCreateStmt()); err != nil {
				if err := refused.refuse(i, raw, err); err != nil {
					return DesiredSchema{}, err
				}
				continue
			}
		}
		if st.sql, err = deparseOne(raw.GetStmt()); err != nil {
			return DesiredSchema{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		if st.kind == KindCreateIndex {
			indexes = append(indexes, admittedIndex{index: i, raw: raw, table: st.table})
		}
		ds.statements = append(ds.statements, st)
	}
	if ds.table != "" {
		for _, idx := range indexes {
			if idx.table == ds.table {
				continue
			}
			cause := fmt.Errorf("%w: index on %q, desired table is %q", ErrWrongIndexTarget, idx.table, ds.table)
			if err := refused.refuse(idx.index, idx.raw, cause); err != nil {
				return DesiredSchema{}, err
			}
		}
	}
	// A file with no CREATE TABLE beside statements that need one, such as
	// an index, also reports its refused statements, so its author fixes
	// both in one edit. A file whose every statement is refused is not
	// missing a table: removing those statements is its remedy. A refused
	// CREATE TABLE still names the file's table, so this reports only a
	// file that declares none.
	if err := refused.err(); err != nil {
		if ds.table == "" && len(ds.statements) > 0 {
			return DesiredSchema{}, errors.Join(err, ErrNoCreateTable)
		}
		return DesiredSchema{}, err
	}
	if ds.table == "" {
		return DesiredSchema{}, ErrNoCreateTable
	}
	ds.statements = executionOrder(ds.statements)
	return ds, nil
}

// refusedFirstTable reports whether raw, refused with err, is the file's first
// CREATE TABLE and is still its table: a partition of another table is not.
func refusedFirstTable(table string, raw *pganalyze.RawStmt, err error) bool {
	if table != "" || raw.GetStmt().GetCreateStmt() == nil {
		return false
	}
	return !errors.Is(err, ErrDesiredPartitions)
}

// admittedIndex is a CREATE INDEX of a desired file, kept with its position
// so a refusal of it can name where it stands.
type admittedIndex struct {
	index int
	raw   *pganalyze.RawStmt
	table string
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

// admitDesiredStatement applies the per-statement admission rules to raw, a
// statement of source, and returns its kind and target. seenTable is the CREATE TABLE
// target admitted so far, empty when none, and seenPartition reports
// whether that CREATE TABLE is a partition.
func admitDesiredStatement(source *desiredSource, raw *pganalyze.RawStmt, seenTable string, seenPartition bool) (Statement, error) {
	node := raw.GetStmt()
	switch {
	case node.GetCreateStmt() != nil:
		create := node.GetCreateStmt()
		rel := create.GetRelation()
		if rel.GetSchemaname() != "" {
			return Statement{}, fmt.Errorf("%w: %s.%s", ErrQualifiedName, rel.GetSchemaname(), rel.GetRelname())
		}
		if seenTable != "" {
			sql, err := deparseOne(node)
			if err != nil {
				return Statement{}, fmt.Errorf("%w: %w", ErrMultipleCreateTables, err)
			}
			return Statement{}, &SecondTableError{SQL: sql, Partition: seenPartition || isPartition(create)}
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
		return Statement{}, disallowedStatement(source, raw)
	}
}

// isPartition reports whether a CREATE TABLE declares a partition of a
// table (PARTITION OF ... FOR VALUES or DEFAULT).
func isPartition(create *pganalyze.CreateStmt) bool {
	return create.GetPartbound() != nil
}

// Qualify returns sql with its target relation qualified by schema; an
// empty schema strips an existing qualification instead. It supports exactly
// one CREATE TABLE, CREATE INDEX, or ALTER TABLE statement. A CREATE TABLE's
// unqualified foreign-key references are qualified by schema too, because a
// desired file's referenced tables live in the target schema: left
// unqualified, a reference would resolve through the session's search_path
// and could bind a same-named table in another schema. Stripping is the
// inverse: it also strips a reference qualified by the target's own schema,
// so a planned CREATE TABLE round-trips back to its desired form, and leaves a
// reference to any other schema as written. This touches qualification only — no semantics are
// ever derived or transformed at the AST level (that is
// execute-and-introspect's job).
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
		// A reference written without a schema lives in the target's schema,
		// whichever qualification the target ends up with.
		own := rel.GetSchemaname()
		for _, con := range foreignKeyConstraints(node.GetCreateStmt()) {
			ref := con.GetPktable()
			if ref == nil {
				continue
			}
			switch {
			case schema != "" && ref.GetSchemaname() == "":
				ref.Schemaname = schema
			case schema == "" && own != "" && ref.GetSchemaname() == own:
				ref.Schemaname = ""
			}
		}
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
