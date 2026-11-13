package executor_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
)

// A sequence run as step 3 of a caller's 5-step run reports every one of
// its statements as step 3 — the last one, the validation, is what the
// tracker shows once the call returns — and leaves the run's lifecycle to
// the caller: the tracker is still running, with the caller's step count,
// not finished by the sequence.
func TestRunSequenceAsStepReportsEveryStatementAsTheCallersStep(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.t (
			id int PRIMARY KEY,
			v  int
		)`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(5, progress.OperationAdmitting)

	steps := saferSequence(t, fmt.Sprintf("ALTER TABLE %s.t ADD CONSTRAINT v_positive CHECK (v > 0)", schema))
	require.Len(t, steps, 2, "the NOT VALID add, then the validation")
	rep, err := executor.RunSequenceAsStep(t.Context(), pool, pt, steps, runBudget, executor.DefaultRetryPolicy(),
		nil, nil, executor.RunStep{Tracker: tracker, Step: 3})
	require.NoError(t, err)
	require.Len(t, rep.Steps, 2)

	snapshot, err := tracker.Progress(t.Context())
	require.NoError(t, err)
	assert.Equal(t, progress.PhaseRunning, snapshot.Phase, "the caller finishes its run, not the sequence")
	assert.Equal(t, 3, snapshot.Step, "the sequence's second statement reports as the caller's step, not as step 2")
	assert.Equal(t, 5, snapshot.TotalSteps)
	assert.Equal(t, progress.OperationValidate, snapshot.Detail.Operation)
	assert.Equal(t, fmt.Sprintf(`ALTER TABLE %s VALIDATE CONSTRAINT "v_positive"`, pgx.Identifier{schema, "t"}.Sanitize()),
		snapshot.Detail.Statement)
}
