package migrate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/diffplan"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

// liveEvents starts a target with an events table of 1,000 rows and no key,
// and returns its pool and schema.
func liveEvents(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (id bigint NOT NULL, email text, name text);
		INSERT INTO %[1]s.events SELECT g, 'u' || g || '@example.com', 'n' || g FROM generate_series(1, 1000) g`, schema))
	require.NoError(t, err)
	return pool, schema
}

// runKeyAdd runs desired against the live events table and checks that the
// key ran as a concurrent build, then an attach that checks the index the
// build left, that the constraint owns that index, and that a second run
// plans nothing.
func runKeyAdd(t *testing.T, pool *pgxpool.Pool, schema, desired, index string, execSQL []string, adopts schemadiff.IndexAdoption) {
	t.Helper()
	req := migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, desired)}

	res, err := migrate.RunDesired(t.Context(), pool, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome, res.Detail)
	assert.Equal(t, &migrate.Convergence{Status: migrate.ConvergenceVerified}, res.Convergence)
	require.NotEmpty(t, res.Plan.Statements)
	st := res.Plan.Statements[len(res.Plan.Statements)-1]
	assert.Equal(t, schemadiff.ChangeAddConstraint, st.Kind)
	assert.Equal(t, execSQL, st.ExecSQL, "the key runs as a concurrent build, then an attach")
	assert.Equal(t, &adopts, st.Adopts, "the attach checks the index its build left")

	var owned bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint con
		               JOIN pg_catalog.pg_class i ON i.oid = con.conindid
		               WHERE con.conrelid = $1::regclass AND con.conname = $2 AND i.relname = $2)`,
		schema+".events", index).Scan(&owned))
	assert.True(t, owned, "the constraint owns the index the run built")

	res, err = migrate.RunDesired(t.Context(), pool, req, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, res.Outcome)
	assert.Empty(t, res.Plan.Statements, "the converged table plans no statements")
}

// A unique constraint a desired file adds to a live table with rows runs as
// a concurrent build of its index, then ADD CONSTRAINT ... USING INDEX. The
// plan records the index the attach takes over, with the definition its
// build leaves, so the attach checks it under the table's lock. The run
// converges.
func TestRunDesiredAddsAUniqueConstraintToALiveTable(t *testing.T) {
	pool, schema := liveEvents(t)
	runKeyAdd(t, pool, schema, `
CREATE TABLE events (
    id bigint NOT NULL,
    email text,
    name text,
    CONSTRAINT events_name_key UNIQUE (name) INCLUDE (email)
);`, "events_name_key", []string{
		fmt.Sprintf(`CREATE UNIQUE INDEX CONCURRENTLY "events_name_key" ON "%s"."events" ("name") INCLUDE ("email")`, schema),
		fmt.Sprintf(`ALTER TABLE "%s"."events" ADD CONSTRAINT "events_name_key" UNIQUE USING INDEX "events_name_key"`, schema),
	}, schemadiff.IndexAdoption{Index: "events_name_key", Def: "CREATE UNIQUE INDEX events_name_key ON events USING btree (name) INCLUDE (email)"})
}

// A unique constraint on a column the same plan adds runs the same way,
// after the column is added.
func TestRunDesiredAddsAUniqueConstraintOnAnAddedColumn(t *testing.T) {
	pool, schema := liveEvents(t)
	runKeyAdd(t, pool, schema, `
CREATE TABLE events (
    id bigint NOT NULL,
    email text,
    name text,
    code text,
    CONSTRAINT events_code_key UNIQUE (code)
);`, "events_code_key", []string{
		fmt.Sprintf(`CREATE UNIQUE INDEX CONCURRENTLY "events_code_key" ON "%s"."events" ("code")`, schema),
		fmt.Sprintf(`ALTER TABLE "%s"."events" ADD CONSTRAINT "events_code_key" UNIQUE USING INDEX "events_code_key"`, schema),
	}, schemadiff.IndexAdoption{Index: "events_code_key", Def: "CREATE UNIQUE INDEX events_code_key ON events USING btree (code)"})
}

// A primary key runs the same way as a unique constraint.
func TestRunDesiredAddsAPrimaryKeyToALiveTable(t *testing.T) {
	pool, schema := liveEvents(t)
	runKeyAdd(t, pool, schema, `
CREATE TABLE events (
    id bigint NOT NULL,
    email text,
    name text,
    CONSTRAINT events_pkey PRIMARY KEY (id)
);`, "events_pkey", []string{
		fmt.Sprintf(`CREATE UNIQUE INDEX CONCURRENTLY "events_pkey" ON "%s"."events" ("id")`, schema),
		fmt.Sprintf(`ALTER TABLE "%s"."events" ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey"`, schema),
	}, schemadiff.IndexAdoption{Index: "events_pkey", Def: "CREATE UNIQUE INDEX events_pkey ON events USING btree (id)"})
}

// A deferrable key whose index has storage parameters builds its index with
// the parameters and attaches it deferrable. The definition the build leaves
// is the one the plan recorded, so the attach's check passes: deferral is
// the constraint's, not the index's.
func TestRunDesiredAddsAKeyWithIndexOptions(t *testing.T) {
	pool, schema := liveEvents(t)
	runKeyAdd(t, pool, schema, `
CREATE TABLE events (
    id bigint NOT NULL,
    email text,
    name text,
    CONSTRAINT events_email_key UNIQUE (email) WITH (fillfactor = 70) DEFERRABLE INITIALLY DEFERRED
);`, "events_email_key", []string{
		fmt.Sprintf(`CREATE UNIQUE INDEX CONCURRENTLY "events_email_key" ON "%s"."events" ("email") WITH ("fillfactor" = '70')`, schema),
		fmt.Sprintf(`ALTER TABLE "%s"."events" ADD CONSTRAINT "events_email_key" UNIQUE USING INDEX "events_email_key" DEFERRABLE INITIALLY DEFERRED`, schema),
	}, schemadiff.IndexAdoption{Index: "events_email_key", Def: "CREATE UNIQUE INDEX events_email_key ON events USING btree (email) WITH (fillfactor='70')"})
}

// An index put under the key's name between the build and the attach is not
// taken over. An adapter that runs the planned steps on its own builds the
// index, someone replaces it with an index of another definition, and the
// attach, run with the adoption the plan recorded, refuses with
// adopted-index-drift and leaves the table without the constraint.
func TestPlannedKeyAttachRefusesAnIndexSwappedInAfterTheBuild(t *testing.T) {
	pool, schema := liveEvents(t)
	report, err := diffplan.Plan(t.Context(), pool, diffplan.Request{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE events (
    id bigint NOT NULL,
    email text,
    name text,
    CONSTRAINT events_name_key UNIQUE (name)
);`)})
	require.NoError(t, err)
	require.Len(t, report.Statements, 1)
	st := report.Statements[0]
	require.Len(t, st.ExecSQL, 2, "a concurrent build, then an attach")
	require.NotNil(t, st.Adopts)

	_, err = pool.Exec(t.Context(), st.ExecSQL[0])
	require.NoError(t, err, "the build")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		DROP INDEX %[1]s.events_name_key;
		CREATE UNIQUE INDEX events_name_key ON %[1]s.events (email)`, schema))
	require.NoError(t, err, "an index of another definition takes the name")

	err = attachPlannedKey(t.Context(), pool, schema, st)
	require.ErrorIs(t, err, executor.ErrAdoptedIndexDrift)
	assert.Equal(t, executor.CodeAdoptedIndexDrift, executor.OutcomeCode(err))
	var constraints int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM pg_catalog.pg_constraint WHERE conrelid = $1::regclass AND conname = 'events_name_key'",
		schema+".events").Scan(&constraints))
	assert.Zero(t, constraints, "the swapped-in index is not attached")
}

// attachPlannedKey runs the attach step of a planned key through the
// adopting entry point, with the adoption the plan recorded.
func attachPlannedKey(ctx context.Context, pool *pgxpool.Pool, schema string, st plan.Statement) error {
	pt, err := preflight.CheckTable(ctx, pool, schema, "events", preflight.NoSizeLimit)
	if err != nil {
		return fmt.Errorf("preflight events: %w", err)
	}
	_, err = executor.RunSequenceAdopting(ctx, pool, pt, st.ExecSQL[1:], runOptions().Budget, executor.DefaultRetryPolicy(), *st.Adopts)
	return err
}
