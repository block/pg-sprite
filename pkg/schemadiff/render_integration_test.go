package schemadiff_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/internal/testutil/modeltest"
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
	desired, err = schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
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
	assert.Equal(t, live, modeltest.Comparable(desired), "rendered output must introspect back to the identical model")
	assert.Empty(t, changes, "diffing a table against its own rendering must yield no changes")
}

// A live check constraint left NOT VALID renders as the validated constraint
// the desired file can declare, since CREATE TABLE ignores NOT VALID. The
// baseline re-diffs to that one validation rather than to zero, and the
// models differ only in the constraint's validity.
func TestRenderNotValidConstraintRendersValidated(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id bigint PRIMARY KEY, qty int);
		ALTER TABLE %[1]s.events ADD CONSTRAINT qty_positive CHECK (qty > 0) NOT VALID`, schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	assert.Contains(t, rendered, `CONSTRAINT "qty_positive" CHECK ((qty > 0))`)
	assert.NotContains(t, rendered, "NOT VALID")

	live, desired, changes := roundTrip(t, pool, schema, "events")
	require.Len(t, changes, 1)
	assert.Equal(t, schemadiff.ChangeValidateConstraint, changes[0].Kind)
	assert.Equal(t, fmt.Sprintf(`ALTER TABLE "%s"."events" VALIDATE CONSTRAINT "qty_positive"`, schema), changes[0].SQL)

	validated := live
	validated.Constraints = append([]schemadiff.Constraint(nil), live.Constraints...)
	for i, con := range validated.Constraints {
		if con.Name == "qty_positive" {
			require.True(t, con.NotValid)
			validated.Constraints[i].Def = "CHECK ((qty > 0))"
			validated.Constraints[i].NotValid = false
		}
	}
	assert.Equal(t, validated, modeltest.Comparable(desired))
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
	assert.Equal(t, live, modeltest.Comparable(desired))
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
	assert.Equal(t, live, modeltest.Comparable(desired), "rendered output must introspect back to the identical model")
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
	desiredPartitioned, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)
	_, err = schemadiff.Diff(schema, livePlain, desiredPartitioned)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
}

func TestRenderRefusesLiveClassicInheritanceOnBothSides(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.parent (id bigint NOT NULL)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.child (note text) INHERITS (%s.parent)", schema, schema))
	require.NoError(t, err)

	parent, err := schemadiff.Introspect(t.Context(), pool, schema, "parent")
	require.NoError(t, err)
	assert.Equal(t, []string{schema + ".child"}, parent.InheritanceChildren)
	assert.Empty(t, parent.InheritsParents)
	_, err = schemadiff.Render(parent)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableInheritance)

	child, err := schemadiff.Introspect(t.Context(), pool, schema, "child")
	require.NoError(t, err)
	assert.Equal(t, []string{schema + ".parent"}, child.InheritsParents)
	assert.Empty(t, child.InheritanceChildren)
	_, err = schemadiff.Render(child)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableInheritance)
}

// A live table with a foreign key cannot be rendered: desired-state
// execution refuses every foreign-key change, so a baseline carrying one
// could never be applied.
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
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableForeignKey)

	// The referenced side refuses too: incoming foreign keys are not part
	// of this table's own definition, so a rendered baseline of it would
	// silently drop the relationship.
	referenced, err := schemadiff.Introspect(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, []string{"orders.orders_user_id_fkey"}, referenced.ReferencedBy)
	_, err = schemadiff.Render(referenced)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableForeignKey)
}

// A foreign key on a partitioned referencing table is cloned onto every
// partition in pg_constraint; ReferencedBy must carry the one real
// relationship, not a row per partition clone.
func TestIntrospectReferencedByCollapsesPartitionClones(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.t (id bigint PRIMARY KEY)", schema),
		fmt.Sprintf("CREATE TABLE %s.pt (id bigint, day date NOT NULL, t_id bigint REFERENCES %s.t(id)) PARTITION BY RANGE (day)", schema, schema),
		fmt.Sprintf("CREATE TABLE %s.pt_1 PARTITION OF %s.pt FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')", schema, schema),
		fmt.Sprintf("CREATE TABLE %s.pt_2 PARTITION OF %s.pt FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')", schema, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	assert.Equal(t, []string{"pt.pt_t_id_fkey"}, live.ReferencedBy)
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableForeignKey)
	assert.NotContains(t, err.Error(), "pt_1")
}

// A self-referential foreign key is the table's own constraint, not an
// incoming reference — ReferencedBy excludes it whether the table is plain
// or partitioned (where every partition carries a clone of the root
// constraint). Rendering still refuses, on the outgoing foreign key the
// table's own definition carries.
func TestIntrospectReferencedByExcludesSelfReference(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE TABLE %s.emp (id bigint PRIMARY KEY, parent_id bigint REFERENCES %s.emp(id))", schema, schema),
		fmt.Sprintf(`CREATE TABLE %s.spt (
			id bigint, day date NOT NULL, parent_id bigint, parent_day date,
			PRIMARY KEY (id, day),
			FOREIGN KEY (parent_id, parent_day) REFERENCES %s.spt (id, day)
		) PARTITION BY RANGE (day)`, schema, schema),
		fmt.Sprintf("CREATE TABLE %s.spt_1 PARTITION OF %s.spt FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')", schema, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	emp, err := schemadiff.Introspect(t.Context(), pool, schema, "emp")
	require.NoError(t, err)
	assert.Empty(t, emp.ReferencedBy)
	assert.Contains(t, emp.Constraints, schemadiff.Constraint{
		Name:       "emp_parent_id_fkey",
		Def:        "FOREIGN KEY (parent_id) REFERENCES emp(id)",
		ForeignKey: true,
	})
	_, err = schemadiff.Render(emp)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableForeignKey)

	spt, err := schemadiff.Introspect(t.Context(), pool, schema, "spt")
	require.NoError(t, err)
	assert.Empty(t, spt.ReferencedBy)
}

// Ownership, not naming, separates a serial column from a hand-written
// nextval default. Two tables share one standalone sequence that happens to
// carry the serial-style name of the first: rendering either as serial
// would silently convert the shared sequence into a private one and break
// the shared-ID invariant, so both refuse. A genuinely owned sequence still
// renders.
func TestRenderRefusesStandaloneSequenceWithSerialName(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf("CREATE SEQUENCE %s.events_id_seq", schema),
		fmt.Sprintf("CREATE TABLE %s.events (id bigint NOT NULL DEFAULT nextval('%s.events_id_seq'::regclass), name text)", schema, schema),
		fmt.Sprintf("CREATE TABLE %s.orders (id bigint NOT NULL DEFAULT nextval('%s.events_id_seq'::regclass), name text)", schema, schema),
		fmt.Sprintf("CREATE TABLE %s.owned (id bigserial NOT NULL, name text)", schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	for _, table := range []string{"events", "orders"} {
		live, err := schemadiff.Introspect(t.Context(), pool, schema, table)
		require.NoError(t, err)
		require.True(t, live.Columns[0].SequenceDefault)
		assert.False(t, live.Columns[0].SequenceOwned, "a standalone sequence is not owned, whatever its name")
		_, err = schemadiff.Render(live)
		require.ErrorIs(t, err, schemadiff.ErrUnrenderableDefault, "table %s", table)
	}

	owned, err := schemadiff.Introspect(t.Context(), pool, schema, "owned")
	require.NoError(t, err)
	assert.True(t, owned.Columns[0].SequenceOwned)
	rendered, err := schemadiff.Render(owned)
	require.NoError(t, err)
	assert.Contains(t, rendered, `"id" bigserial NOT NULL`)
}

// An unlogged live table refuses to render, and a persistence mismatch
// between live and desired is a typed diff refusal — never a plain-table
// baseline or a silent zero diff.
func TestRenderRefusesUnloggedLiveTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE UNLOGGED TABLE %s.buffer (id bigint NOT NULL, name text)", schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "buffer")
	require.NoError(t, err)
	assert.True(t, live.Unlogged)
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableUnlogged)

	// The diff guard closes the persistence-blind hole end to end: a
	// desired file declaring UNLOGGED against a plain live table must
	// refuse, not diff to zero.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.plainbuf (id bigint NOT NULL, name text)", schema))
	require.NoError(t, err)
	ds, err := statement.ParseDesired("CREATE UNLOGGED TABLE plainbuf (id bigint NOT NULL, name text)")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)
	assert.True(t, desired.Unlogged)
	livePlain, err := schemadiff.Introspect(t.Context(), pool, schema, "plainbuf")
	require.NoError(t, err)
	_, err = schemadiff.Diff(schema, livePlain, desired)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
}

// A live virtual generated column (PostgreSQL 18+) is introspected as
// virtual, never as a column default. Export refuses it, and a desired file,
// which cannot declare VIRTUAL, fails the diff rather than converging the
// expression as a default.
func TestRefusesVirtualGeneratedLiveColumn(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	var version int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_setting('server_version_num')::int`).Scan(&version))
	const virtualColumnsSince = 180000
	if version < virtualColumnsSince {
		t.Skipf("virtual generated columns exist from PostgreSQL 18; server reports %d", version)
	}
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		`CREATE TABLE %s.gen (id bigint NOT NULL, n int NOT NULL, doubled int GENERATED ALWAYS AS (n * 2) VIRTUAL)`, schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "gen")
	require.NoError(t, err)
	require.Len(t, live.Columns, 3)
	assert.True(t, live.Columns[2].Virtual)
	assert.False(t, live.Columns[2].Generated)
	assert.Equal(t, "(n * 2)", live.Columns[2].Default)
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableVirtual)

	ds, err := statement.ParseDesired(`CREATE TABLE gen (id bigint NOT NULL, n int NOT NULL, doubled int GENERATED ALWAYS AS (n * 3) STORED)`)
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)
	_, err = schemadiff.Diff(schema, live, desired)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
}

// A column with an explicit collation refuses to render, and a collation
// delta between live and desired is a typed diff refusal — never a
// baseline that silently drops the COLLATE clause or a silent zero diff.
func TestRenderRefusesCollatedLiveColumn(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.words (id bigint NOT NULL, word text COLLATE "C")`, schema))
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "words")
	require.NoError(t, err)
	assert.Equal(t, `pg_catalog."C"`, live.Columns[1].Collation)
	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableCollation)

	ds, err := statement.ParseDesired(`CREATE TABLE words (id bigint NOT NULL, word text)`)
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)
	_, err = schemadiff.Diff(schema, live, desired)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
}

// Storage parameters, the table's own and its TOAST table's, are part of
// the model: export writes them back as a WITH clause that materializes to
// the same parameters, so the baseline keeps a tuned table's settings.
func TestRenderRoundTripsStorageParameters(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.tuned (id bigint PRIMARY KEY, body text)
		WITH (fillfactor = 70, autovacuum_vacuum_scale_factor = 0.05, toast.autovacuum_enabled = false)`, schema))
	require.NoError(t, err)

	live, desired, changes := roundTrip(t, pool, schema, "tuned")
	assert.Equal(t, []string{"autovacuum_vacuum_scale_factor=0.05", "fillfactor=70", "toast.autovacuum_enabled=false"}, live.Options)
	assert.Equal(t, live, modeltest.Comparable(desired), "rendered output must introspect back to the identical model")
	assert.Empty(t, changes)

	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	assert.Contains(t, rendered, `) WITH (autovacuum_vacuum_scale_factor='0.05', fillfactor='70', toast.autovacuum_enabled='false');`)
}

// A table keeps its TOAST table, and parameters set on it, after its last
// toastable column is dropped, though a fresh CREATE TABLE of the columns
// left would build none. Building the rendered file would discard the
// parameters, so export refuses TOAST parameters on a table with a dropped
// column unless a remaining column, such as a text one, guarantees the
// rebuild a TOAST table; a bounded varchar guarantees nothing and still
// refuses. The table's own parameters beside a dropped column render.
func TestRenderRefusesToastParametersAFreshTableMightDiscard(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	for _, tc := range []struct {
		name, setup string
		refused     bool
	}{
		{"a dropped column", `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, body text) WITH (toast.autovacuum_enabled = false);
			ALTER TABLE %[1]s.t DROP COLUMN body`, true},
		{"a dropped column beside a remaining text column", `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, body text, legacy int) WITH (toast.autovacuum_enabled = false);
			ALTER TABLE %[1]s.t DROP COLUMN legacy`, false},
		{"a dropped column beside only a bounded varchar", `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, code varchar(10), body text) WITH (toast.autovacuum_enabled = false);
			ALTER TABLE %[1]s.t DROP COLUMN body`, true},
		{"a dropped column without TOAST parameters", `CREATE TABLE %[1]s.t (id bigint PRIMARY KEY, body text, extra text) WITH (fillfactor = 70);
			ALTER TABLE %[1]s.t DROP COLUMN extra`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.setup, schema))
			require.NoError(t, err)
			live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
			require.NoError(t, err)
			_, err = schemadiff.Render(live)
			if tc.refused {
				require.ErrorIs(t, err, schemadiff.ErrUnrenderableToastOptions)
				return
			}
			require.NoError(t, err)
			_, _, changes := roundTrip(t, pool, schema, "t")
			assert.Empty(t, changes)
		})
	}
}

// The diff does not converge storage parameters, so a difference in either
// direction is a typed refusal: a parameter the file declares is never
// reported as applied, and a tuned live parameter the file omits is never
// reported as matching.
func TestDiffRefusesStorageParameterMismatch(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.plain (id bigint PRIMARY KEY)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.tuned (id bigint PRIMARY KEY) WITH (fillfactor = 70)`, schema))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, table, desired, message string
	}{
		{
			name:    "the file declares a parameter the live table lacks",
			table:   "plain",
			desired: `CREATE TABLE plain (id bigint PRIMARY KEY) WITH (fillfactor = 70)`,
			message: "live (none) and desired (fillfactor=70)",
		},
		{
			name:    "the file omits a parameter the live table has",
			table:   "tuned",
			desired: `CREATE TABLE tuned (id bigint PRIMARY KEY)`,
			message: "live (fillfactor=70) and desired (none)",
		},
		{
			name:    "the file declares a different value",
			table:   "tuned",
			desired: `CREATE TABLE tuned (id bigint PRIMARY KEY) WITH (fillfactor = 80)`,
			message: "live (fillfactor=70) and desired (fillfactor=80)",
		},
		{
			// A table with no column that needs a TOAST table has none,
			// and the server discards toast parameters set on it. The file
			// adds such a column, so its scratch table keeps the parameter
			// the live table never held: the plan could not deliver it.
			name:    "the file's toast parameter has no TOAST table to land on live",
			table:   "plain",
			desired: `CREATE TABLE plain (id bigint PRIMARY KEY, body text) WITH (toast.autovacuum_enabled = false)`,
			message: "live (none) and desired (toast.autovacuum_enabled=false)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, err := schemadiff.Introspect(t.Context(), pool, schema, tc.table)
			require.NoError(t, err)
			ds, err := statement.ParseDesired(tc.desired)
			require.NoError(t, err)
			desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
			require.NoError(t, err)
			_, err = schemadiff.Diff(schema, live, desired)
			require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
			assert.Contains(t, err.Error(), tc.message)
		})
	}
}

// The server stores a storage parameter's value as written, so a boolean
// set as off live and declared as false in the file, or in any other
// spelling the server reads as the same boolean, still matches: the diff
// compares what the parameters mean, not how they were typed. A number is
// left as written.
func TestDiffMatchesBooleanStorageParameterSpellings(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.tuned (id bigint PRIMARY KEY, body text)
		WITH (autovacuum_enabled = off, toast.autovacuum_enabled = 'No', autovacuum_vacuum_threshold = 0)`, schema))
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "tuned")
	require.NoError(t, err)
	assert.Equal(t, []string{"autovacuum_enabled=false", "autovacuum_vacuum_threshold=0", "toast.autovacuum_enabled=false"}, live.Options)

	ds, err := statement.ParseDesired(`CREATE TABLE tuned (id bigint PRIMARY KEY, body text)
		WITH (autovacuum_enabled = false, toast.autovacuum_enabled = FALSE, autovacuum_vacuum_threshold = 0)`)
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
}

// An identity column declared away from the defaults carries its sequence
// declaration through export: the rendered options clause materializes to
// the same sequence, so the baseline keeps the column's bounds and step. A
// column declared at the defaults records no declaration at all.
func TestRenderRoundTripsIdentitySequenceOptions(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.tickets (
		id bigint GENERATED ALWAYS AS IDENTITY (START WITH 1000 INCREMENT BY 10 MAXVALUE 900000 CACHE 20) PRIMARY KEY,
		seat int GENERATED BY DEFAULT AS IDENTITY NOT NULL
	)`, schema))
	require.NoError(t, err)

	live, desired, changes := roundTrip(t, pool, schema, "tickets")
	assert.Equal(t, &schemadiff.SequenceOptions{Type: "bigint", Start: 1000, Increment: 10, Min: 1, Max: 900000, Cache: 20}, live.Columns[0].Sequence)
	assert.Nil(t, live.Columns[1].Sequence, "a sequence declared at the defaults records nothing")
	assert.Equal(t, live, modeltest.Comparable(desired), "rendered output must introspect back to the identical model")
	assert.Empty(t, changes)

	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	assert.Contains(t, rendered, `"id" bigint GENERATED ALWAYS AS IDENTITY (START WITH 1000 INCREMENT BY 10 MINVALUE 1 MAXVALUE 900000 CACHE 20 NO CYCLE) NOT NULL`)
	assert.Contains(t, rendered, `"seat" integer GENERATED BY DEFAULT AS IDENTITY NOT NULL`)
}

// A serial's sequence declared away from the defaults cannot be stated by
// the serial shorthand, so export refuses rather than writing a serial
// that would recreate the sequence at the defaults. That covers a tuned
// sequence and an integer serial whose column was later widened to bigint
// while its sequence stayed integer, which a bigserial would silently
// widen.
func TestRenderRefusesSerialWithDeclaredSequence(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %s.tuned (id bigserial PRIMARY KEY)`, schema),
		fmt.Sprintf(`ALTER SEQUENCE %s.tuned_id_seq MAXVALUE 5000`, schema),
		fmt.Sprintf(`CREATE TABLE %s.widened (id serial PRIMARY KEY)`, schema),
		fmt.Sprintf(`ALTER TABLE %s.widened ALTER COLUMN id TYPE bigint`, schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err, ddl)
	}

	tuned, err := schemadiff.Introspect(t.Context(), pool, schema, "tuned")
	require.NoError(t, err)
	assert.Equal(t, &schemadiff.SequenceOptions{Type: "bigint", Start: 1, Increment: 1, Min: 1, Max: 5000, Cache: 1}, tuned.Columns[0].Sequence)
	_, err = schemadiff.Render(tuned)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableSequenceOptions)

	widened, err := schemadiff.Introspect(t.Context(), pool, schema, "widened")
	require.NoError(t, err)
	assert.Equal(t, &schemadiff.SequenceOptions{Type: "integer", Start: 1, Increment: 1, Min: 1, Max: 2147483647, Cache: 1}, widened.Columns[0].Sequence)
	_, err = schemadiff.Render(widened)
	require.ErrorIs(t, err, schemadiff.ErrUnrenderableSequenceOptions)
}

// A column whose default draws on two sequences it owns cannot be carried
// as one sequence declaration. Introspection still reads the table, so a
// plan for any other statement on it works, and records the column once
// with no sequence; export refuses it rather than write one of the
// sequences, or neither, into the baseline.
func TestRenderRefusesColumnWithTwoOwnedSequences(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)
	createTwoOwnedSequenceTable(t, pool, schema)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	require.Len(t, live.Columns, 2)
	assert.Equal(t, "id", live.Columns[0].Name)
	assert.True(t, live.Columns[0].MultipleOwnedSequences)
	assert.Nil(t, live.Columns[0].Sequence)
	assert.False(t, live.Columns[1].MultipleOwnedSequences)

	_, err = schemadiff.Render(live)
	require.ErrorIs(t, err, schemadiff.ErrMultipleOwnedSequences)
	assert.Contains(t, err.Error(), `column "id"`)
}

func createTwoOwnedSequenceTable(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE SEQUENCE %[1]s.first_seq`,
		`CREATE SEQUENCE %[1]s.second_seq`,
		`CREATE TABLE %[1]s.t (id bigint PRIMARY KEY DEFAULT nextval('%[1]s.first_seq') + nextval('%[1]s.second_seq'), note text)`,
		`ALTER SEQUENCE %[1]s.first_seq OWNED BY %[1]s.t.id`,
		`ALTER SEQUENCE %[1]s.second_seq OWNED BY %[1]s.t.id`,
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
}

// The diff does not converge an identity's sequence declaration, so a
// difference in either direction is a typed refusal: a MAXVALUE the file
// declares is never reported as applied, and a live START WITH the file
// omits is never reported as matching.
func TestDiffRefusesIdentitySequenceMismatch(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.plain (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.tuned (id bigint GENERATED ALWAYS AS IDENTITY (START WITH 1000) PRIMARY KEY)`, schema))
	require.NoError(t, err)

	for _, tc := range []struct {
		name, table, desired, message string
	}{
		{
			name:    "the file declares options the live sequence lacks",
			table:   "plain",
			desired: `CREATE TABLE plain (id bigint GENERATED ALWAYS AS IDENTITY (MAXVALUE 5000) PRIMARY KEY)`,
			message: "live (the defaults) and desired (bigint START WITH 1 INCREMENT BY 1 MINVALUE 1 MAXVALUE 5000 CACHE 1 NO CYCLE)",
		},
		{
			name:    "the file omits options the live sequence has",
			table:   "tuned",
			desired: `CREATE TABLE tuned (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY)`,
			message: "live (bigint START WITH 1000 INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 CACHE 1 NO CYCLE) and desired (the defaults)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, err := schemadiff.Introspect(t.Context(), pool, schema, tc.table)
			require.NoError(t, err)
			ds, err := statement.ParseDesired(tc.desired)
			require.NoError(t, err)
			desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
			require.NoError(t, err)
			_, err = schemadiff.Diff(schema, live, desired)
			require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
			assert.Contains(t, err.Error(), tc.message)
		})
	}
}

// Widening an identity column retypes its sequence with it: a bound at the
// old type's limit follows the new type, and a declared one stays. So a
// file that widens the column and states the live declaration plans the
// type change, and applying it leaves the table matching the file. A file
// that also drops a declared bound still refuses, since the type change
// keeps that bound as written.
func TestDiffWidensAnIdentityWithADeclaredSequence(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	for _, tc := range []struct {
		name, options, desired string
		refused                bool
	}{
		{"a declared start", "START WITH 1000", "START WITH 1000", false},
		{"a declared maximum", "MAXVALUE 5000", "MAXVALUE 5000", false},
		{"a declared maximum the file drops", "MAXVALUE 5000", "START WITH 1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := testutil.NewSchema(t, pool)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.t (id integer GENERATED ALWAYS AS IDENTITY (%s) PRIMARY KEY)`, schema, tc.options))
			require.NoError(t, err)
			ds, err := statement.ParseDesired(fmt.Sprintf(`CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY (%s) PRIMARY KEY)`, tc.desired))
			require.NoError(t, err)
			desired, err := schemadiff.IntrospectDesired(t.Context(), pool, schema, ds)
			require.NoError(t, err)
			live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
			require.NoError(t, err)

			changes, err := schemadiff.Diff(schema, live, desired)
			if tc.refused {
				require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
				assert.Contains(t, err.Error(), "MAXVALUE 5000")
				return
			}
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Contains(t, changes[0].SQL, "TYPE bigint")
			_, err = pool.Exec(t.Context(), changes[0].SQL)
			require.NoError(t, err)

			converged, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
			require.NoError(t, err)
			after, err := schemadiff.Diff(schema, converged, desired)
			require.NoError(t, err)
			assert.Empty(t, after)
			assert.Equal(t, desired.Columns[0].Sequence, converged.Columns[0].Sequence)
		})
	}
}
