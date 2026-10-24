package chunksql

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
)

// resolutionFixture is a throwaway schema on a plain pool, enough to plant
// catalog entries under the names a conversion spells and read the
// fingerprint back.
type resolutionFixture struct {
	pool   *pgxpool.Pool
	schema string
}

func newResolutionFixture(t *testing.T) resolutionFixture {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testutil.StartPostgres(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return resolutionFixture{pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f resolutionFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
}

// fingerprint reads the fingerprint of conversions in the fixture schema
// through a fresh connection, as a guarded chunk transaction would.
func (f resolutionFixture) fingerprint(t *testing.T, conversions map[string]string) string {
	t.Helper()
	got, err := ResolutionFingerprint(t.Context(), f.pool, f.schema, conversions)
	require.NoError(t, err)
	return got
}

// The conversion `(price::numeric * 100)::int` resolves `*` among the
// operators of that name on the path. Planting a `numeric * integer` in
// the source schema — an exact match the server would now prefer over the
// catalog's `numeric * numeric` — changes the fingerprint; dropping it
// restores the one the build saw, so a resume after the plant is undone
// still matches its proof.
func TestResolutionFingerprintChangesWhenAnOperatorIsPlantedOnThePath(t *testing.T) {
	f := newResolutionFixture(t)
	conversions := map[string]string{"price": "(price::numeric * 100)::int"}
	before := f.fingerprint(t, conversions)
	assert.True(t, strings.HasPrefix(before, "sha256:"), "a copy with conversions carries a fingerprint")

	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE AS 'SELECT 0'`)
	f.exec(t, `
		CREATE OPERATOR %s.* (
			LEFTARG = numeric,
			RIGHTARG = integer,
			FUNCTION = %s.times_zero
		)`)
	planted := f.fingerprint(t, conversions)
	assert.NotEqual(t, before, planted, "an operator under a spelled name in a path schema changes the fingerprint")

	f.exec(t, `DROP OPERATOR %s.* (numeric, integer)`)
	f.exec(t, `DROP FUNCTION %s.times_zero(numeric, integer)`)
	assert.Equal(t, before, f.fingerprint(t, conversions), "the fingerprint is a function of the catalog, not of when it was read")
}

// A function the conversion calls by an unqualified name is digested with
// its body, so replacing the body under the same signature — which changes
// what every later chunk lands without changing which function resolves —
// changes the fingerprint too.
func TestResolutionFingerprintChangesWhenACalledFunctionIsReplaced(t *testing.T) {
	f := newResolutionFixture(t)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE AS 'SELECT ($1::numeric * 100)::integer'`)
	conversions := map[string]string{"price": "to_cents(price)"}
	before := f.fingerprint(t, conversions)

	f.exec(t, `
		CREATE OR REPLACE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE AS 'SELECT ($1::numeric * 1000)::integer'`)
	assert.NotEqual(t, before, f.fingerprint(t, conversions))
}

// A catalog change under a name no conversion spells — here a function
// named like nothing in the expression, in the path schema — leaves the
// fingerprint alone: the digest is over the spelled names, not the schema.
func TestResolutionFingerprintIgnoresNamesTheConversionsDoNotSpell(t *testing.T) {
	f := newResolutionFixture(t)
	conversions := map[string]string{"price": "(price::numeric * 100)::int"}
	before := f.fingerprint(t, conversions)

	f.exec(t, `
		CREATE FUNCTION %s.unrelated(numeric) RETURNS numeric
			LANGUAGE sql IMMUTABLE AS 'SELECT $1'`)
	assert.Equal(t, before, f.fingerprint(t, conversions))
}

// A qualified name is looked up only in the schema it names: the same
// operator planted in the source schema changes the fingerprint of an
// unqualified `*` and not of `OPERATOR(pg_catalog.*)`.
func TestResolutionFingerprintHonoursQualification(t *testing.T) {
	f := newResolutionFixture(t)
	qualified := map[string]string{"price": "(price::numeric OPERATOR(pg_catalog.*) 100)::int"}
	before := f.fingerprint(t, qualified)

	f.exec(t, `
		CREATE FUNCTION %s.times_zero(numeric, integer) RETURNS numeric
			LANGUAGE sql IMMUTABLE AS 'SELECT 0'`)
	f.exec(t, `
		CREATE OPERATOR %s.* (
			LEFTARG = numeric,
			RIGHTARG = integer,
			FUNCTION = %s.times_zero
		)`)
	assert.Equal(t, before, f.fingerprint(t, qualified))
}

// A copy without conversions evaluates no expression and carries no
// fingerprint, whatever the catalog holds.
func TestResolutionFingerprintIsEmptyWithoutConversions(t *testing.T) {
	f := newResolutionFixture(t)
	assert.Empty(t, f.fingerprint(t, nil))
	assert.Empty(t, f.fingerprint(t, map[string]string{}))
}

// The fingerprint reads the same under any search_path: a session whose
// path puts the source schema first sees the same catalog entries as one
// pinned to pg_catalog, because the query qualifies everything it names.
func TestResolutionFingerprintIsIndependentOfTheSessionSearchPath(t *testing.T) {
	f := newResolutionFixture(t)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE AS 'SELECT ($1::numeric * 100)::integer'`)
	conversions := map[string]string{"price": "to_cents(price)"}
	before := f.fingerprint(t, conversions)

	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { assert.NoError(t, tx.Rollback(t.Context())) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL search_path = "+f.schema+", pg_catalog")
	require.NoError(t, err)
	inSchemaFirst, err := ResolutionFingerprint(t.Context(), tx, f.schema, conversions)
	require.NoError(t, err)
	assert.Equal(t, before, inSchemaFirst)
}

// A conversion that calls a function by name is pinned to what that
// function does, not to the text of its body alone: replacing a
// SQL-standard body, flipping STRICT, attaching a SET clause, or changing a
// default argument changes every later chunk's result while leaving prosrc
// as it was, so each changes the fingerprint.
func TestResolutionFingerprintChangesWhenACalledFunctionChangesWithoutItsSourceText(t *testing.T) {
	cases := []struct {
		name   string
		create string
		change string
	}{
		{
			name: "sql-standard body replaced",
			create: `
				CREATE FUNCTION %s.to_cents(text) RETURNS integer
					LANGUAGE sql IMMUTABLE
					BEGIN ATOMIC SELECT ($1::numeric * 100)::integer; END`,
			change: `
				CREATE OR REPLACE FUNCTION %s.to_cents(text) RETURNS integer
					LANGUAGE sql IMMUTABLE
					BEGIN ATOMIC SELECT ($1::numeric * 1000)::integer; END`,
		},
		{
			name: "strictness flipped",
			create: `
				CREATE FUNCTION %s.to_cents(text) RETURNS integer
					LANGUAGE sql IMMUTABLE
					AS 'SELECT COALESCE(($1::numeric * 100)::integer, -1)'`,
			change: `ALTER FUNCTION %s.to_cents(text) STRICT`,
		},
		{
			name: "function-local search_path attached",
			create: `
				CREATE FUNCTION %s.to_cents(text) RETURNS integer
					LANGUAGE sql IMMUTABLE
					AS 'SELECT ($1::numeric * 100)::integer'`,
			change: `ALTER FUNCTION %s.to_cents(text) SET search_path = pg_catalog`,
		},
		{
			name: "default argument changed",
			create: `
				CREATE FUNCTION %s.to_cents(text, integer DEFAULT 100) RETURNS integer
					LANGUAGE sql IMMUTABLE
					AS 'SELECT ($1::numeric * $2)::integer'`,
			change: `
				CREATE OR REPLACE FUNCTION %s.to_cents(text, integer DEFAULT 1000) RETURNS integer
					LANGUAGE sql IMMUTABLE
					AS 'SELECT ($1::numeric * $2)::integer'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolutionFixture(t)
			f.exec(t, tc.create)
			conversions := map[string]string{"price": "to_cents(price)"}
			before := f.fingerprint(t, conversions)

			f.exec(t, tc.change)
			assert.NotEqual(t, before, f.fingerprint(t, conversions))
		})
	}
}

// Each arm of the digest and each schema on the path is pinned by a change
// that only that arm or that schema can see: a function's volatility, an
// operator's function body in either form, its strictness and settings,
// and the function the operator points at, a type, a
// collation, and a function found in public rather than the source schema.
func TestResolutionFingerprintSeesEachArmAndEachPathSchema(t *testing.T) {
	cases := []struct {
		name    string
		setup   []string
		expr    string
		change  []string
		cleanup string
	}{
		{
			name: "function volatility",
			setup: []string{`
				CREATE FUNCTION %s.to_cents(text) RETURNS integer
					LANGUAGE sql IMMUTABLE AS 'SELECT 100'`},
			expr:   "to_cents(price)",
			change: []string{`ALTER FUNCTION %s.to_cents(text) VOLATILE`},
		},
		{
			name: "operator's function body",
			setup: []string{
				`CREATE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE AS 'SELECT $1 || $2'`,
				`CREATE OPERATOR %s.* (LEFTARG = text, RIGHTARG = text, FUNCTION = %s.addf)`,
			},
			expr: "price OPERATOR(%s.*) 'x'",
			change: []string{`
				CREATE OR REPLACE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE AS 'SELECT $2'`},
		},
		{
			name: "operator's function flipped to strict",
			setup: []string{
				`CREATE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE AS 'SELECT COALESCE($1, '''') || $2'`,
				`CREATE OPERATOR %s.* (LEFTARG = text, RIGHTARG = text, FUNCTION = %s.addf)`,
			},
			expr:   "price OPERATOR(%s.*) 'x'",
			change: []string{`ALTER FUNCTION %s.addf(text, text) STRICT`},
		},
		{
			name: "operator's function given a search_path",
			setup: []string{
				`CREATE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE AS 'SELECT $1 || $2'`,
				`CREATE OPERATOR %s.* (LEFTARG = text, RIGHTARG = text, FUNCTION = %s.addf)`,
			},
			expr:   "price OPERATOR(%s.*) 'x'",
			change: []string{`ALTER FUNCTION %s.addf(text, text) SET search_path = pg_catalog`},
		},
		{
			name: "operator's function SQL-standard body replaced",
			setup: []string{
				`CREATE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE
					BEGIN ATOMIC SELECT $1 || $2; END`,
				`CREATE OPERATOR %s.* (LEFTARG = text, RIGHTARG = text, FUNCTION = %s.addf)`,
			},
			expr: "price OPERATOR(%s.*) 'x'",
			change: []string{`
				CREATE OR REPLACE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE
					BEGIN ATOMIC SELECT $2; END`},
		},
		{
			name: "operator repointed at another function",
			setup: []string{
				`CREATE FUNCTION %s.addf(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE AS 'SELECT $1 || $2'`,
				`CREATE FUNCTION %s.addg(text, text) RETURNS text
					LANGUAGE sql IMMUTABLE AS 'SELECT $1 || $2'`,
				`CREATE OPERATOR %s.* (LEFTARG = text, RIGHTARG = text, FUNCTION = %s.addf)`,
			},
			expr: "price OPERATOR(%s.*) 'x'",
			change: []string{
				`DROP OPERATOR %s.* (text, text)`,
				`CREATE OPERATOR %s.* (LEFTARG = text, RIGHTARG = text, FUNCTION = %s.addg)`,
			},
		},
		{
			name:   "type redefined",
			setup:  []string{`CREATE DOMAIN %s.dd AS text`},
			expr:   "price::dd",
			change: []string{`DROP DOMAIN %s.dd`, `CREATE DOMAIN %s.dd AS varchar(3)`},
		},
		{
			// libc's C and POSIX locales exist on every server, whatever
			// ICU support it was built with.
			name:   "collation redefined",
			setup:  []string{`CREATE COLLATION %s.cc (provider = libc, locale = 'C')`},
			expr:   `price COLLATE %s.cc`,
			change: []string{`DROP COLLATION %s.cc`, `CREATE COLLATION %s.cc (provider = libc, locale = 'POSIX')`},
		},
		{
			name: "function in public",
			setup: []string{`
				CREATE FUNCTION public.to_cents_zz(text) RETURNS integer
					LANGUAGE sql IMMUTABLE AS 'SELECT 100'`},
			expr: "to_cents_zz(price)",
			change: []string{`
				CREATE OR REPLACE FUNCTION public.to_cents_zz(text) RETURNS integer
					LANGUAGE sql IMMUTABLE AS 'SELECT 200'`},
			cleanup: `DROP FUNCTION IF EXISTS public.to_cents_zz(text)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResolutionFixture(t)
			if tc.cleanup != "" {
				t.Cleanup(func() {
					_, err := f.pool.Exec(context.WithoutCancel(t.Context()), tc.cleanup)
					require.NoError(t, err)
				})
			}
			for _, s := range tc.setup {
				f.exec(t, s)
			}
			conversions := map[string]string{"price": strings.ReplaceAll(tc.expr, "%s", f.schema)}
			before := f.fingerprint(t, conversions)
			for _, s := range tc.change {
				f.exec(t, s)
			}
			assert.NotEqual(t, before, f.fingerprint(t, conversions))
		})
	}
}

// Every column's conversion is digested, whichever column sorts first: the
// function only the second column calls changes the fingerprint.
func TestResolutionFingerprintDigestsEveryConvertedColumn(t *testing.T) {
	f := newResolutionFixture(t)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE AS 'SELECT 100'`)
	conversions := map[string]string{"a_price": "price::integer", "b_price": "to_cents(price)"}
	before := f.fingerprint(t, conversions)

	f.exec(t, `
		CREATE OR REPLACE FUNCTION %s.to_cents(text) RETURNS integer
			LANGUAGE sql IMMUTABLE AS 'SELECT 200'`)
	assert.NotEqual(t, before, f.fingerprint(t, conversions))
}
