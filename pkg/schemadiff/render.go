package schemadiff

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/statement"
)

// ErrUnrenderableDefault is returned when a column carries a
// sequence-backed default that is not the canonical serial form. A desired
// schema file cannot define a standalone sequence, so the only
// sequence-backed default a rendered file can reproduce is the serial
// shorthand (an owned sequence named <table>_<column>_seq on a NOT NULL
// integer column). Anything else — a shared sequence, a renamed or
// truncated sequence name, a nullable column — must be resolved by hand.
var ErrUnrenderableDefault = errors.New("sequence-backed default cannot be rendered as a desired schema")

// ErrUnrenderablePartition is returned for a partition. The model does not
// carry partition bounds or the parent/partition topology, so a rendered
// file would silently lose the partition attachment — the renderer refuses
// instead of emitting a wrong baseline. A partitioned parent renders as its
// own definition, PARTITION BY clause included.
var ErrUnrenderablePartition = errors.New("partitions cannot be rendered as a desired schema")

// ErrUnrenderableInheritance is returned for either side of a classic
// PostgreSQL inheritance relationship. The desired model cannot express
// inheritance edges, so rendering would flatten a child or omit its children.
var ErrUnrenderableInheritance = errors.New("table inheritance cannot be rendered as a desired schema")

// ErrUnrenderableForeignKey is returned when the table owns foreign keys or
// other tables reference it with foreign keys, unless the caller opts in
// with RenderOptions.ForeignKeys. A foreign key belongs to the file of the
// table that holds it, so a rendered baseline of a referenced table is
// complete only when the referencing tables are rendered with it, which one
// table's model cannot show.
var ErrUnrenderableForeignKey = errors.New("tables that own or are referenced by foreign keys cannot be rendered as a desired schema")

// ErrUnrenderableUnlogged is returned for an unlogged table. The
// declarative model does not manage persistence, so a rendered plain
// CREATE TABLE would silently change the table's crash-safety and
// replication behavior — the renderer refuses instead.
var ErrUnrenderableUnlogged = errors.New("unlogged tables cannot be rendered as a desired schema")

// ErrUnrenderableCollation is returned when a column carries an explicit
// collation. The declarative model does not manage collations, so a
// rendered baseline without the COLLATE clause would silently change sort
// order and index semantics — the renderer refuses instead.
var ErrUnrenderableCollation = errors.New("columns with an explicit collation cannot be rendered as a desired schema")

// ErrUnrenderableVirtual is returned when a column is generated VIRTUAL.
// The desired-file grammar cannot parse VIRTUAL, and rendering the column
// as STORED or as a plain default would silently change what it is.
var ErrUnrenderableVirtual = errors.New("virtual generated columns cannot be rendered as a desired schema")

// ErrUnrenderableToastOptions is returned for TOAST storage parameters on a
// table whose TOAST table a fresh CREATE TABLE might not build (see
// Model.ToastMayNotRebuild). The server would discard the parameters when
// it builds the rendered file, so the baseline would plan them again on
// every diff against the table it came from.
var ErrUnrenderableToastOptions = errors.New("TOAST storage parameters on a table whose remaining columns may not build a TOAST table cannot be rendered as a desired schema")

// ErrMultipleOwnedSequences is returned for a column whose default uses
// more than one sequence the column owns (Column.MultipleOwnedSequences):
// the serial shorthand declares one, and exporting the default as written
// would leave both sequences out of the file.
var ErrMultipleOwnedSequences = errors.New("column default uses more than one sequence the column owns")

// ErrUnrenderableSequenceOptions is returned when a column owns a
// sequence declared differently from what declaring the column fresh
// creates and a desired file cannot state the declaration: a serial
// column, whose shorthand has no options clause, or an identity whose
// sequence's type differs from the column's. Rendering the column at the
// defaults would silently move its bounds, step, or type.
var ErrUnrenderableSequenceOptions = errors.New("sequence options cannot be rendered as a desired schema")

// ErrUnrenderableMergedConstraints is returned for a table with two PRIMARY
// KEY, UNIQUE, or EXCLUDE constraints declared the same way. Built from one
// CREATE TABLE, the server keeps only one of them, and a desired file has
// no other way to declare the second (see ErrDesiredConstraintMerged).
var ErrUnrenderableMergedConstraints = errors.New("constraints that repeat each other on the same columns cannot be rendered as a desired schema")

// renderRefusals returns every verdict the renderer refuses a table with.
func renderRefusals() []error {
	return []error{
		ErrUnrenderableDefault,
		ErrUnrenderablePartition,
		ErrUnrenderableInheritance,
		ErrUnrenderableForeignKey,
		ErrUnrenderableUnlogged,
		ErrUnrenderableCollation,
		ErrUnrenderableVirtual,
		ErrUnrenderableToastOptions,
		ErrMultipleOwnedSequences,
		ErrUnrenderableSequenceOptions,
		ErrUnrenderableMergedConstraints,
		ErrUnrenderableUnattachedIndex,
		ErrUnrenderablePartitionedRowSecurity,
		ErrUnrenderableRowSecurity,
		statement.ErrRowSecurityForeignKey,
	}
}

// RenderRefusal returns the verdict a Render error carries, such as
// ErrUnrenderableUnlogged, so a caller can say what the table holds without
// the schema and table names the full error repeats. It reports false for an
// error that carries none of the renderer's verdicts.
func RenderRefusal(err error) (error, bool) {
	for _, verdict := range renderRefusals() {
		if errors.Is(err, verdict) {
			return verdict, true
		}
	}
	return nil, false
}

// Render renders the canonical model into a desired-state schema file: one
// CREATE TABLE, with a WITH clause carrying the table's storage parameters,
// followed by the model's CREATE INDEX statements. The output
// is proven admissible by parsing it through statement.ParseDesired before
// it is returned, so anything a desired file refuses (a schema-qualified
// name, for example) surfaces here as that gate's typed error. Materializing the
// output with IntrospectDesired reproduces the model's names and
// definitions, so diffing it against the table it came from yields no
// changes — the round-trip contract the integration tests enforce.
// Validity is not rendered: an index the table carries invalid — an
// unfinished concurrent build — renders as its definition and materializes
// valid, so the round-trip diff of such a table is exactly the create-index
// change that rebuilds it.
// A partitioned parent renders with its PARTITION BY clause and its indexes
// declared on the whole table; its partitions are not rendered, and one
// with an index some partition lacks is refused.
func Render(m Model) (string, error) {
	return renderTable(m, RenderOptions{})
}

// RenderOptions widens what RenderWith writes beyond Render's default.
type RenderOptions struct {
	// ForeignKeys writes the table's own foreign keys, each as its
	// constraint in the CREATE TABLE, and renders a table other tables
	// reference instead of refusing it. Still refused: a foreign key whose
	// referenced table is in another schema, because a desired file names
	// tables only in its own schema (statement.ErrQualifiedName); one from
	// the table to itself whose referenced columns are unique only through
	// a standalone index, which the CREATE TABLE would declare before the
	// index exists (ErrUnrenderableForeignKey); and one on a table rendered
	// with RowSecurity (statement.ErrRowSecurityForeignKey).
	//
	// The caller takes on what one table's model cannot show: it renders
	// every table in Model.IncomingForeignKeys too, so each relationship is
	// written in the file of the table that holds it. A baseline that
	// leaves a referencing table out drops that table's foreign key.
	ForeignKeys bool
	// RowSecurity writes the table's row security settings and policies
	// after it, as RenderWithRowSecurity does.
	RowSecurity bool
}

// RenderWith renders the model as Render does, widened by opts.
func RenderWith(m Model, opts RenderOptions) (string, error) {
	if opts.RowSecurity {
		return renderWithRowSecurity(m, opts)
	}
	return renderTable(m, opts)
}

// renderTable renders the table without its row security, which it
// refuses when the model carries any.
func renderTable(m Model, opts RenderOptions) (string, error) {
	if m.RowSecurity.present() {
		return "", fmt.Errorf("render table %q: %w", m.Table, ErrUnrenderableRowSecurity)
	}
	// A sub-partitioned partition carries a partition key too, so the
	// partition refusal comes first: it is never rendered, whatever its own
	// indexes are.
	if m.IsPartition {
		return "", fmt.Errorf("render table %q: partition of a partitioned parent: %w", m.Table, ErrUnrenderablePartition)
	}
	if m.PartitionKey != "" {
		if err := partitionedParentRefusal(m); err != nil {
			return "", fmt.Errorf("render table %q: partitioned parent (PARTITION BY %s): %w", m.Table, m.PartitionKey, err)
		}
	}
	if len(m.InheritsParents) != 0 {
		return "", fmt.Errorf("render table %q: inherits from %s: %w", m.Table, strings.Join(m.InheritsParents, ", "), ErrUnrenderableInheritance)
	}
	if len(m.InheritanceChildren) != 0 {
		return "", fmt.Errorf("render table %q: has inheritance children %s: %w", m.Table, strings.Join(m.InheritanceChildren, ", "), ErrUnrenderableInheritance)
	}
	if fks := ownForeignKeys(m); len(fks) != 0 && !opts.ForeignKeys {
		return "", fmt.Errorf("render table %q: owns foreign keys (%s): %w", m.Table, strings.Join(fks, ", "), ErrUnrenderableForeignKey)
	}
	if len(m.ReferencedBy) != 0 && !opts.ForeignKeys {
		return "", fmt.Errorf("render table %q: referenced by foreign keys (%s): %w", m.Table, strings.Join(m.ReferencedBy, ", "), ErrUnrenderableForeignKey)
	}
	if err := unrenderableForeignKey(m); err != nil {
		return "", fmt.Errorf("render table %q: %w", m.Table, err)
	}
	if m.Unlogged {
		return "", fmt.Errorf("render table %q: %w", m.Table, ErrUnrenderableUnlogged)
	}
	if prior, repeat, ok := repeatedIndexConstraint(m.Constraints); ok {
		return "", fmt.Errorf("render table %q: constraint %q repeats %q: %w", m.Table, repeat, prior, ErrUnrenderableMergedConstraints)
	}
	defs := make([]string, 0, len(m.Columns)+len(m.Constraints))
	for _, c := range m.Columns {
		def, err := renderColumnDef(m.Table, c)
		if err != nil {
			return "", fmt.Errorf("render table %q: %w", m.Table, err)
		}
		defs = append(defs, "    "+def)
	}
	// CREATE TABLE ignores NOT VALID, so a constraint the live table has not
	// validated renders as the validated one the file actually declares; the
	// diff then plans its validation.
	for _, con := range m.Constraints {
		def := con.Def
		if con.NotValid {
			def = strings.TrimSuffix(def, notValidSuffix)
		}
		defs = append(defs, "    CONSTRAINT "+pgx.Identifier{con.Name}.Sanitize()+" "+def)
	}

	var b strings.Builder
	b.WriteString("CREATE TABLE " + pgx.Identifier{m.Table}.Sanitize() + " (")
	// CREATE TABLE t () is legal; render it without an empty body line.
	if len(defs) != 0 {
		b.WriteString("\n" + strings.Join(defs, ",\n") + "\n")
	}
	b.WriteString(")")
	if m.PartitionKey != "" {
		b.WriteString(" PARTITION BY " + m.PartitionKey)
	}
	if m.ToastMayNotRebuild {
		return "", fmt.Errorf("render table %q: %w", m.Table, ErrUnrenderableToastOptions)
	}
	if len(m.Options) != 0 {
		opts, err := renderOptions(m.Options)
		if err != nil {
			return "", fmt.Errorf("render table %q: %w", m.Table, err)
		}
		b.WriteString(" WITH (" + opts + ")")
	}
	b.WriteString(";\n")
	for _, ix := range m.Indexes {
		def, err := renderIndexDef(m, ix)
		if err != nil {
			return "", fmt.Errorf("render table %q: %w", m.Table, err)
		}
		b.WriteString("\n" + def + ";\n")
	}

	out := b.String()
	if _, err := statement.ParseDesired(out); err != nil {
		return "", fmt.Errorf("render table %q: output not admissible as a desired schema: %w", m.Table, err)
	}
	return out, nil
}

// repeatedIndexConstraint returns the first pair of PRIMARY KEY, UNIQUE,
// or EXCLUDE constraints whose definitions match, which one CREATE TABLE
// would build as a single constraint. A primary key's definition is
// compared as the UNIQUE one with the same key, because that is the
// comparison the server makes. The repeat is the constraint the server
// would drop: the later one, or the UNIQUE one when the other is the
// primary key, as the desired build reports it.
func repeatedIndexConstraint(cons []Constraint) (prior, repeat string, ok bool) {
	seen := map[string]string{}
	for _, con := range cons {
		def := con.Def
		switch {
		case con.Primary:
			def = "UNIQUE " + strings.TrimPrefix(def, "PRIMARY KEY ")
		case strings.HasPrefix(def, "UNIQUE "), strings.HasPrefix(def, "EXCLUDE "):
		default:
			continue
		}
		if name, dup := seen[def]; dup {
			if con.Primary {
				return con.Name, name, true
			}
			return name, con.Name, true
		}
		seen[def] = con.Name
	}
	return "", "", false
}

// optionName matches a storage parameter name as the server stores it,
// optionally in the toast namespace.
var optionName = regexp.MustCompile(`^(toast\.)?[a-z_][a-z0-9_]*$`)

// renderOptions renders storage parameters for a WITH clause, each value as
// a quoted literal the way pg_dump writes them, so a value is never parsed
// as anything but the parameter's text. A name outside the server's own
// spelling is refused rather than interpolated.
func renderOptions(opts []string) (string, error) {
	rendered := make([]string, 0, len(opts))
	for _, opt := range opts {
		name, value, ok := strings.Cut(opt, "=")
		if !ok || !optionName.MatchString(name) {
			return "", fmt.Errorf("storage parameter %q is not in name=value form", opt)
		}
		rendered = append(rendered, name+"='"+strings.ReplaceAll(value, "'", "''")+"'")
	}
	return strings.Join(rendered, ", "), nil
}

// renderColumnDef renders one column for CREATE TABLE. A serial column is
// rendered back to its pseudo-type so the desired file recreates the owned
// sequence on the scratch schema; every other sequence-backed default is
// refused because the sequence it references cannot exist there. An
// explicit column collation is refused: the model carries it only to keep
// the baseline from silently dropping it. A virtual generated column is
// refused because no desired file can declare one, and so is a sequence
// declaration the column's definition cannot state.
func renderColumnDef(table string, c Column) (string, error) {
	if c.Collation != "" {
		return "", fmt.Errorf("column %q collation %s: %w", c.Name, c.Collation, ErrUnrenderableCollation)
	}
	if c.Virtual {
		return "", fmt.Errorf("column %q: %w", c.Name, ErrUnrenderableVirtual)
	}
	if c.MultipleOwnedSequences {
		return "", fmt.Errorf("column %q: %w", c.Name, ErrMultipleOwnedSequences)
	}
	if c.SequenceDefault {
		st, ok := serialType(table, c)
		if !ok {
			return "", fmt.Errorf("column %q default %q: %w", c.Name, c.Default, ErrUnrenderableDefault)
		}
		if c.Sequence != nil {
			return "", fmt.Errorf("column %q serial sequence declared as %s: %w", c.Name, formatSequence(c.Sequence), ErrUnrenderableSequenceOptions)
		}
		return pgx.Identifier{c.Name}.Sanitize() + " " + st + " NOT NULL", nil
	}
	if err := identityExpressible(c); err != nil {
		return "", err
	}
	return columnDef(c), nil
}

// identityExpressible refuses an identity column whose sequence a column
// definition cannot declare: its options clause sets the bounds, step,
// start, cache, and cycling, but the sequence always takes the column's
// type.
func identityExpressible(c Column) error {
	if c.Identity != IdentityNone && c.Sequence != nil && c.Sequence.Type != c.Type {
		return fmt.Errorf("column %q identity sequence is %s, column is %s: %w", c.Name, c.Sequence.Type, c.Type, ErrUnrenderableSequenceOptions)
	}
	return nil
}

// serialType maps a sequence-backed integer column back to the serial
// pseudo-type it expands from. It requires exactly what the serial
// shorthand produces: an integer-family type, NOT NULL, a sequence the
// column actually owns (the pg_depend OWNED BY edge — a standalone
// sequence that merely happens to carry the serial-style name is not
// ownership, and rendering it as serial would silently convert a shared
// sequence into a private one), and a default of nextval on the sequence
// named <table>_<column>_seq, spelled the way the server prints it
// (serialDefault). A truncated sequence name fails closed to
// ErrUnrenderableDefault.
func serialType(table string, c Column) (string, bool) {
	base, ok := map[string]string{
		"smallint": "smallserial",
		"integer":  "serial",
		"bigint":   "bigserial",
	}[c.Type]
	if !ok || !c.NotNull || !c.SequenceOwned {
		return "", false
	}
	if c.Default != serialDefault(table+"_"+c.Name+"_seq") {
		return "", false
	}
	return base, true
}

// serialDefault is the default the server prints for a serial column whose
// sequence is named sequence: nextval of the sequence's regclass text as a
// string literal. The regclass text quotes the name exactly when
// quote_ident would, so a name with an uppercase letter, such as an ORM's
// "User_id_seq", prints quoted. quote_ident also quotes a keyword, and a
// name ending in _seq is never one. The sequence lives in the table's
// schema, which the introspecting session's search_path lists, so the text
// is unqualified.
func serialDefault(sequence string) string {
	regclass := sequence
	if !plainIdentifier(sequence) {
		regclass = `"` + strings.ReplaceAll(sequence, `"`, `""`) + `"`
	}
	return "nextval('" + strings.ReplaceAll(regclass, "'", "''") + "'::regclass)"
}

// plainIdentifier reports whether quote_ident leaves name unquoted, keywords
// aside: it starts with a lowercase ASCII letter or an underscore, and the
// rest are lowercase ASCII letters, digits, or underscores.
func plainIdentifier(name string) bool {
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}

// unrenderableForeignKey refuses a foreign key of the table that a desired
// file cannot declare faithfully: one into another schema, which a file
// cannot name, and one from the table to itself whose referenced columns are
// unique only through a standalone index, which the file's CREATE TABLE
// would declare before the index exists.
func unrenderableForeignKey(m Model) error {
	for _, con := range m.Constraints {
		switch {
		case !con.ForeignKey:
		case con.ReferencesOtherSchema:
			return fmt.Errorf("foreign key %q references a table in another schema: %w", con.Name, statement.ErrQualifiedName)
		case con.SelfReferencesStandaloneIndex:
			return fmt.Errorf("foreign key %q references the table's own columns through a standalone unique index; declare the index as an immediate UNIQUE constraint first: %w", con.Name, ErrUnrenderableForeignKey)
		}
	}
	return nil
}

// ownForeignKeys returns the names of the foreign keys the table itself
// carries, in model order.
func ownForeignKeys(m Model) []string {
	var names []string
	for _, con := range m.Constraints {
		if con.ForeignKey {
			names = append(names, con.Name)
		}
	}
	return names
}
