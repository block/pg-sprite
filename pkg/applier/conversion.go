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
// source's type and collation for the column, which a value from the
// stream or from the source row is bound as before the expression converts
// it. The collation is empty for a column that collates as its type does
// by default; a column declared with its own is bound under it, so an
// expression that compares or orders the value sees what it saw over the
// source column in the copy and sees in the digest.
type conversion struct {
	expr       string
	sourceType string
	collation  string
}

// conversions is the flush's view of the shadow proof's Conversions, keyed
// by column, completed with each column's source type; it is empty for a
// shadow whose proof carries no conversions.
type conversions map[string]conversion

// shadowForm marks a value read from the shadow row: for a converted column
// it is already the converted value, as the shadow renders it, and the write
// binds it bare for the shadow column's input function to read. Every other
// value a flush writes is in the source's form — the text pgoutput carried,
// or the source row rendered as text — and a converted column's is
// converted through its expression. NULL carries its form too, since an
// expression can turn a source NULL into a value. A column the statement
// does not convert is bound bare in either form.
type shadowForm struct {
	value any
}

// inShadowForm returns cols with each value marked as read from the shadow
// row, as Hold keeps a completion read from the shadow. A column with no
// value has nothing to mark and is carried as it is.
func inShadowForm(cols []decode.Column) []decode.Column {
	out := make([]decode.Column, len(cols))
	for i, c := range cols {
		value, present := c.Get()
		if !present {
			out[i] = c
			continue
		}
		out[i] = decode.PresentColumn(c.Name(), shadowForm{value: value})
	}
	return out
}

// isShadowForm reports whether the column carries a value read from the
// shadow row.
func isShadowForm(c decode.Column) bool {
	value, present := c.Get()
	if !present {
		return false
	}
	_, ok := value.(shadowForm)
	return ok
}

// sourceColumnTypesSQL reads the type and collation of each named column as
// the source ($1) declares it, with every catalog name pg_catalog-qualified
// so the session's search_path plays no part (CO-9). format_type qualifies
// every type the transaction's search_path does not reach, and the write's
// cast resolves the rest on that same path, which the guard set before this
// ran: the build's path, since a flush with conversions runs under it. The
// collation is NULL for a column that collates as its type does by default,
// and otherwise the column's own, schema-qualified and quoted, as the
// column's definition would spell it.
const sourceColumnTypesSQL = `
	SELECT a.attname,
	       pg_catalog.format_type(a.atttypid, a.atttypmod),
	       CASE WHEN a.attcollation <> 0 AND a.attcollation <> t.typcollation
	            THEN pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(c.collname)
	       END
	FROM pg_catalog.pg_attribute a
	JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
	LEFT JOIN pg_catalog.pg_collation c ON c.oid = a.attcollation
	LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid = c.collnamespace
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
		var collation *string
		err := row.Scan(&c.name, &c.typeName, &collation)
		if collation != nil {
			c.collation = *collation
		}
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read column types of %s.%s: %w", shadow.Schema(), shadow.SourceTable(), err)
	}
	out := make(conversions, len(exprs))
	for _, c := range types {
		out[c.name] = conversion{expr: exprs[c.name], sourceType: c.typeName, collation: c.collation}
	}
	for column := range exprs {
		if _, ok := out[column]; !ok {
			// INV: ST-6
			return nil, fmt.Errorf("%w (ST-6): %s.%s has no column %s the shadow proof converts", ErrInvariantViolation, shadow.Schema(), shadow.SourceTable(), column)
		}
	}
	return out, nil
}

// columnType pairs a column with its type as format_type renders it, the
// SQL spelling a cast can name, and the collation it declares beyond the
// type's own, or "" for none.
type columnType struct {
	name      string
	typeName  string
	collation string
}

// valueSQL renders the value a statement writes for column c bound at
// parameter n: the bare parameter for a column the statement does not
// convert, or for a value already in the shadow's form; otherwise the
// column's USING expression over the parameter bound as the source's type,
// and under the source column's own collation when it declares one, so the
// expression sees the value as the source column held it. The expression
// is run as a scalar subquery whose one input column bears the column's
// own name, which is the only name the statement admits it to refer to.
func valueSQL(n int, c decode.Column, conv conversions) string {
	param := "$" + strconv.Itoa(n)
	cv, converted := conv[c.Name()]
	if !converted {
		return param
	}
	if isShadowForm(c) {
		return param
	}
	name := pgx.Identifier{c.Name()}.Sanitize()
	return "(SELECT " + cv.expr + " FROM (SELECT " + cv.sourceValue(param) + " AS " + name + ") AS \"source\")"
}

// sourceValue renders param as the source column held it: cast to the
// source's type, and collated as the column declares when that is not the
// type's default.
func (cv conversion) sourceValue(param string) string {
	value := param + "::" + cv.sourceType
	if cv.collation == "" {
		return value
	}
	return value + " COLLATE " + cv.collation
}

// bindValue is the value bound for a column: the value itself, or the
// value a shadowForm wraps. A column with no value has nothing to bind:
// binding one would write a value pgoutput never carried over the shadow's
// (CO-8), so the flush refuses instead.
func bindValue(c decode.Column) (any, error) {
	value, present := c.Get()
	if !present {
		// INV: CO-8 — a column with no value is refused, never bound as NULL.
		return nil, fmt.Errorf("%w (CO-8): column %s carries no value to bind", ErrInvariantViolation, c.Name())
	}
	if sf, ok := value.(shadowForm); ok {
		return sf.value, nil
	}
	return value, nil
}
