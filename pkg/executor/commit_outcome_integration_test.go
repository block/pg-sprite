package executor_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/executor"
)

// A COMMIT that ends without a success reply may have landed, so the bounded
// attempt must report an unknown outcome — never the rollback an attempt that
// failed before COMMIT gets, and never a budget refusal a caller would retry.
func TestExecuteNativeTypesALostCommitAsOutcomeUnknown(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	testutil.LoseConnectionAtCommit(t, pool, schema, "ADD COLUMN lost_commit")

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN lost_commit int", schema))
	err = executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())

	var unknown *executor.CommitOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, executor.CodeCommitOutcomeUnknown, executor.OutcomeCode(err))
	var budgetErr *executor.BudgetError
	assert.NotErrorAs(t, err, &budgetErr, "a lost commit is not a budget refusal")
	assert.False(t, executor.OutcomeCode(err).Permanent(), "the catalog decides whether a retry is needed")
}

// A brief step whose COMMIT is lost mid-sequence reports step 2 as the failed
// step with the committed prefix of step 1 only, and the code says step 2's
// own outcome is unknown: "retry resumes from step 2" is not safe to assume.
func TestRunSequenceTypesALostCommitMidSequenceAsOutcomeUnknown(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	testutil.LoseConnectionAtCommit(t, pool, schema, "ADD COLUMN lost_commit")

	steps := []string{
		fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN committed_first int", schema),
		fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN lost_commit int", schema),
	}
	rep, err := executor.RunSequence(t.Context(), pool, pt, steps, runBudget, executor.DefaultRetryPolicy())

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 2, stepErr.Step)
	assert.Equal(t, steps[1], stepErr.SQL)
	var unknown *executor.CommitOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, executor.CodeCommitOutcomeUnknown, executor.OutcomeCode(err))
	require.Len(t, rep.Steps, 1, "only step 1 is known committed")
	assert.Equal(t, steps[0], rep.Steps[0].SQL)
	assert.True(t, columnExists(t, pool, schema, "t", "committed_first"))
}

// A COMMIT the server answers with an error is typed unknown too, on purpose:
// a server reply at COMMIT is not always a rollback (a backend terminated
// while waiting for synchronous replication has committed locally and then
// reports FATAL), and reading an unknown outcome as a rollback is the
// unsafe direction. Here a deferred constraint trigger refuses the COMMIT;
// the server's error stays in the chain and the change did not land, yet
// the outcome is still unknown until the caller inspects the catalog.
func TestExecuteNativeTypesAServerAnsweredCommitErrorAsOutcomeUnknown(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id int PRIMARY KEY
		)`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.commit_probe (id integer);
		CREATE FUNCTION %[1]s.refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'refused at commit' USING ERRCODE = '23514';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER refuse_commit AFTER INSERT ON %[1]s.commit_probe
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %[1]s.refuse_commit()`, schema))
	require.NoError(t, err)
	testutil.InstallEventTrigger(t, pool, testutil.DDLCommandEnd, schema, "queue_commit_refusal", fmt.Sprintf(`
		IF TG_TAG = 'ALTER TABLE' THEN
			INSERT INTO %s.commit_probe VALUES (1);
		END IF;`, schema))

	st := mustParse(t, fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN c int", schema))
	err = executor.ExecuteNative(t.Context(), pool, pt, st, budget, executor.DefaultRetryPolicy())

	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23514", pgErr.Code, "the server answered the COMMIT with an error")
	assert.False(t, columnExists(t, pool, schema, "t", "c"), "and the change did not land")
	assert.Equal(t, executor.CodeCommitOutcomeUnknown, executor.OutcomeCode(err),
		"yet the outcome stays unknown: the client does not read a server reply at COMMIT as a rollback")
}
