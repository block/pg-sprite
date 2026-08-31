package schemadiff

import (
	"errors"
	"fmt"
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
// table's relationships. The renderer refuses instead. Outgoing foreign
// keys are refused separately by the desired-file grammar
// (statement.ErrForeignKey).
var ErrUnrenderableForeignKey = errors.New("tables referenced by foreign keys cannot be rendered as a desired schema")

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
// CREATE TABLE followed by the model's CREATE INDEX statements. The output
// is proven admissible by parsing it through statement.ParseDesired before
// it is returned, so anything a desired file refuses (a foreign key, for
// example) surfaces here as that gate's typed error. Materializing the
// output with IntrospectDesired reproduces the model, so diffing it against
// the table it came from yields no changes — the round-trip contract the
// integration tests enforce.
func Render(m Model) (string, error) {
	if m.PartitionKey != "" {
		return "", fmt.Errorf("render table %q: partitioned parent (PARTITION BY %s): %w", m.Table, m.PartitionKey, ErrUnrenderablePartition)
	}
	if m.IsPartition {
		return "", fmt.Errorf("render table %q: partition of a partitioned parent: %w", m.Table, ErrUnrenderablePartition)
	}
	if len(m.InheritsParents) != 0 {
		return "", fmt.Errorf("render table %q: inherits from %s: %w", m.Table, strings.Join(m.InheritsParents, ", "), ErrUnrenderableInheritance)
	}
	if len(m.InheritanceChildren) != 0 {
		return "", fmt.Errorf("render table %q: has inheritance children %s: %w", m.Table, strings.Join(m.InheritanceChildren, ", "), ErrUnrenderableInheritance)
	}
	if len(m.ReferencedBy) != 0 {
		return "", fmt.Errorf("render table %q: referenced by foreign keys (%s): %w", m.Table, strings.Join(m.ReferencedBy, ", "), ErrUnrenderableForeignKey)
	}
	if m.Unlogged {
		return "", fmt.Errorf("render table %q: %w", m.Table, ErrUnrenderableUnlogged)
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

// renderColumnDef renders one column for CREATE TABLE. A serial column is
// rendered back to its pseudo-type so the desired file recreates the owned
// sequence on the scratch schema; every other sequence-backed default is
// refused because the sequence it references cannot exist there. An
// explicit column collation is refused: the model carries it only to keep
// the baseline from silently dropping it.
func renderColumnDef(table string, c Column) (string, error) {
	if c.Collation != "" {
		return "", fmt.Errorf("column %q collation %s: %w", c.Name, c.Collation, ErrUnrenderableCollation)
	}
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
