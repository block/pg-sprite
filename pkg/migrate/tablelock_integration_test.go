package migrate_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// twoInstances is what LK-1 is about: two pg-sprite processes pointed at one
// database, each with its own pool and its own sessions.
func twoInstances(t *testing.T) (first, second *pgxpool.Pool, schema string) {
	t.Helper()
	url := testutil.StartPostgres(t)
	first = newInstancePool(t, url)
	second = newInstancePool(t, url)
	schema = testutil.NewSchema(t, first)
	_, err := first.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	return first, second, schema
}

func newInstancePool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// Two instances asked to change one table at the same time do not both
// proceed. The second gets a typed refusal rather than an error: nothing was
// planned or executed and the request is safe to retry, which is what lets an
// orchestrator treat concurrency as a scheduling condition rather than a
// failure. The lock is released with the run, so the retry succeeds.
func TestRunRefusesWhileAnotherInstanceHoldsTheLock(t *testing.T) {
	first, second, schema := twoInstances(t)

	held, err := dbconn.AcquireTableLock(t.Context(), first, schema, "t", dbconn.TableLockOptions{})
	require.NoError(t, err)

	st := parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN age int", schema))
	v, err := migrate.Run(t.Context(), second, st, runOptions())
	require.NoError(t, err, "another instance holding the lock is a refusal, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonTableLocked, v.Reason)
	assert.Equal(t, schema+".t", v.Table)
	assert.False(t, columnExists(t, first, schema, "t", "age"), "a refused run executes nothing")

	require.NoError(t, held.Release(t.Context()))

	v, err = migrate.Run(t.Context(), second, st, runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, "the refusal must be transient, not a permanent claim")
}

// A convergence takes the lock before it derives the plan, so the whole
// decide-then-act sequence is atomic against another instance: a second
// instance is refused before it reads the facts its plan would be built
// from, rather than planning against a shape the first instance is midway
// through changing.
func TestRunDesiredRefusesWhileAnotherInstanceHoldsTheLock(t *testing.T) {
	first, second, schema := twoInstances(t)

	held, err := dbconn.AcquireTableLock(t.Context(), first, schema, "t", dbconn.TableLockOptions{})
	require.NoError(t, err)
	defer func() { _ = held.Release(t.Context()) }()

	req := migrate.DesiredRequest{Schema: schema,
		Desired: parseDesired(t, "CREATE TABLE t (id int PRIMARY KEY, age int);")}
	res, err := migrate.RunDesired(t.Context(), second, req, runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
	assert.Equal(t, verdict.ReasonTableLocked, res.Reason)
	assert.Empty(t, res.Plan.Statements, "the refusal must come before a plan is derived")
	assert.False(t, columnExists(t, first, schema, "t", "age"))
}

// The lock serializes per table, so two instances converging different
// tables in one schema run at the same time. Serializing more broadly would
// make an unrelated long change look like an outage to every other table.
func TestRunDoesNotSerializeAcrossTables(t *testing.T) {
	first, second, schema := twoInstances(t)
	_, err := first.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other (id int PRIMARY KEY)", schema))
	require.NoError(t, err)

	held, err := dbconn.AcquireTableLock(t.Context(), first, schema, "t", dbconn.TableLockOptions{})
	require.NoError(t, err)
	defer func() { _ = held.Release(t.Context()) }()

	v, err := migrate.Run(t.Context(), second,
		parseOne(t, fmt.Sprintf("ALTER TABLE %s.other ADD COLUMN age int", schema)), runOptions())

	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
}

// columnExists is the catalog oracle for whether a refused run executed
// anything.
func columnExists(t *testing.T, pool *pgxpool.Pool, schema, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (
		   SELECT FROM information_schema.columns
		    WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)`,
		schema, table, column).Scan(&exists))
	return exists
}
