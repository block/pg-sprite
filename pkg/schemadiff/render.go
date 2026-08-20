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

// Render renders the canonical model into a desired-state schema file: one
// CREATE TABLE followed by the model's CREATE INDEX statements. The output
// is proven admissible by parsing it through statement.ParseDesired before
// it is returned, so anything a desired file refuses (a foreign key, for
// example) surfaces here as that gate's typed error. Materializing the
// output with IntrospectDesired reproduces the model, so diffing it against
// the table it came from yields no changes — the round-trip contract the
// integration tests enforce.
func Render(m Model) (string, error) {
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
	b.WriteString("CREATE TABLE " + pgx.Identifier{m.Table}.Sanitize() + " (\n")
	b.WriteString(strings.Join(defs, ",\n"))
	b.WriteString("\n);\n")
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
// refused because the sequence it references cannot exist there.
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
// shorthand produces: an integer-family type, NOT NULL, and a default of
// nextval on the owned sequence named <table>_<column>_seq. The name check
// covers only names that need no quoting inside the nextval literal;
// exotic or truncated sequence names fail closed to ErrUnrenderableDefault.
func serialType(table string, c Column) (string, bool) {
	base, ok := map[string]string{
		"smallint": "smallserial",
		"integer":  "serial",
		"bigint":   "bigserial",
	}[c.Type]
	if !ok || !c.NotNull {
		return "", false
	}
	if c.Default != "nextval('"+table+"_"+c.Name+"_seq'::regclass)" {
		return "", false
	}
	return base, true
}
