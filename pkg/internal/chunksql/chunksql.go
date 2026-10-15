// Package chunksql holds the one statement that puts a chunk of source rows
// into the shadow, and the search_path it runs under. The copier runs the
// statement for every chunk it lands and the verifier runs it for every
// chunk it repairs; one text in one place is what makes a repair put back
// exactly what a copy would have, and keeps the statement off the public
// API, where a caller could run it outside the guard both packages wrap
// around it.
package chunksql

import (
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// Insert is the statement that copies one chunk: insert the shared columns
// of the source rows whose key lies in the closed range [$1, $2] into the
// shadow, skipping any key the shadow already holds — the applier always
// overwrites and the copier never does, which is what lets the two run
// concurrently (CO-4). The bounds are declared bigint whatever the key's
// integer type, as the chunker's boundary query declares them, so a bound
// outside a smaller key type's range can still be sent and the primary-key
// index still serves the range scan.
//
// A column named in conversions is selected as that expression — the
// USING clause the gated statement retyped it with, as pkg/statement
// renders it — so the copy converts each value the way the schema change
// declared, and a value the expression refuses fails the chunk the way the
// statement would have failed on the source. Every other column is
// selected bare and converted, where the two tables' types differ, by the
// server's assignment cast. Every identifier is quoted and the relations
// are schema-qualified; the expressions are the user's text, which is why
// SearchPath puts the source schema on the path they resolve in.
func Insert(schema, source, shadow, key string, columns []string, conversions map[string]string) string {
	targets := make([]string, 0, len(columns))
	selected := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted := pgx.Identifier{column}.Sanitize()
		targets = append(targets, quoted)
		if expr, ok := conversions[column]; ok {
			selected = append(selected, expr+" AS "+quoted)
			continue
		}
		selected = append(selected, quoted)
	}
	pk := pgx.Identifier{key}.Sanitize()
	return "INSERT INTO " + pgx.Identifier{schema, shadow}.Sanitize() +
		" (" + strings.Join(targets, ", ") + ")" +
		" SELECT " + strings.Join(selected, ", ") +
		" FROM " + pgx.Identifier{schema, source}.Sanitize() +
		" WHERE " + pk + " BETWEEN $1::bigint AND $2::bigint" +
		" ON CONFLICT (" + pk + ") DO NOTHING"
}

// SearchPath is the transaction-local path statement, built by dbconn, for
// a transaction that runs Insert. Without conversions the statement names nothing the path
// resolves — every relation is qualified and the catalog answers the key
// range's operators — so the path is the catalog alone, and a schema ahead
// of it on the caller's pool cannot answer a shadow default or trigger
// (CO-9). With conversions the statement carries the user's expressions,
// whose unqualified types, functions, and operators must resolve as they
// did when the gated statement ran on the shadow: the path is the one the
// shadow build sets, the source schema then public. Either way dbconn
// builds the path, so pg_catalog stays ahead of both.
func SearchPath(schema string, converted bool) string {
	// INV: CO-9
	if !converted {
		return dbconn.LocalSearchPath("pg_catalog")
	}
	return dbconn.LocalSearchPath(schema, "public")
}
