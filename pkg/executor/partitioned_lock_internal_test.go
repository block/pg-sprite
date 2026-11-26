package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// A lost table lock usually cancels the step that was running, so the step
// fails with a typed error of its own: an invalid index its cancelled build
// left, or an expired budget. The lost lock still decides the outcome code,
// because it voids whatever the step did, and the step's account stays in the
// text for the operator.
func TestLockLostErrorOutranksTheStepsTypedError(t *testing.T) {
	lost := fmt.Errorf("table lock keepalive: %w", errors.New("connection reset"))
	steps := []struct {
		name string
		err  error
		text string
	}{
		{"an invalid index left by the cancelled build", &InvalidIndexError{Schema: "app", Index: "idx_p1", Cleanup: ErrBuildLeftInvalidIndex}, "idx_p1"},
		{"an expired lock budget", &BudgetError{Cause: CauseLock}, "lock"},
		{"an expired statement budget", &BudgetError{Cause: CauseStatement}, "statement"},
		{"an unknown commit outcome", &CommitOutcomeUnknownError{Err: errors.New("commit attach")}, "commit attach"},
		{"no step error", nil, ""},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			err := lockLostError(lost, s.err)

			assert.Equal(t, CodeInvariantViolation, OutcomeCode(err))
			assert.ErrorIs(t, err, ErrInvariantViolation)
			assert.ErrorIs(t, err, dbconn.ErrTableLockLost)
			assert.Contains(t, err.Error(), "LK-1")
			assert.Contains(t, err.Error(), "connection reset")
			if s.err != nil {
				require.NotErrorIs(t, err, s.err, "the step's error is text, not a cause")
				assert.Contains(t, err.Error(), s.text)
			}
		})
	}
}
