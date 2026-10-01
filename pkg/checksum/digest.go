package checksum

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/preflight"
)

// Digest is one side's summary of a chunk: how many rows the range holds
// and an order-sensitive hash of every one of them.
type Digest struct {
	// Rows is the number of rows whose key lies in the chunk.
	Rows int64
	// Hash is md5 over the concatenation, in key order, of the per-row md5
	// of the row's copy columns rendered as a record. It is the md5 of the
	// empty string for an empty range.
	Hash string
}

// columnType pairs a copy column with the type the shadow declares for it,
// as format_type renders it: the SQL spelling a cast can name.
type columnType struct {
	name     string
	typeName string
}

// shadowColumnTypesSQL reads the type of every copy column as the shadow
// ($1) declares it. Every catalog name is pg_catalog-qualified so the
// session's search_path plays no part (CO-9), and the session's search_path
// is pg_catalog alone while it runs, so format_type qualifies every type
// that is not built in.
const shadowColumnTypesSQL = `
	SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
	FROM pg_catalog.pg_attribute a
	WHERE a.attrelid = $1::oid
	  AND a.attnum > 0
	  AND NOT a.attisdropped
	  AND a.attname::text OPERATOR(pg_catalog.=) ANY($2::text[])`

// shadowColumnTypes returns the copy columns in the order the shadow proof
// lists them, each with the shadow's type. A copy column the shadow no
// longer has is a shadow that is not the one the proof describes.
func shadowColumnTypes(ctx context.Context, tx pgx.Tx, shadow copier.Shadow) ([]columnType, error) {
	rows, err := tx.Query(ctx, shadowColumnTypesSQL, shadow.ShadowOID(), shadow.CopyColumns())
	if err != nil {
		return nil, fmt.Errorf("read column types of shadow %s.%s: %w", shadow.Schema(), shadow.ShadowTable(), err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (columnType, error) {
		var c columnType
		err := row.Scan(&c.name, &c.typeName)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("read column types of shadow %s.%s: %w", shadow.Schema(), shadow.ShadowTable(), err)
	}
	byName := make(map[string]string, len(found))
	for _, c := range found {
		byName[c.name] = c.typeName
	}
	types := make([]columnType, 0, len(shadow.CopyColumns()))
	for _, column := range shadow.CopyColumns() {
		typeName, ok := byName[column]
		if !ok {
			// INV: ST-6
			return nil, fmt.Errorf("%w (ST-6): shadow %s.%s has no column %s the proof lists for copy", ErrInvariantViolation, shadow.Schema(), shadow.ShadowTable(), column)
		}
		types = append(types, columnType{name: column, typeName: typeName})
	}
	return types, nil
}

// digestSQL is the one statement run on each side of a chunk, with the
// table it reads the only difference between the two: the row count and
// the md5 of the key-ordered concatenation of every row's md5, each row
// rendered as a record of its copy columns cast to the shadow's types —
// the D7 rule of docs/copy-and-swap-design.md#d7--checksum-through-the-destination-types.
// The source side is where the casts do work: a column whose type the
// schema change widens or narrows hashes as the value the shadow holds,
// and both sides run the identical expression so nothing but the data can
// differ. The record rendering tells NULL from the empty string, and
// hashing per row before aggregating bounds the aggregate's input to one
// md5 per row. The bounds are declared bigint whatever the key's integer
// type, as the chunker's boundary query declares them, so the primary-key
// index serves the range scan. Every function is pg_catalog-qualified, and
// the guarded session's search_path is pg_catalog alone for the operators
// and casts that cannot be, so the session's own search_path plays no part
// (CO-9); COALESCE is syntax, not a function, and takes no qualifier.
func digestSQL(target preflight.CopySwapTarget, schema, table string, types []columnType) string {
	cast := make([]string, 0, len(types))
	for _, c := range types {
		cast = append(cast, pgx.Identifier{c.name}.Sanitize()+"::"+c.typeName)
	}
	key := pgx.Identifier{target.PKColumn()}.Sanitize()
	return "SELECT pg_catalog.count(*)," +
		" pg_catalog.md5(COALESCE(pg_catalog.string_agg(pg_catalog.md5(ROW(" + strings.Join(cast, ", ") + ")::text), '' ORDER BY " + key + "), ''))" +
		" FROM " + pgx.Identifier{schema, table}.Sanitize() +
		" WHERE " + key + " BETWEEN $1::bigint AND $2::bigint"
}

// digest runs one side's statement for the closed range [lower, upper].
func digest(ctx context.Context, tx pgx.Tx, sql string, lower, upper int64) (Digest, error) {
	var d Digest
	if err := tx.QueryRow(ctx, sql, lower, upper).Scan(&d.Rows, &d.Hash); err != nil {
		return Digest{}, err
	}
	return d, nil
}
