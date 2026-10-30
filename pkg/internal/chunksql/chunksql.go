// Package chunksql holds the statements that put a chunk of source rows
// into the shadow, and the search_path and output settings they run under.
// The copier runs Insert for every chunk it lands and the verifier runs it
// for every chunk it repairs; one text in one place is what makes a repair
// put back exactly what a copy would have, and keeps the statement off the
// public API, where a caller could run it outside the guard both packages
// wrap around it. InsertSkipping and InsertKeys are the copier's statements
// for a chunk whose Insert collided; they select the rows exactly as Insert
// does. The applier's flush shares the path and the settings, since it
// evaluates the same conversion expressions.
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
	targets, selected := columnLists(columns, conversions)
	pk := pgx.Identifier{key}.Sanitize()
	return "INSERT INTO " + pgx.Identifier{schema, shadow}.Sanitize() +
		" (" + targets + ")" +
		" SELECT " + selected +
		" FROM " + pgx.Identifier{schema, source}.Sanitize() +
		" WHERE " + pk + " BETWEEN $1::bigint AND $2::bigint" +
		" ON CONFLICT (" + pk + ") DO NOTHING"
}

// InsertSkipping is the statement the copier runs over a chunk whose Insert
// collided: the same rows, selected the same way, inserted with ON CONFLICT
// DO NOTHING on every unique index and exclusion constraint rather than on
// the key alone, so a source row whose value a shadow row still holds under
// another key is left out instead of failing the chunk. It returns one row:
// the number of rows it inserted, and the ascending bigint keys of the
// source rows it read that are in the shadow neither now nor from before —
// the rows still held off, which the copier runs InsertKeys for once the
// catch-up has rewritten the rows holding their values (CO-4). Every part of
// the statement runs on the one snapshot its start took, so the shadow it
// reads for "from before" is the shadow before this statement's own
// inserts, which the insert's RETURNING accounts for instead.
func InsertSkipping(schema, source, shadow, key string, columns []string, conversions map[string]string) string {
	pk := pgx.Identifier{key}.Sanitize()
	return insertPending(schema, source, shadow, pk, columns, conversions, pk+" BETWEEN $1::bigint AND $2::bigint")
}

// InsertKeys is InsertSkipping over the source rows whose key is in the
// bigint array $1 rather than over a key range: the copier's statement for
// the keys an earlier pass left held off, and only those, since a source row
// that arrived in the chunk after that pass is the catch-up's to write.
func InsertKeys(schema, source, shadow, key string, columns []string, conversions map[string]string) string {
	pk := pgx.Identifier{key}.Sanitize()
	return insertPending(schema, source, shadow, pk, columns, conversions, pk+" = ANY($1::bigint[])")
}

// insertPending renders the skipping insert over the source rows where
// matches. The source rows are read once into a materialized CTE so the
// insert and the held-off query see the same rows; the insert's RETURNING
// names what landed, and a read row that neither landed nor was already in
// the shadow is held off.
func insertPending(schema, source, shadow, pk string, columns []string, conversions map[string]string, where string) string {
	targets, selected := columnLists(columns, conversions)
	shadowName := pgx.Identifier{schema, shadow}.Sanitize()
	return "WITH src AS MATERIALIZED (" +
		"SELECT " + selected +
		" FROM " + pgx.Identifier{schema, source}.Sanitize() +
		" WHERE " + where + ")," +
		" ins AS (INSERT INTO " + shadowName + " (" + targets + ")" +
		" SELECT " + targets + " FROM src" +
		" ON CONFLICT DO NOTHING RETURNING " + pk + ")" +
		" SELECT (SELECT count(*) FROM ins)," +
		" ARRAY(SELECT src." + pk + "::bigint FROM src" +
		" WHERE NOT EXISTS (SELECT 1 FROM ins WHERE ins." + pk + " = src." + pk + ")" +
		" AND NOT EXISTS (SELECT 1 FROM " + shadowName + " sh WHERE sh." + pk + " = src." + pk + ")" +
		" ORDER BY src." + pk + ")"
}

// columnLists renders the insert's column list and the matching select
// list: every column quoted, a converted column selected as its expression
// aliased to the column it lands in.
func columnLists(columns []string, conversions map[string]string) (targets, selected string) {
	targetList := make([]string, 0, len(columns))
	selectedList := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted := pgx.Identifier{column}.Sanitize()
		targetList = append(targetList, quoted)
		if expr, ok := conversions[column]; ok {
			selectedList = append(selectedList, expr+" AS "+quoted)
			continue
		}
		selectedList = append(selectedList, quoted)
	}
	return strings.Join(targetList, ", "), strings.Join(selectedList, ", ")
}

// Rendering pins, for the rest of the transaction, the two output settings
// under which one value has more than one text rendering: extra_float_digits
// at its maximum, so float4, float8, and the geometric types render every
// digit rather than rounding to fifteen at zero or below, and bytea_output
// at hex rather than escape. The server classifies a float or bytea cast
// to text as immutable, so a conversion such as `value::text` passes the
// shadow build's probe although its result follows these settings; the
// chunk copy, the verifier's recopy and digest, and the catch-up's flush
// each evaluate it in a session of their own, and a database or role
// configured differently from the copier's pool would otherwise land one
// rendering from the copy and another from the flush, which the digest then
// reads as divergence and the repair recopies forever. The flush's marker
// completions read a value as text and bind it back, so they need the same
// pin whether or not the proof carries conversions. SET LOCAL cannot take
// bind parameters; both values are constants.
const Rendering = "SET LOCAL extra_float_digits = 3; SET LOCAL bytea_output = 'hex'"

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
