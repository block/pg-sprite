package testutil

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// NewLookalikeTextMismatchPool plants, in a fresh schema of the database at
// databaseURL, an = and a <> on text and text that answer false for every
// pair, and returns a pool whose search_path lists that schema ahead of
// pg_catalog. On that pool an unqualified comparison of two texts never
// holds, so a read that finds a row by a text column through it is proven
// to spell its operators OPERATOR(pg_catalog.…). The pool is asserted to
// reach both lookalikes before it is returned.
func NewLookalikeTextMismatchPool(t *testing.T, databaseURL string, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	schema := NewSchema(t, owner)
	for _, ddl := range []string{
		`
		CREATE FUNCTION %[1]s.never_text(text, text) RETURNS boolean
		LANGUAGE sql AS 'SELECT false'`,
		`
		CREATE OPERATOR %[1]s.= (
			LEFTARG  = text,
			RIGHTARG = text,
			FUNCTION = %[1]s.never_text
		)`,
		`
		CREATE OPERATOR %[1]s.<> (
			LEFTARG  = text,
			RIGHTARG = text,
			FUNCTION = %[1]s.never_text
		)`,
	} {
		_, err := owner.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, "plant a lookalike text operator")
	}
	pool := NewCatalogShadowingPool(t, databaseURL, schema)

	var equal, unequal bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT 'a'::text = 'a'::text, 'a'::text <> 'b'::text`).Scan(&equal, &unequal))
	require.False(t, equal, "the lookalike text = must answer on this pool")
	require.False(t, unequal, "the lookalike text <> must answer on this pool")
	return pool
}

// NewLookalikeTextTypePool plants, in a fresh schema of the database at
// databaseURL, a type named text — a domain over varchar(3) — and returns
// a pool whose search_path lists that schema ahead of pg_catalog. On that
// pool an unqualified ::text cast resolves to the lookalike and cuts the
// value to its first three characters, so a WAL position reads as 0/1
// whatever the server's, and a read that renders a value as text through
// it is proven to spell the cast ::pg_catalog.text. The pool is asserted
// to reach the lookalike before it is returned.
func NewLookalikeTextTypePool(t *testing.T, databaseURL string, owner *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	schema := NewSchema(t, owner)
	_, err := owner.Exec(t.Context(), fmt.Sprintf(`
		CREATE DOMAIN %s.text AS pg_catalog.varchar(3)`, schema))
	require.NoError(t, err, "plant a lookalike text type")
	pool := NewCatalogShadowingPool(t, databaseURL, schema)

	var cut string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT '0/16B3748'::pg_lsn::text`).Scan(&cut))
	require.Equal(t, "0/1", cut, "the lookalike text type must answer on this pool")
	return pool
}
