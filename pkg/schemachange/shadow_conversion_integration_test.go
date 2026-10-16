package schemachange_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// A USING expression the server does not classify as IMMUTABLE is refused
// before the build commits, under the conversion cause and its own
// sentinel: the copy evaluates the expression in the chunk copy, in the
// recopy, and for every image the catch-up flushes, in different sessions
// at different times, and only an immutable expression gives all of them
// the same answer for the same source value. now() reads the clock;
// text to timestamptz reads the session's time zone. The refusal rolls the
// build back, so no shadow is left behind.
func TestBuildShadowRefusesAConversionThatIsNotImmutable(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id integer PRIMARY KEY,
			note text NOT NULL,
			created text NOT NULL
		)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")

	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN note TYPE text USING now()::text`), schemachange.Options{})
	require.ErrorIs(t, err, schemachange.ErrInvariantViolation, "an expression that reads the clock")
	assert.Equal(t, schemachange.CauseStatementConversion, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrConversionNotImmutable)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "nothing was created")

	_, err = schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN created TYPE timestamptz USING created::timestamptz`), schemachange.Options{})
	require.ErrorIs(t, err, schemachange.ErrInvariantViolation, "a cast that reads the session time zone")
	assert.Equal(t, schemachange.CauseStatementConversion, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrConversionNotImmutable)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "nothing was created")
}

// The probe reads the server's own classification of the expression, not
// a list of known functions: the same user function body is refused when
// the function is declared STABLE and admitted when it is declared
// IMMUTABLE. The functions are PL/pgSQL so that no server version inlines
// them before classifying the expression.
func TestBuildShadowReadsTheVolatilityOfAUserFunctionInUSING(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id integer PRIMARY KEY,
			price numeric NOT NULL
		)`)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents_stable(numeric) RETURNS integer
			LANGUAGE plpgsql STABLE
			AS $$ BEGIN RETURN ($1 * 100)::integer; END $$`)
	f.exec(t, `
		CREATE FUNCTION %s.to_cents(numeric) RETURNS integer
			LANGUAGE plpgsql IMMUTABLE
			AS $$ BEGIN RETURN ($1 * 100)::integer; END $$`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")

	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN price TYPE integer USING to_cents_stable(price)`), schemachange.Options{})
	require.ErrorIs(t, err, schemachange.ErrInvariantViolation, "a STABLE function")
	assert.Equal(t, schemachange.CauseStatementConversion, schemachange.RefusalCauseOf(err))
	assert.ErrorIs(t, err, schemachange.ErrConversionNotImmutable)
	assert.False(t, f.relationExists(t, schemachange.ShadowName(f.schema, "orders")), "nothing was created")

	built, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN price TYPE integer USING to_cents(price)`), schemachange.Options{})
	require.NoError(t, err, "the same body declared IMMUTABLE")
	assert.Equal(t, map[string]string{"price": "to_cents(price)"}, built.Conversions())
	assert.Equal(t, "integer", f.columnType(t, built.ShadowTable(), "price"))
}

// An immutable expression of operators and casts is admitted, with the
// probe leaving no trace on the shadow: the built shadow has exactly the
// source's columns as the statement left them.
func TestBuildShadowAdmitsAnImmutableConversion(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id integer PRIMARY KEY,
			price numeric NOT NULL,
			placed_at timestamptz NOT NULL
		)`)

	built, err := f.build(t, "orders", `
		ALTER TABLE %s.orders
			ALTER COLUMN price TYPE integer USING (price::numeric * 100)::integer,
			ALTER COLUMN placed_at TYPE timestamp USING placed_at AT TIME ZONE 'UTC'`)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"price", "placed_at"}, slices.Collect(maps.Keys(built.Conversions())))
	assert.Equal(t, []string{"id", "price", "placed_at"}, built.CopyColumns())
	assert.Equal(t, "integer", f.columnType(t, built.ShadowTable(), "price"))
	assert.Equal(t, "timestamp without time zone", f.columnType(t, built.ShadowTable(), "placed_at"))
	assert.False(t, f.columnExists(t, built.ShadowTable(), "_pgsprite_using_probe"), "the probe column was rolled back")
}

// Every converted column of one statement is probed, not only the first,
// and when more than one is refused the same column is named on every run:
// a statement whose second conversion is the volatile one would otherwise
// be admitted, which is the fail-open the probe exists to prevent.
func TestBuildShadowProbesEveryConversionAndNamesTheFirstRefusedColumn(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id integer PRIMARY KEY,
			note text NOT NULL,
			price numeric NOT NULL
		)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")

	// note is immutable and sorts first; price is not, so a probe that
	// stops after the first conversion admits the statement.
	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN note TYPE text USING lower(note),
			ALTER COLUMN price TYPE integer USING (price * random())::integer`), schemachange.Options{})
	require.ErrorIs(t, err, schemachange.ErrConversionNotImmutable, "the second conversion is the volatile one")
	assert.ErrorContains(t, err, "USING expression of price")
	assert.ErrorContains(t, err, "(price * random())::int", "the refusal carries the expression as the deparser renders it")
	assert.ErrorContains(t, err, "IMMUTABLE functions of price alone", "and the rewrite")

	// Both are refused: map order must not decide which one the error
	// names.
	for range 20 {
		_, err = schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
			ALTER TABLE %s.orders
				ALTER COLUMN note TYPE text USING now()::text,
				ALTER COLUMN price TYPE integer USING (price * random())::integer`), schemachange.Options{})
		require.ErrorIs(t, err, schemachange.ErrConversionNotImmutable)
		require.ErrorContains(t, err, "USING expression of note")
	}
}

// A USING on a generated column is the server's own refusal when the
// statement runs, and a name that does not resolve is the server's error
// from the probe: neither is reported as an immutability refusal, so a
// user reading the error is pointed at what they wrote, not at the probe.
func TestBuildShadowLeavesOtherServerErrorsToTheServer(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id integer PRIMARY KEY,
			note text NOT NULL,
			doubled integer GENERATED ALWAYS AS (id * 2) STORED
		)`)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")

	_, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN doubled TYPE bigint USING doubled::bigint`), schemachange.Options{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, schemachange.ErrConversionNotImmutable, "the statement's own error, not the probe's")
	assert.Empty(t, schemachange.RefusalCauseOf(err))
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42611", pgErr.Code, "invalid_column_definition: USING on a generated column")

	_, err = schemachange.BuildShadow(t.Context(), f.pool, lock, target, f.alter(t, `
		ALTER TABLE %s.orders
			ALTER COLUMN note TYPE integer USING note::nosuchtype`), schemachange.Options{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, schemachange.ErrConversionNotImmutable, "a type that does not exist")
	assert.Empty(t, schemachange.RefusalCauseOf(err))
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42704", pgErr.Code, "undefined_object: the type named in the cast")
}

// The probe's throwaway column takes a name no source column carries, so a
// source that happens to have a column named like the probe still has its
// immutable conversion admitted, and the shadow ends with the source's
// columns alone.
func TestBuildShadowProbesBesideAColumnNamedLikeTheProbe(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.orders (
			id integer PRIMARY KEY,
			note text NOT NULL,
			_pgsprite_using_probe text
		)`)

	built, err := f.build(t, "orders", `
		ALTER TABLE %s.orders
			ALTER COLUMN note TYPE text USING upper(note)`)
	require.NoError(t, err, "an immutable expression beside a column named like the probe")
	assert.Equal(t, map[string]string{"note": "upper(note)"}, built.Conversions())
	assert.Equal(t, []string{"id", "note", "_pgsprite_using_probe"}, built.CopyColumns())
	assert.False(t, f.columnExists(t, built.ShadowTable(), "_pgsprite_using_probe_"), "the lengthened probe name was rolled back")
}
