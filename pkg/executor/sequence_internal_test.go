// White-box tests for sequence admission: the shape classification and
// fail-closed refusals that decide which budget class each step runs under
// — decisions that must be provable without a database.

package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

func TestAdmitStepClassifiesShapes(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want StepKind
	}{
		{
			name: "ADD CONSTRAINT NOT VALID is brief",
			sql:  `ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID`,
			want: StepBrief,
		},
		{
			name: "a lone VALIDATE CONSTRAINT gets the validate class",
			sql:  `ALTER TABLE s.t VALIDATE CONSTRAINT c`,
			want: StepValidateConstraint,
		},
		{
			name: "SET NOT NULL is brief",
			sql:  `ALTER TABLE s.t ALTER COLUMN v SET NOT NULL`,
			want: StepBrief,
		},
		{
			name: "the scaffold DROP CONSTRAINT is brief",
			sql:  `ALTER TABLE s.t DROP CONSTRAINT c`,
			want: StepBrief,
		},
		{
			name: "a fast-default ADD COLUMN is brief",
			sql:  `ALTER TABLE s.t ADD COLUMN age int NOT NULL DEFAULT 0`,
			want: StepBrief,
		},
		{
			name: "a metadata-only change is brief",
			sql:  `ALTER TABLE s.t ALTER COLUMN v DROP DEFAULT`,
			want: StepBrief,
		},
		{
			name: "ADD CONSTRAINT USING INDEX is brief",
			sql:  `ALTER TABLE s.t ADD CONSTRAINT t_pkey PRIMARY KEY USING INDEX t_pkey`,
			want: StepBrief,
		},
		{
			name: "a concurrent index build is delegated",
			sql:  `CREATE UNIQUE INDEX CONCURRENTLY i ON s.t (v)`,
			want: StepConcurrentIndexBuild,
		},
		{
			name: "a VALIDATE sharing a multi-operation ALTER runs brief",
			sql:  `ALTER TABLE s.t VALIDATE CONSTRAINT c, ADD COLUMN x int`,
			want: StepBrief,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step, err := admitStep("s", "t", tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, step.kind)
		})
	}
}

func TestAdmitStepRefusals(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{
			name:    "a blocking CREATE INDEX never belongs in a sequence",
			sql:     `CREATE INDEX i ON s.t (v)`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "an unnamed concurrent build is refused at admission, not mid-run",
			sql:     `CREATE UNIQUE INDEX CONCURRENTLY ON s.t (v)`,
			wantErr: ErrUnnamedIndex,
		},
		{
			name:    "IF NOT EXISTS on a concurrent build is refused at admission, not mid-run",
			sql:     `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS i ON s.t (v)`,
			wantErr: ErrIfNotExistsUnsupported,
		},
		{
			name:    "DROP INDEX CONCURRENTLY is not driven",
			sql:     `DROP INDEX CONCURRENTLY s.i`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "REINDEX CONCURRENTLY is not driven",
			sql:     `REINDEX INDEX CONCURRENTLY s.i`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "a cancelled DETACH PARTITION CONCURRENTLY leaves detach-pending state",
			sql:     `ALTER TABLE s.t DETACH PARTITION p CONCURRENTLY`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "CREATE TABLE is not a sequence step",
			sql:     `CREATE TABLE s.t (id int)`,
			wantErr: ErrUnsupportedSequenceStep,
		},
		{
			name:    "a step against another table breaks the preflight binding",
			sql:     `ALTER TABLE s.other DROP CONSTRAINT c`,
			wantErr: ErrInvariantViolation,
		},
		{
			name:    "a step against another schema breaks the preflight binding",
			sql:     `ALTER TABLE elsewhere.t DROP CONSTRAINT c`,
			wantErr: ErrInvariantViolation,
		},
		{
			name:    "an unqualified index build is refused by the build admission before the target check",
			sql:     `CREATE UNIQUE INDEX CONCURRENTLY i ON t (v)`,
			wantErr: ErrUnqualifiedTable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := admitStep("s", "t", tt.sql)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

// TestAdmitStepRefusesUnqualifiedBuildUnderUnqualifiedPreflight covers the
// one build-qualification gap the ST-7 target check cannot see: with an
// unqualified preflight, an unqualified build's schemas match ("" == ""),
// so only the delegated executor's own admission refuses it — and that
// refusal must fire here, before anything executes.
func TestAdmitStepRefusesUnqualifiedBuildUnderUnqualifiedPreflight(t *testing.T) {
	_, err := admitStep("", "t", `CREATE UNIQUE INDEX CONCURRENTLY i ON t (v)`)
	require.ErrorIs(t, err, ErrUnqualifiedTable)
}

func TestAdmitSequenceNamesTheOffendingStep(t *testing.T) {
	steps := []string{
		`ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID`,
		`CREATE TABLE s.t (id int)`,
	}
	_, err := admitSequence("s", "t", steps)
	require.ErrorIs(t, err, ErrUnsupportedSequenceStep)
	var stepErr *SequenceStepError
	assert.False(t, errors.As(err, &stepErr), "an admission refusal is not a step failure: nothing executed")
}

func TestAdmitSequenceSurfacesParseFailures(t *testing.T) {
	_, err := admitSequence("s", "t", []string{`ALTER TABLE s.t THIS IS NOT SQL`})
	require.Error(t, err)
}

// skewedClock reads a fixed instant far from the wall clock, so any code
// path that mixes it with time.Now()/time.Since produces a wildly wrong
// elapsed value instead of a subtly wrong one.
type skewedClock struct{ now time.Time }

func (c *skewedClock) Now() time.Time { return c.now }

// TestCorroborateValidateCancel covers the validate step's cancellation
// partition, in precedence order: a statement-budget verdict at or past the
// overall budget stands whatever the caller's context did; below it, an
// ended caller context claims the cancellation in whichever form it
// reached the executor; a live one makes the server's cancel external.
// Neither reclassified failure may remain a *BudgetError — that type is
// the consumer's escalation trigger.
func TestCorroborateValidateCancel(t *testing.T) {
	budget := ValidateBudget{LockTimeout: time.Second, Overall: time.Hour}
	statementBudget := &BudgetError{Cause: CauseStatement, Budget: budget.Overall}
	lockBudget := &BudgetError{Cause: CauseLock, Budget: budget.LockTimeout}
	live := t.Context()
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	assertNotBudget := func(t *testing.T, err error) {
		t.Helper()
		var budgetErr *BudgetError
		assert.False(t, errors.As(err, &budgetErr), "a reclassified cancel must not read as budget exhaustion")
	}

	t.Run("a statement verdict at or past the budget stands under a live context", func(t *testing.T) {
		err := corroborateValidateCancel(live, statementBudget, budget, budget.Overall)
		assert.Same(t, statementBudget, err)
	})
	t.Run("a statement verdict at or past the budget stands under an ended context", func(t *testing.T) {
		err := corroborateValidateCancel(ended, statementBudget, budget, budget.Overall+time.Second)
		assert.Same(t, statementBudget, err)
	})
	t.Run("an early statement verdict under an ended context is the caller's cancellation", func(t *testing.T) {
		err := corroborateValidateCancel(ended, statementBudget, budget, time.Minute)
		require.ErrorIs(t, err, ErrCancelledByCaller)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		assertNotBudget(t, err)
	})
	t.Run("the client's own context error under an ended context is the caller's cancellation", func(t *testing.T) {
		err := corroborateValidateCancel(ended, fmt.Errorf("validate: %w", context.Canceled), budget, time.Minute)
		require.ErrorIs(t, err, ErrCancelledByCaller)
		assert.NotErrorIs(t, err, ErrCancelledExternally)
		assertNotBudget(t, err)
	})
	t.Run("an early statement verdict under a live context is an external cancellation", func(t *testing.T) {
		err := corroborateValidateCancel(live, statementBudget, budget, time.Minute)
		require.ErrorIs(t, err, ErrCancelledExternally)
		assert.NotErrorIs(t, err, ErrCancelledByCaller)
		assertNotBudget(t, err)
	})
	t.Run("a lock verdict is not a cancellation in either context", func(t *testing.T) {
		assert.Same(t, lockBudget, corroborateValidateCancel(live, lockBudget, budget, time.Minute))
		assert.Same(t, lockBudget, corroborateValidateCancel(ended, lockBudget, budget, time.Minute))
	})
	t.Run("any other failure passes through", func(t *testing.T) {
		boom := errors.New("connection reset")
		assert.Same(t, boom, corroborateValidateCancel(ended, boom, budget, time.Minute))
	})
	t.Run("success passes through", func(t *testing.T) {
		assert.NoError(t, corroborateValidateCancel(ended, nil, budget, time.Minute))
	})
}

// Elapsed values fed to budget corroboration and step reports must come
// from the same clock that produced the start instant: a tracker's injected
// clock when present, the wall clock otherwise.
func TestElapsedSinceReadsTheClockThatProducedStart(t *testing.T) {
	clock := &skewedClock{now: time.Unix(1000, 0)}
	tracker, err := progress.NewTracker(clock)
	require.NoError(t, err)

	start := tracker.Now()
	clock.now = clock.now.Add(5 * time.Second)
	assert.Equal(t, 5*time.Second, elapsedSince(tracker, start),
		"with a tracker, elapsed must be measured on its injected clock")

	wallStart := time.Now()
	elapsed := elapsedSince(nil, wallStart)
	assert.GreaterOrEqual(t, elapsed, time.Duration(0))
	assert.Less(t, elapsed, time.Minute, "without a tracker, elapsed must be wall-clock time since start")
}
