package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// roundTrip renders the live model, re-admits the output through
// ParseDesired, materializes it on the scratch schema, and returns both
// models plus the diff between them. The round-trip contract is that the
// models are equal and the diff is empty.
func roundTrip(t *testing.T, pool *pgxpool.Pool, schema, table string) (live, desired schemadiff.Model, changes []schemadiff.Change) {
	t.Helper()
	live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
	require.NoError(t, err)
	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	ds, err := statement.ParseDesired(rendered)
	require.NoError(t, err, "rendered output must parse as a desired schema:\n%s", rendered)
	desired, err = schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err, "rendered output must materialize on the scratch schema:\n%s", rendered)
	changes, err = schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	return live, desired, changes
}

// The round-trip oracle: introspect a rich live table, render it, and the
// rendered file must materialize back to the identical model with an empty
// diff. Both sides go through the server's decompilers, so equality here is
// equality of real semantics, not of source text.
func TestRenderRoundTripsLiveTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %s.events (
			id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			name varchar(50) NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			flags int NOT NULL DEFAULT 0,
			doubled int GENERATED ALWAYS AS (flags * 2) STORED,
			note text,
			CONSTRAINT name_len CHECK (length(name) > 0),
			CONSTRAINT events_name_key UNIQUE (name)
		)`, schema),
		fmt.Sprintf("CREATE INDEX events_created_at_idx ON %s.events (created_at DESC)", schema),
		fmt.Sprintf("CREATE INDEX events_note_idx ON %s.events (note) WHERE note IS NOT NULL", schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	live, desired, changes := roundTrip(t, pool, schema, "events")
	assert.Equal(t, live, desired, "rendered output must introspect back to the identical model")
	assert.Empty(t, changes, "diffing a table against its own rendering must yield no changes")
}

// Quoted identifiers round-trip: a table whose name carries whitespace and
// mixed case, columns that are mixed-case and a reserved word, and a
// mixed-case constraint all force every identifier the renderer emits
// through real quoting — a Sanitize call replaced with raw interpolation
// would produce a file this test refuses to parse or materialize.
func TestRenderRoundTripsQuotedIdentifiers(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %s."Order Items" (
			"ID" bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			"User ID" bigint NOT NULL,
			"select" text,
			CONSTRAINT "User Positive" CHECK ("User ID" > 0)
		)`, schema),
		fmt.Sprintf(`CREATE INDEX "Order Items_User_idx" ON %s."Order Items" ("User ID")`, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	live, desired, changes := roundTrip(t, pool, schema, "Order Items")
	assert.Equal(t, live, desired, "rendered output must introspect back to the identical model")
	assert.Empty(t, changes, "diffing a table against its own rendering must yield no changes")
}

// A serial table round-trips through the serial pseudo-type: the rendered
// file recreates the owned sequence on the scratch schema and both sides
// decompile the default identically.
func TestRenderRoundTripsSerialTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id bigserial PRIMARY KEY, v text)", schema))
	require.NoError(t, err)

	live, desired, changes := roundTrip(t, pool, schema, "t")
	assert.Equal(t, live, desired)
	assert.Empty(t, changes)
}

// Quoted, mixed-case, whitespace, and reserved-word identifiers must
// survive the round trip: every rendered name goes through
// pgx.Identifier.Sanitize(), and this fixture is the one that would break
// if a Sanitize call were dropped for raw interpolation — plain lowercase
// fixtures render byte-identically either way.
func TestRenderRoundTripsQuotedIdentifiers(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %s."Order Items" (
			"Item ID" bigint PRIMARY KEY,
			"select" text NOT NULL,
			CONSTRAINT "Select Len" CHECK (length("select") > 0)
		)`, schema),
		fmt.Sprintf(`CREATE INDEX "Order Items select_idx" ON %s."Order Items" ("select")`, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	live, desired, changes := roundTrip(t, pool, schema, "Order Items")
	assert.Equal(t, live, desired, "rendered output must introspect back to the identical model")
	assert.Empty(t, changes, "diffing a table against its own rendering must yield no changes")
}

// Partitioned parents and their partitions are introspectable (the
// statement front door supports in-place changes on them) but refuse to
// render: the model captures the partition key and attachment exactly so
// the refusal — and the diff guard against a partitioning mismatch — fire
// on real catalogs, never silently.
func TestRenderRefusesLivePartitionedTables(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.metrics (id bigint NOT NULL, day date NOT NULL) PARTITION BY RANGE (day)", schema),
		fmt.Sprintf("CREATE TABLE %s.metrics_p1 PARTITION OF %s.metrics FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')", schema, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	parent, err := schemadiff.Introspect(t.Context(), pool, schema, "metrics")
	require.NoError(t, err)
	assert.Equal(t, "RANGE (day)", parent.PartitionKey)
	_, err = schemadiff.Render(parent)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderablePartition)

	child, err := schemadiff.Introspect(t.Context(), pool, schema, "metrics_p1")
	require.NoError(t, err)
	assert.True(t, child.IsPartition)
	_, err = schemadiff.Render(child)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderablePartition)

	// The diff guard closes the partition-blind hole end to end: a desired
	// file declaring PARTITION BY against a plain live table must refuse,
	// not diff to zero.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.plain (id bigint NOT NULL, day date NOT NULL)", schema))
	require.NoError(t, err)
	ds, err := statement.ParseDesired("CREATE TABLE plain (id bigint NOT NULL, day date NOT NULL) PARTITION BY RANGE (day)")
	require.NoError(t, err)
	livePlain, err := schemadiff.Introspect(t.Context(), pool, schema, "plain")
	require.NoError(t, err)
	desiredPartitioned, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)
	_, err = schemadiff.Diff(schema, livePlain, desiredPartitioned)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
}

// A live table with a foreign key cannot be rendered: the desired-file
// grammar refuses foreign keys, and the renderer surfaces that gate's typed
// error rather than emitting a file the front door would reject.
func TestRenderRefusesLiveForeignKey(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.users (id bigint PRIMARY KEY)", schema),
		fmt.Sprintf("CREATE TABLE %s.orders (id bigint PRIMARY KEY, user_id bigint REFERENCES %s.users)", schema, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "orders")
	require.NoError(t, err)
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, statement.ErrForeignKey)

	// The referenced side refuses too: incoming foreign keys are not part
	// of this table's own definition, so a rendered baseline of it would
	// silently drop the relationship.
	referenced, err := schemadiff.Introspect(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, []string{"orders.orders_user_id_fkey"}, referenced.ReferencedBy)
	_, err = schemadiff.Render(referenced)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableForeignKey)
}
