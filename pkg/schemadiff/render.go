package schemadiff

import (
	"errors"
	"fmt"
	"slices"
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

// ErrUnrenderablePartition is returned for a partitioned parent or a
// partition. The model does not carry partition bounds or the
// parent/partition topology, so a rendered file would silently lose the
// PARTITION BY clause or the partition attachment — the renderer refuses
// instead of emitting a wrong baseline.
var ErrUnrenderablePartition = errors.New("partitioned tables cannot be rendered as a desired schema")

// ErrUnrenderableInheritance is returned for either side of a classic
// PostgreSQL inheritance relationship. The desired model cannot express
// inheritance edges, so rendering would flatten a child or omit its children.
var ErrUnrenderableInheritance = errors.New("table inheritance cannot be rendered as a desired schema")

// ErrUnrenderableForeignKey is returned when other tables reference this
// one with foreign keys. A desired file cannot declare foreign keys, so
// the single-table model carries no incoming foreign-key topology — a
// rendered baseline would look complete while silently dropping the
// table's relationships. The renderer refuses instead. The table's own
// foreign keys are refused under the desired-file grammar's sentinel
// (statement.ErrForeignKey), named by constraint.
var ErrUnrenderableForeignKey = errors.New("tables referenced by foreign keys cannot be rendered as a desired schema")

// RenderRefusal is the error Render returns when the model carries anything
// a desired schema file cannot express. It gathers every cause rather than
// stopping at the first, so a caller that has to explain why a live table
// cannot be declared can name all of them, and it unwraps to each cause's
// sentinel so errors.Is keeps matching for a caller that branches on one.
type RenderRefusal struct {
	// Table is the unqualified table name.
	Table string
	// Causes lists what the desired schema cannot express, table-level
	// properties first and then each column's, in attribute order. It is
	// never empty.
	Causes []RenderCause
}

// RenderCause is one property of the model a desired schema file cannot
// express.
type RenderCause struct {
	// Err is the sentinel that names the property: ErrUnrenderablePartition,
	// ErrUnrenderableInheritance, statement.ErrForeignKey for the table's
	// own foreign keys, ErrUnrenderableForeignKey for the foreign keys that
	// reference it, ErrUnrenderableUnlogged, ErrUnrenderableCollation, or
	// ErrUnrenderableDefault.
	Err error
	// Objects are the catalog names the property is about: the table's own
	// foreign key constraints, "table.constraint" for the foreign keys that
	// reference it, the relations on the other side of an inheritance edge,
	// the column carrying a collation or a sequence-backed default. It is
	// empty for a property of the table as a whole.
	Objects []string
	// Detail is the phrase Error prints for the cause, carrying what the
	// names alone do not: the partition key, the collation, the default.
	Detail string
}

// Error joins every cause: a table refused for one reason reads as a single
// clause, and a table refused for several names them all.
func (e *RenderRefusal) Error() string {
	parts := make([]string, len(e.Causes))
	for i, cause := range e.Causes {
		parts[i] = cause.Detail + ": " + cause.Err.Error()
	}
	return fmt.Sprintf("render table %q: %s", e.Table, strings.Join(parts, "; "))
}

// Unwrap exposes every cause's sentinel to errors.Is.
func (e *RenderRefusal) Unwrap() []error {
	errs := make([]error, len(e.Causes))
	for i, cause := range e.Causes {
		errs[i] = cause.Err
	}
	return errs
}

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

// Render renders the canonical model into a desired-state schema file: one
// CREATE TABLE followed by the model's CREATE INDEX statements. A model
// carrying anything a desired file cannot express is refused with a
// *RenderRefusal that names every such property, so one call gives the
// whole answer. The output is proven admissible by parsing it through
// statement.ParseDesired before it is returned, so a property the refusals
// do not model but the desired-file grammar refuses still cannot become a
// baseline. Materializing the output with IntrospectDesired reproduces the
// model's names and definitions, so diffing it against the table it came
// from yields no changes — the round-trip contract the integration tests
// enforce. Validity is not rendered: an index the table carries invalid —
// an unfinished concurrent build — renders as its definition and
// materializes valid, so the round-trip diff of such a table is exactly the
// create-index change that rebuilds it.
func Render(m Model) (string, error) {
	if causes := renderRefusals(m); len(causes) != 0 {
		return "", &RenderRefusal{Table: m.Table, Causes: causes}
	}
	defs := make([]string, 0, len(m.Columns)+len(m.Constraints))
	for _, c := range m.Columns {
		def, err := renderColumnDef(m.Table, c)
		if err != nil {
			return "", fmt.Errorf("render table %q: %w", m.Table, err)
		}
		defs = append(defs, "    "+def)
	}
	for _, con := range m.Constraints {
		defs = append(defs, "    CONSTRAINT "+pgx.Identifier{con.Name}.Sanitize()+" "+con.Def)
	}

	var b strings.Builder
	b.WriteString("CREATE TABLE " + pgx.Identifier{m.Table}.Sanitize() + " (")
	// CREATE TABLE t () is legal; render it without an empty body line.
	if len(defs) != 0 {
		b.WriteString("\n" + strings.Join(defs, ",\n") + "\n")
	}
	b.WriteString(");\n")
	for _, ix := range m.Indexes {
		b.WriteString("\n" + ix.Def + ";\n")
	}

	out := b.String()
	if _, err := statement.ParseDesired(out); err != nil {
		return "", fmt.Errorf("render table %q: output not admissible as a desired schema: %w", m.Table, err)
	}
	return out, nil
}

// renderRefusals collects every property of the model a desired schema file
// cannot express: the table's own first, then each column's in attribute
// order. An empty result means the model renders.
func renderRefusals(m Model) []RenderCause {
	var causes []RenderCause
	if m.PartitionKey != "" {
		causes = append(causes, RenderCause{
			Err:    ErrUnrenderablePartition,
			Detail: fmt.Sprintf("partitioned parent (PARTITION BY %s)", m.PartitionKey),
		})
	}
	if m.IsPartition {
		causes = append(causes, RenderCause{
			Err:    ErrUnrenderablePartition,
			Detail: "partition of a partitioned parent",
		})
	}
	if len(m.InheritsParents) != 0 {
		causes = append(causes, RenderCause{
			Err:     ErrUnrenderableInheritance,
			Objects: slices.Clone(m.InheritsParents),
			Detail:  "inherits from " + strings.Join(m.InheritsParents, ", "),
		})
	}
	if len(m.InheritanceChildren) != 0 {
		causes = append(causes, RenderCause{
			Err:     ErrUnrenderableInheritance,
			Objects: slices.Clone(m.InheritanceChildren),
			Detail:  "has inheritance children " + strings.Join(m.InheritanceChildren, ", "),
		})
	}
	if own := foreignKeyNames(m.Constraints); len(own) != 0 {
		causes = append(causes, RenderCause{
			Err:     statement.ErrForeignKey,
			Objects: own,
			Detail:  "foreign key constraint(s) " + strings.Join(own, ", "),
		})
	}
	if len(m.ReferencedBy) != 0 {
		causes = append(causes, RenderCause{
			Err:     ErrUnrenderableForeignKey,
			Objects: slices.Clone(m.ReferencedBy),
			Detail:  fmt.Sprintf("referenced by foreign keys (%s)", strings.Join(m.ReferencedBy, ", ")),
		})
	}
	if m.Unlogged {
		causes = append(causes, RenderCause{
			Err:    ErrUnrenderableUnlogged,
			Detail: "unlogged table",
		})
	}
	for _, c := range m.Columns {
		causes = append(causes, columnRefusals(m.Table, c)...)
	}
	return causes
}

// columnRefusals collects what a desired file cannot express about one
// column: an explicit collation, which the model carries only to keep a
// baseline from silently dropping it, and a sequence-backed default that
// is not the serial shorthand, because the sequence it references cannot
// exist on the scratch schema.
func columnRefusals(table string, c Column) []RenderCause {
	var causes []RenderCause
	if c.Collation != "" {
		causes = append(causes, RenderCause{
			Err:     ErrUnrenderableCollation,
			Objects: []string{c.Name},
			Detail:  fmt.Sprintf("column %q collation %s", c.Name, c.Collation),
		})
	}
	if c.SequenceDefault {
		if _, ok := serialType(table, c); !ok {
			causes = append(causes, RenderCause{
				Err:     ErrUnrenderableDefault,
				Objects: []string{c.Name},
				Detail:  fmt.Sprintf("column %q default %q", c.Name, c.Default),
			})
		}
	}
	return causes
}

// foreignKeyNames returns the names of the FOREIGN KEY constraints among
// cons, in the model's (name-sorted) order.
func foreignKeyNames(cons []Constraint) []string {
	var names []string
	for _, con := range cons {
		if con.ForeignKey {
			names = append(names, con.Name)
		}
	}
	return names
}

// renderColumnDef renders one column for CREATE TABLE. A serial column is
// rendered back to its pseudo-type so the desired file recreates the owned
// sequence on the scratch schema. Any other sequence-backed default was
// refused by renderRefusals before this runs; the check here keeps the
// renderer fail-closed should the two ever disagree.
func renderColumnDef(table string, c Column) (string, error) {
	if c.SequenceDefault {
		st, ok := serialType(table, c)
		if !ok {
			return "", fmt.Errorf("column %q default %q: %w", c.Name, c.Default, ErrUnrenderableDefault)
		}
		return pgx.Identifier{c.Name}.Sanitize() + " " + st + " NOT NULL", nil
	}
	return columnDef(c), nil
}

// serialType maps a sequence-backed integer column back to the serial
// pseudo-type it expands from. It requires exactly what the serial
// shorthand produces: an integer-family type, NOT NULL, a sequence the
// column actually owns (the pg_depend OWNED BY edge — a standalone
// sequence that merely happens to carry the serial-style name is not
// ownership, and rendering it as serial would silently convert a shared
// sequence into a private one), and a default of nextval on the sequence
// named <table>_<column>_seq. The name check covers only names that need
// no quoting inside the nextval literal; exotic or truncated sequence
// names fail closed to ErrUnrenderableDefault.
func serialType(table string, c Column) (string, bool) {
	base, ok := map[string]string{
		"smallint": "smallserial",
		"integer":  "serial",
		"bigint":   "bigserial",
	}[c.Type]
	if !ok || !c.NotNull || !c.SequenceOwned {
		return "", false
	}
	if c.Default != "nextval('"+table+"_"+c.Name+"_seq'::regclass)" {
		return "", false
	}
	return base, true
}
