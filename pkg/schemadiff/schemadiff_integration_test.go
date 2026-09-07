package schemadiff_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// sqlstateDuplicateTable is the SQLSTATE the server raises when a CREATE
// INDEX names a relation that already exists.
const sqlstateDuplicateTable = "42P07"

const desiredSQL = `
CREATE TABLE events (
  id bigint PRIMARY KEY,
  name varchar(50) NOT NULL,
  payload jsonb DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT name_not_empty CHECK (length(name) > 0)
);
CREATE INDEX events_created_at_idx ON events (created_at);
CREATE UNIQUE INDEX events_name_key ON events (name);
`

// The two-oracle test of execute-and-introspect: creating the table live and
// materializing the same file on the scratch schema must introspect to the
// identical canonical model.
func TestIntrospectDesiredMatchesLiveIntrospection(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	ds, err := statement.ParseDesired(desiredSQL)
	require.NoError(t, err)
	for _, st := range ds.Statements() {
		qualified, err := statement.Qualify(st.SQL(), schema)
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), qualified)
		require.NoError(t, err)
	}

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	assert.Equal(t, live, desired,
		"live introspection and scratch execute-and-introspect must agree on the canonical model")
}

// Converging a plain integer column onto serial would emit a SET DEFAULT
// referencing a sequence that only ever existed inside the rolled-back
// scratch transaction — a plan that cannot execute. The diff refuses it as
// an unsupported change instead.
func TestDiffRefusesSerialAdoption(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY, v text)", schema))
	require.NoError(t, err)

	ds, err := statement.ParseDesired("CREATE TABLE t (id serial PRIMARY KEY, v text)")
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	_, err = schemadiff.Diff(schema, live, desired)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
}

// A serial table that already matches its desired file must converge to no
// changes: both sides decompile the sequence default identically under
// their introspection search_path.
func TestDiffSerialTableConverges(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id serial PRIMARY KEY, v text)", schema))
	require.NoError(t, err)

	ds, err := statement.ParseDesired("CREATE TABLE t (id serial PRIMARY KEY, v text)")
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "t")
	require.NoError(t, err)
	assert.True(t, live.Columns[0].SequenceDefault, "serial column default must be marked sequence-backed")
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	assert.Empty(t, changes)
}

// Cosmetically different spellings of the same schema must introspect to the
// same canonical model: the server's decompilers are the canonicalizer.
func TestIntrospectCanonicalizesTypeAliases(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %s.aliased (a int4, b varchar(50), c timestamptz, d bool DEFAULT TRUE)", schema))
	require.NoError(t, err)

	m, err := schemadiff.Introspect(t.Context(), pool, schema, "aliased")
	require.NoError(t, err)
	require.Len(t, m.Columns, 4)
	assert.Equal(t, "integer", m.Columns[0].Type)
	assert.Equal(t, "character varying(50)", m.Columns[1].Type)
	assert.Equal(t, "timestamp with time zone", m.Columns[2].Type)
	assert.Equal(t, "boolean", m.Columns[3].Type)
	assert.Equal(t, "true", m.Columns[3].Default)
}

func TestIntrospectDesiredLeavesNoFootprint(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	ds, err := statement.ParseDesired(desiredSQL)
	require.NoError(t, err)
	_, err = schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	var leftover int
	err = pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'pgsprite\\_scratch\\_%'").Scan(&leftover)
	require.NoError(t, err)
	assert.Zero(t, leftover, "the scratch schema must never survive its transaction")
}

func TestIntrospectTableNotFound(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = schemadiff.Introspect(t.Context(), pool, schema, "missing")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
}

func TestIntrospectRefusesViews(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE VIEW %s.v AS SELECT 1 AS one", schema))
	require.NoError(t, err)

	_, err = schemadiff.Introspect(t.Context(), pool, schema, "v")
	require.ErrorIs(t, err, schemadiff.ErrNotTable)
}

// A desired statement that is valid grammar but invalid semantics (a type
// that does not exist) must fail at scratch execution — semantic truth comes
// from the server, not the parser.
func TestIntrospectDesiredSurfacesSemanticErrors(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	ds, err := statement.ParseDesired("CREATE TABLE t (id no_such_type)")
	require.NoError(t, err, "the grammar accepts unknown type names; only the server can refuse them")
	_, err = schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.Error(t, err)
}

// The convergence oracle: diff live against desired, execute the plan, and
// the re-diff must be empty. This closes the loop between the diff engine
// and the real server semantics.
func TestDiffConverges(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	// The live table starts from an older shape: a column to drop, a column
	// to retype, a default to add, an index to replace.
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE TABLE %s.events (
			id bigint PRIMARY KEY,
			name varchar(20) NOT NULL,
			legacy int,
			created_at timestamptz NOT NULL
		)`, schema),
		fmt.Sprintf("CREATE INDEX events_created_at_idx ON %s.events (created_at DESC)", schema),
	} {
		_, err := pool.Exec(t.Context(), ddl)
		require.NoError(t, err)
	}

	ds, err := statement.ParseDesired(desiredSQL)
	require.NoError(t, err)

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	desired, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, live, desired)
	require.NoError(t, err)
	require.NotEmpty(t, changes)

	for _, ch := range changes {
		_, err := pool.Exec(t.Context(), ch.SQL)
		require.NoError(t, err, "derived statement must execute: %s", ch.SQL)
	}

	after, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	rediff, err := schemadiff.Diff(schema, after, desired)
	require.NoError(t, err)
	assert.Empty(t, rediff, "after executing the plan the live table must match the desired state")
}

// The real debris of a failed concurrent build — a unique build over
// duplicate rows fails after its catalog entry exists — introspects as an
// invalid index, and the diff plans the rebuild as a create alone: a live
// entry with the desired name and definition that never finished building
// does not deliver the desired state, and the diff never drops it. The
// planned create does not run as-is against the occupied name; the
// concurrent build path's recovery proves the occupant abandoned, removes
// it, and builds, after which the same desired state diffs to nothing.
func TestDiffRebuildsInvalidIndex(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	const desired = `
CREATE TABLE events (
  id bigint PRIMARY KEY,
  name text NOT NULL
);
CREATE UNIQUE INDEX events_name_key ON events (name);
`
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.events (id bigint PRIMARY KEY, name text NOT NULL); INSERT INTO %[1]s.events VALUES (1, 'dup'), (2, 'dup')",
		schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY events_name_key ON %s.events (name)", schema))
	require.Error(t, err, "a unique build over duplicates must fail after creating its catalog entry")

	ds, err := statement.ParseDesired(desired)
	require.NoError(t, err)
	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	want, err := schemadiff.IntrospectDesired(t.Context(), pool, ds)
	require.NoError(t, err)

	require.Len(t, live.Indexes, 1, "the failed build leaves its catalog entry")
	assert.True(t, live.Indexes[0].Invalid, "the leftover introspects as invalid")
	require.Len(t, want.Indexes, 1)
	assert.Equal(t, live.Indexes[0].Def, want.Indexes[0].Def, "only validity separates the two sides")

	changes, err := schemadiff.Diff(schema, live, want)
	require.NoError(t, err)
	require.Len(t, changes, 1, "the invalid leftover plans as a rebuild, not a drop-and-recreate")
	assert.Equal(t, schemadiff.ChangeCreateIndex, changes[0].Kind)
	assert.False(t, changes[0].Destructive)
	assert.Equal(t, fmt.Sprintf("CREATE UNIQUE INDEX events_name_key ON %s.events USING btree (name)", schema), changes[0].SQL)

	// A baseline pulled from the table renders the leftover like any other
	// index and materializes it valid, so the round trip diffs to exactly
	// the rebuild.
	rendered, err := schemadiff.Render(live)
	require.NoError(t, err)
	baselineDS, err := statement.ParseDesired(rendered)
	require.NoError(t, err)
	baseline, err := schemadiff.IntrospectDesired(t.Context(), pool, baselineDS)
	require.NoError(t, err)
	roundTrip, err := schemadiff.Diff(schema, live, baseline)
	require.NoError(t, err)
	assert.Equal(t, changes, roundTrip, "the pulled baseline re-diffs to the rebuild and nothing else")

	// The planned create cannot run against the occupied name: the server
	// rejects it as a duplicate relation. That is the executor's cue to
	// prove and remove the occupant, never the diff's cue to drop it.
	_, err = pool.Exec(t.Context(), changes[0].SQL)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, sqlstateDuplicateTable, pgErr.Code, "the occupied name refuses the plain create")

	// The duplicates are why the build failed and are the operator's to
	// resolve; the leftover itself is the recovery's. The recovery accepts
	// the plan's create in its concurrent form — the form the planner
	// substitutes on the way to the executor.
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DELETE FROM %s.events WHERE id = 2", schema))
	require.NoError(t, err)
	concurrent, err := statement.Concurrently(changes[0].SQL)
	require.NoError(t, err)
	rep, err := executor.RebuildAbandonedIndex(t.Context(), pool, concurrent, executor.ConcurrentBudget{CallerOwned: true})
	require.NoError(t, err)
	require.Len(t, rep.Dropped, 1, "the recovery removes exactly the abandoned entry")
	assert.Equal(t, "events_name_key", rep.Build.Index)

	after, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	require.Len(t, after.Indexes, 1, "the rebuilt index is the table's only index; no quarantined debris survives")
	assert.False(t, after.Indexes[0].Invalid)
	rediff, err := schemadiff.Diff(schema, after, want)
	require.NoError(t, err)
	assert.Empty(t, rediff, "a valid index with the desired definition is delivered")
}

// A healthy concurrent build still running introspects as invalid too —
// indisready turns true once the build has scanned the table, indisvalid
// only once every older snapshot is gone — and the diff must plan the same
// way as for debris: the create alone when desired names the index, and no
// drop of the in-flight entry when desired does not. Reading indisready in
// place of indisvalid would report the build as delivered.
func TestDiffInFlightConcurrentBuildIsNeitherDeliveredNorDropped(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	const withIndex = `
CREATE TABLE events (
  id bigint PRIMARY KEY,
  name text NOT NULL
);
CREATE INDEX events_name_idx ON events (name);
`
	const withoutIndex = `
CREATE TABLE events (
  id bigint PRIMARY KEY,
  name text NOT NULL
);
`
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.events (id bigint PRIMARY KEY, name text NOT NULL); INSERT INTO %[1]s.events VALUES (1, 'a')", schema))
	require.NoError(t, err)

	// A repeatable-read snapshot older than the build holds no lock on the
	// table, so the build scans it and marks the entry ready, then parks
	// in its final wait for older snapshots with the entry still invalid.
	blocker, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var n int
	require.NoError(t, blocker.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.events", schema)).Scan(&n))

	// The build runs caller-owned: the pool's bounded lock_timeout would
	// cancel its snapshot wait, so its only bound is the context.
	buildCtx, cancelBuild := context.WithCancel(t.Context())
	var buildErr error
	done := make(chan struct{})
	// Registered before the build starts, so a failing assertion below
	// still tears the build and its blocker down rather than leaking them
	// into the pool's close.
	t.Cleanup(func() {
		cancelBuild()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Error("the build did not return")
		}
		if err := blocker.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back the blocker: %v", err)
		}
	})
	go func() {
		defer close(done)
		_, buildErr = executor.BuildIndexConcurrently(buildCtx, pool,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY events_name_idx ON %s.events (name)", schema),
			executor.ConcurrentBudget{CallerOwned: true})
	}()
	require.Eventually(t, func() bool {
		var ready, valid bool
		err := pool.QueryRow(t.Context(), `
			SELECT i.indisready, i.indisvalid
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = 'events_name_idx'`, schema).Scan(&ready, &valid)
		return err == nil && ready && !valid
	}, 30*time.Second, 50*time.Millisecond, "the build must park ready but not yet valid")

	live, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	require.Len(t, live.Indexes, 1)
	assert.True(t, live.Indexes[0].Invalid, "a ready but unvalidated build introspects as invalid")

	wantDS, err := statement.ParseDesired(withIndex)
	require.NoError(t, err)
	want, err := schemadiff.IntrospectDesired(t.Context(), pool, wantDS)
	require.NoError(t, err)
	changes, err := schemadiff.Diff(schema, live, want)
	require.NoError(t, err)
	require.Len(t, changes, 1, "an in-flight build does not deliver the index")
	assert.Equal(t, schemadiff.ChangeCreateIndex, changes[0].Kind)
	assert.Equal(t, fmt.Sprintf("CREATE INDEX events_name_idx ON %s.events USING btree (name)", schema), changes[0].SQL)

	withoutDS, err := statement.ParseDesired(withoutIndex)
	require.NoError(t, err)
	without, err := schemadiff.IntrospectDesired(t.Context(), pool, withoutDS)
	require.NoError(t, err)
	changes, err = schemadiff.Diff(schema, live, without)
	require.NoError(t, err)
	assert.Empty(t, changes, "an in-flight build desired does not name is not dropped from under its builder")

	// Releasing the snapshot lets the build finish; the same desired state
	// then diffs to nothing.
	require.NoError(t, blocker.Rollback(t.Context()))
	select {
	case <-done:
		require.NoError(t, buildErr, "the build completes once the older snapshot is gone")
	case <-time.After(time.Minute):
		t.Fatal("the build did not complete after its blocker released")
	}
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	require.Len(t, after.Indexes, 1)
	assert.False(t, after.Indexes[0].Invalid)
	rediff, err := schemadiff.Diff(schema, after, want)
	require.NoError(t, err)
	assert.Empty(t, rediff)
}

// Identity and generated columns round-trip through both introspection paths.
func TestIntrospectIdentityAndGeneratedColumns(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()
	schema := testutil.NewSchema(t, pool)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.gen (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		n int NOT NULL,
		doubled int GENERATED ALWAYS AS (n * 2) STORED
	)`, schema))
	require.NoError(t, err)

	m, err := schemadiff.Introspect(t.Context(), pool, schema, "gen")
	require.NoError(t, err)
	require.Len(t, m.Columns, 3)
	assert.Equal(t, schemadiff.IdentityAlways, m.Columns[0].Identity)
	assert.True(t, m.Columns[2].Generated)
	assert.Equal(t, "(n * 2)", m.Columns[2].Default)
}
