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
