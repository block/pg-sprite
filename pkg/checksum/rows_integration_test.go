package checksum

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rows statement is the digest statement's companion in one string:
// the same per-row hash over the same closed bigint-typed key range,
// returned with its key in key order instead of aggregated, and the same
// USING expression in a retyped column's place on the source side.
func TestRowsSQLIsFrozen(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			"select" text,
			qty integer NOT NULL
		)`)
	target := f.prove(t, "orders")
	types := []columnType{
		{name: "id", typeName: "bigint"},
		{name: "select", typeName: "character varying(20)"},
		{name: "qty", typeName: "numeric(10,2)"},
	}
	want := `SELECT "id", pg_catalog.sha256(pg_catalog.convert_to(ROW("id"::bigint, "select"::character varying(20), "qty"::numeric(10,2))::text, pg_catalog.getdatabaseencoding()))` +
		` FROM "` + f.schema + `"."_pgsprite_orders_new"` +
		` WHERE "id" BETWEEN $1::bigint AND $2::bigint` +
		` ORDER BY "id"`
	assert.Equal(t, want, rowsSQL(target, f.schema, "_pgsprite_orders_new", types, nil))

	conversions := map[string]string{"qty": "(NULLIF(TRIM (BOTH  FROM qty), ''))::numeric"}
	converted := strings.Replace(want, `"qty"::numeric(10,2)`, `((NULLIF(TRIM (BOTH  FROM qty), ''))::numeric)::numeric(10,2)`, 1)
	assert.Equal(t, strings.Replace(converted, `"_pgsprite_orders_new"`, `"orders"`, 1), rowsSQL(target, f.schema, "orders", types, conversions),
		"a converted column hashes as its USING expression, cast to the shadow's type, on the source side")
}

// The two statements are two oracles for one chunk (TM-2): the digest the
// server aggregates and the digest derived in the process from the rows
// the rows statement returns must agree, over a range with rows, an empty
// range, a range whose rows are NULL or empty strings, and a retyped
// column read through its USING expression. The derived digest is what a
// live pass judges a chunk by, so the agreement is the pass's warrant for
// reading rows where every other pass reads a digest.
func TestRowsDeriveTheServersDigest(t *testing.T) {
	f := newProofFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			note text,
			qty text NOT NULL
		)`)
	f.exec(t, `
		INSERT INTO %s.orders (id, note, qty) VALUES
			(1, 'first', ' 10 '),
			(2, NULL, '2.5'),
			(3, '', ''),
			(7, 'seven', '7')`)
	target := f.prove(t, "orders")
	types := []columnType{
		{name: "id", typeName: "bigint"},
		{name: "note", typeName: "text"},
		{name: "qty", typeName: "numeric(10,2)"},
	}
	conversions := map[string]string{"qty": "(NULLIF(TRIM(BOTH FROM qty), ''))::numeric"}
	digestStmt := digestSQL(target, f.schema, "orders", types, conversions)
	rowsStmt := rowsSQL(target, f.schema, "orders", types, conversions)

	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })

	for _, tc := range []struct {
		name         string
		lower, upper int64
		rows         int64
	}{
		{"every row", 1, 7, 4},
		{"the null and empty rows", 2, 3, 2},
		{"a range holding no rows", 4, 6, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, err := digest(t.Context(), tx, digestStmt, tc.lower, tc.upper)
			require.NoError(t, err)
			rows, err := readRows(t.Context(), tx, rowsStmt, tc.lower, tc.upper)
			require.NoError(t, err)
			require.Len(t, rows, int(tc.rows))
			assert.Equal(t, server, rows.digest(), "the digest derived from the rows is the digest the server aggregated")
			require.NoError(t, confirmDigest("source", server, rows))
		})
	}
}
