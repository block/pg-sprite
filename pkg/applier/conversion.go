package applier

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
)

// conversion is how a flush writes one column the gated statement retyped
// with USING: the expression the shadow proof carries for it, and the
// source's type for the column, which a value from the stream or from the
// source row is bound as before the expression converts it.
type conversion struct {
	expr       string
	sourceType string
}

// conversions is the flush's view of the shadow proof's Conversions, keyed
// by column, completed with each column's source type; it is empty for a
// shadow whose proof carries no conversions.
type conversions map[string]conversion

// shadowForm marks a value read from the shadow row for a converted
// column: it is already the converted value, as the shadow renders it, and
// the write binds it bare for the shadow column's input function to read.
// Every other value a flush writes is in the source's form — the text
// pgoutput carried, or the source row rendered as text — and is converted
// through the column's expression. NULL carries its form too, since an
// expression can turn a source NULL into a value.
type shadowForm struct {
	value any
}

// sourceColumnTypesSQL reads the type of each named column as the source
// ($1) declares it, with every catalog name pg_catalog-qualified so the
// session's search_path plays no part (CO-9). format_type qualifies every
// type the transaction's search_path does not reach, and the write's cast
// resolves the rest on that same path, which the guard set before this
// ran: the build's path, since a flush with conversions runs under it.
const sourceColumnTypesSQL = `
	SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
	FROM pg_catalog.pg_attribute a
	WHERE a.attrelid = $1::oid
	  AND a.attnum > 0
	  AND NOT a.attisdropped
	  AND a.attname::text OPERATOR(pg_catalog.=) ANY($2::text[])`

// readConversions pairs each converted column of the shadow proof with the
// source's type for it, read inside the guarded transaction so the source
// whose types they are is the relation the proof describes. A converted
// column the source no longer has is a source that is not the one the
// proof describes. It returns an empty map, without a read, for a proof
// with no conversions.
func readConversions(ctx context.Context, tx pgx.Tx, shadow copier.Shadow) (conversions, error) {
	exprs := shadow.Conversions()
	if len(exprs) == 0 {
		return conversions{}, nil
	}
	columns := make([]string, 0, len(exprs))
	for column := range exprs {
		columns = append(columns, column)
	}
	rows, err := tx.Query(ctx, sourceColumnTypesSQL, shadow.SourceOID(), columns)
	if err != nil {
		return nil, fmt.Errorf("read column types of %s.%s: %w", shadow.Schema(), shadow.SourceTable(), err)
	}
	types, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (columnType, error) {
		var c columnType
		err := row.Scan(&c.name, &c.typeName)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read column types of %s.%s: %w", shadow.Schema(), shadow.SourceTable(), err)
	}
	out := make(conversions, len(exprs))
	for _, c := range types {
		out[c.name] = conversion{expr: exprs[c.name], sourceType: c.typeName}
	}
	for column := range exprs {
		if _, ok := out[column]; !ok {
			// INV: ST-6
			return nil, fmt.Errorf("%w (ST-6): %s.%s has no column %s the shadow proof converts", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), column)
		}
	}
	return out, nil
}

// columnType pairs a column with its type as format_type renders it: the
// SQL spelling a cast can name.
type columnType struct {
	name     string
	typeName string
}

// valueSQL renders the value a statement writes for column c bound at
// parameter n: the bare parameter for a column the statement does not
// convert, or for a value already in the shadow's form; otherwise the
// column's USING expression over the parameter bound as the source's type,
// so the expression sees the value as the source column held it. The
// expression is run as a scalar subquery whose one input column bears the
// column's own name, which is the only name the statement admits it to
// refer to.
func valueSQL(n int, c decode.Column, conv conversions) string {
	param := "$" + strconv.Itoa(n)
	cv, converted := conv[c.Name]
	if !converted {
		return param
	}
	if _, already := c.Value.(shadowForm); already {
		return param
	}
	name := pgx.Identifier{c.Name}.Sanitize()
	return "(SELECT " + cv.expr + " FROM (SELECT " + param + "::" + cv.sourceType + " AS " + name + ") AS \"source\")"
}

// bindValue is the value bound for a column: the value itself, or the
// value a shadowForm wraps.
func bindValue(c decode.Column) any {
	if sf, ok := c.Value.(shadowForm); ok {
		return sf.value
	}
	return c.Value
}
