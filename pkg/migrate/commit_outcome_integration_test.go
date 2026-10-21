package migrate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A COMMIT the client never heard back from may have landed, so the failed
// verdict says the outcome is unknown and points at the catalog — it never
// claims the rollback a failed attempt normally gets, and it never lists the
// unknown step as committed.
func TestRunReportsALostCommitAsAnUnknownOutcome(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("a one-step change reports its only step as unknown", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
		require.NoError(t, err)
		testutil.LoseConnectionAtCommit(t, pool, schema, "ADD COLUMN lost_commit")

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN lost_commit int", schema)), runOptions())

		var unknown *executor.CommitOutcomeUnknownError
		require.ErrorAs(t, err, &unknown)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeCommitOutcomeUnknown), v.Code)
		assert.Equal(t, 1, v.FailedStep)
		assert.Empty(t, v.ExecutedSQL, "no earlier step committed; the failed step is unknown, not committed")
		assert.Contains(t, v.Detail, "outcome unknown")
		assert.NotContains(t, v.Detail, "nothing committed")
	})

	t.Run("a mid-sequence loss lists the earlier steps as committed and the step as unknown", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.t (id int PRIMARY KEY, v text);
			INSERT INTO %[1]s.t SELECT g, 'x' FROM generate_series(1, 100) g`, schema))
		require.NoError(t, err)
		// SET NOT NULL runs as a four-step sequence; only the third step,
		// the SET NOT NULL itself, loses its COMMIT.
		testutil.LoseConnectionAtCommit(t, pool, schema, "SET NOT NULL")

		v, err := migrate.Run(t.Context(), pool,
			parseOne(t, fmt.Sprintf("ALTER TABLE %s.t ALTER COLUMN v SET NOT NULL", schema)), runOptions())

		var unknown *executor.CommitOutcomeUnknownError
		require.ErrorAs(t, err, &unknown)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeCommitOutcomeUnknown), v.Code)
		assert.Equal(t, 3, v.FailedStep)
		assert.Contains(t, v.FailedStepSQL, "SET NOT NULL")
		require.Len(t, v.ExecutedSQL, 2, "steps 1 and 2 committed; step 3 is unknown")
		assert.Contains(t, v.ExecutedSQL[0], "NOT VALID")
		assert.Contains(t, v.ExecutedSQL[1], "VALIDATE CONSTRAINT")
		assert.Contains(t, v.Detail, "outcome unknown")
		assert.Contains(t, v.Detail, "inspect the catalog")
	})
}

// A desired-state run whose statement loses its COMMIT reports, in the
// aggregate as well as the statement's verdict, an unknown outcome, never a
// plain failure: for an alter of an existing table and for a create of a new
// one, which run through different executor paths.
func TestRunDesiredReportsALostCommitAsAnUnknownOutcome(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("an alter of an existing table", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %s.orders (
				id bigint PRIMARY KEY
			)`, schema))
		require.NoError(t, err)
		testutil.LoseConnectionAtCommit(t, pool, schema, "ADD COLUMN note")

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE orders (
	id   bigint PRIMARY KEY,
	note text
);`)}, runOptions())

		var unknown *executor.CommitOutcomeUnknownError
		require.ErrorAs(t, err, &unknown)
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		require.Len(t, res.Verdicts, 1)
		assert.Equal(t, string(executor.CodeCommitOutcomeUnknown), res.Verdicts[0].Code)
		assert.Contains(t, res.Detail, "unknown commit outcome")
		assert.NotContains(t, res.Detail, "failed;")
	})

	// The create step's COMMIT is lost the same way LoseConnectionAtCommit
	// loses an ALTER's: the CREATE TABLE queues a row whose deferred trigger
	// terminates the backend inside COMMIT.
	t.Run("a create of a new table", func(t *testing.T) {
		schema := testutil.NewSchema(t, pool)
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`
			CREATE TABLE %[1]s.commit_probe (id integer);
			CREATE FUNCTION %[1]s.lose_connection() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				PERFORM pg_terminate_backend(pg_backend_pid());
				PERFORM pg_sleep(30);
				RETURN NULL;
			END;
			$$;
			CREATE CONSTRAINT TRIGGER lose_connection AFTER INSERT ON %[1]s.commit_probe
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %[1]s.lose_connection()`, schema))
		require.NoError(t, err)
		testutil.InstallEventTrigger(t, pool, testutil.DDLCommandEnd, schema, "queue_commit_fault", fmt.Sprintf(`
			IF TG_TAG = 'CREATE TABLE' AND current_query() LIKE '%%lost_create%%' THEN
				INSERT INTO %s.commit_probe VALUES (1);
			END IF;`, schema))

		res, err := migrate.RunDesired(t.Context(), pool, migrate.DesiredRequest{Schema: schema, Desired: parseDesired(t, `
CREATE TABLE lost_create (
	id bigint PRIMARY KEY
);`)}, runOptions())

		var unknown *executor.CommitOutcomeUnknownError
		require.ErrorAs(t, err, &unknown)
		assert.Equal(t, verdict.OutcomeFailed, res.Outcome)
		require.Len(t, res.Verdicts, 1)
		assert.Equal(t, string(executor.CodeCommitOutcomeUnknown), res.Verdicts[0].Code)
		assert.Contains(t, res.Verdicts[0].Detail, "cannot be established")
		assert.Contains(t, res.Detail, "unknown commit outcome")
		assert.NotContains(t, res.Detail, "failed;")
	})
}
