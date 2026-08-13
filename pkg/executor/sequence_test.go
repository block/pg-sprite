package executor_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
)

// sequenceBudget is a fully-bounded budget set for unit tests; admission
// refusals return before any database access.
var sequenceBudget = executor.SequenceBudget{
	Brief:      executor.Budget{LockTimeout: 500 * time.Millisecond, StatementTimeout: 2 * time.Second},
	Concurrent: executor.ConcurrentBudget{Overall: time.Minute},
	Validate:   executor.ValidateBudget{LockTimeout: 500 * time.Millisecond, Overall: time.Minute},
}

func TestRunSequenceRejectsUnboundedBudgets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*executor.SequenceBudget)
	}{
		{name: "zero brief lock budget", mutate: func(b *executor.SequenceBudget) { b.Brief.LockTimeout = 0 }},
		{name: "zero brief statement budget", mutate: func(b *executor.SequenceBudget) { b.Brief.StatementTimeout = 0 }},
		{name: "zero concurrent overall budget", mutate: func(b *executor.SequenceBudget) { b.Concurrent.Overall = 0 }},
		{name: "zero validate lock budget", mutate: func(b *executor.SequenceBudget) { b.Validate.LockTimeout = 0 }},
		{name: "zero validate overall budget", mutate: func(b *executor.SequenceBudget) { b.Validate.Overall = 0 }},
		{name: "negative validate overall budget", mutate: func(b *executor.SequenceBudget) { b.Validate.Overall = -time.Second }},
		{
			name: "validate overall budget above the server ceiling",
			mutate: func(b *executor.SequenceBudget) {
				b.Validate.Overall = time.Duration(math.MaxInt32)*time.Millisecond + time.Millisecond
			},
		},
		{
			name: "validate lock budget above the server ceiling",
			mutate: func(b *executor.SequenceBudget) {
				b.Validate.LockTimeout = time.Duration(math.MaxInt32)*time.Millisecond + time.Millisecond
			},
		},
		{
			name: "brief statement budget above the server ceiling",
			mutate: func(b *executor.SequenceBudget) {
				b.Brief.StatementTimeout = time.Duration(math.MaxInt32)*time.Millisecond + time.Millisecond
			},
		},
		{
			name: "brief lock budget above the server ceiling",
			mutate: func(b *executor.SequenceBudget) {
				b.Brief.LockTimeout = time.Duration(math.MaxInt32)*time.Millisecond + time.Millisecond
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := sequenceBudget
			tt.mutate(&b)
			// A nil pool proves the refusal happens at admission, before
			// any database access.
			_, err := executor.RunSequence(t.Context(), nil, preflight.PreflightedTable{},
				[]string{"ALTER TABLE s.t DROP CONSTRAINT c"}, b, executor.DefaultRetryPolicy())
			require.Error(t, err)
			var stepErr *executor.SequenceStepError
			assert.False(t, errors.As(err, &stepErr), "a budget refusal must precede any step execution")
		})
	}
}

func TestRunSequenceRefusesEmptySequence(t *testing.T) {
	_, err := executor.RunSequence(t.Context(), nil, preflight.PreflightedTable{}, nil, sequenceBudget,
		executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrEmptySequence)
}

// The renderer's own unit test: a step-1 failure must not claim earlier
// steps committed, because none did.
func TestSequenceStepErrorNamesTheCommittedPrefix(t *testing.T) {
	cause := errors.New("boom")
	first := &executor.SequenceStepError{Step: 1, Total: 3, Kind: executor.StepBrief, Err: cause}
	assert.Contains(t, first.Error(), "no earlier steps had committed")
	assert.NotContains(t, first.Error(), "steps before it committed")

	later := &executor.SequenceStepError{Step: 2, Total: 3, Kind: executor.StepBrief, Err: cause}
	assert.Contains(t, later.Error(), "steps before it committed and their state remains")
}
