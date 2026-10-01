// Package chunksql holds the one statement that puts a chunk of source rows
// into the shadow. The copier runs it for every chunk it lands and the
// verifier runs it for every chunk it repairs; one text in one place is what
// makes a repair put back exactly what a copy would have, and keeps the
// statement off the public API, where a caller could run it outside the
// guard both packages wrap around it.
package chunksql

import (
	"strings"

	"github.com/jackc/pgx/v5"
)

// Insert is the statement that copies one chunk: insert the shared columns
// of the source rows whose key lies in the closed range [$1, $2] into the
// shadow, skipping any key the shadow already holds — the applier always
// overwrites and the copier never does, which is what lets the two run
// concurrently (CO-4). The bounds are declared bigint whatever the key's
// integer type, as the chunker's boundary query declares them, so a bound
// outside a smaller key type's range can still be sent and the primary-key
// index still serves the range scan. The statement carries no conversion
// expression: a column whose type differs between the two tables is
// converted by the server's assignment cast, so a type change that needs a
// USING expression cannot be copied by this statement and must not be
// routed to the copier until it can carry one. Every identifier is quoted
// and the relations are schema-qualified.
func Insert(schema, source, shadow, key string, columns []string) string {
	quoted := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted = append(quoted, pgx.Identifier{column}.Sanitize())
	}
	list := strings.Join(quoted, ", ")
	pk := pgx.Identifier{key}.Sanitize()
	return "INSERT INTO " + pgx.Identifier{schema, shadow}.Sanitize() +
		" (" + list + ")" +
		" SELECT " + list +
		" FROM " + pgx.Identifier{schema, source}.Sanitize() +
		" WHERE " + pk + " BETWEEN $1::bigint AND $2::bigint" +
		" ON CONFLICT (" + pk + ") DO NOTHING"
}
