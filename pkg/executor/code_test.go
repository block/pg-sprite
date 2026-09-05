package executor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
)

func TestOutcomeCodeMapsTypedOutcomes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want executor.Code
	}{
		{name: "nil error has no code", err: nil, want: executor.Code("")},
		{
			name: "lock budget",
			err:  &executor.BudgetError{Cause: executor.CauseLock, Budget: time.Second},
			want: executor.CodeBudgetLockExceeded,
		},
		{
			name: "statement budget",
			err:  &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Second},
			want: executor.CodeBudgetStatementExceeded,
		},
		{
			name: "own invalid leftover",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrBuildLeftInvalidIndex},
			want: executor.CodeInvalidIndexOwnLeftover,
		},
		{
			name: "invalid index build in flight",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", BuilderPID: 42, Cleanup: executor.ErrInvalidIndexBuildInFlight},
			want: executor.CodeInvalidIndexBuildInFlight,
		},
		{
			name: "abandoned invalid index",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t", Cleanup: executor.ErrAbandonedInvalidIndex},
			want: executor.CodeInvalidIndexAbandoned,
		},
		{
			name: "invalid index on another table",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "u", Cleanup: executor.ErrInvalidIndexOnOtherTable},
			want: executor.CodeInvalidIndexOtherTable,
		},
		{
			name: "invalid index with an unobservable builder",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Table: "t", Cleanup: executor.ErrInvalidIndexBuilderUnobservable},
			want: executor.CodeInvalidIndexBuilderUnobservable,
		},
		{
			name: "unproven invalid index",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrTargetIdentityChanged},
			want: executor.CodeInvalidIndexUnproven,
		},
		{
			name: "abandonment unproven through to removal",
			err:  &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrAbandonmentUnproven},
			want: executor.CodeInvalidIndexUnproven,
		},
		{name: "cancelled externally", err: executor.ErrCancelledExternally, want: executor.CodeCancelledExternally},
		{name: "empty sequence", err: executor.ErrEmptySequence, want: executor.CodeEmptySequence},
		{name: "unsupported sequence step", err: executor.ErrUnsupportedSequenceStep, want: executor.CodeUnsupportedSequenceStep},
		{name: "not a concurrent build", err: executor.ErrNotConcurrentIndexBuild, want: executor.CodeNotConcurrentIndexBuild},
		{name: "unnamed index", err: executor.ErrUnnamedIndex, want: executor.CodeUnnamedIndex},
		{name: "unqualified table", err: executor.ErrUnqualifiedTable, want: executor.CodeUnqualifiedTable},
		{name: "if not exists", err: executor.ErrIfNotExistsUnsupported, want: executor.CodeIfNotExistsUnsupported},
		{name: "create collision", err: executor.ErrCreateCollision, want: executor.CodeCreateCollision},
		{name: "duplicate create name", err: executor.ErrDuplicateCreateName, want: executor.CodeDuplicateCreateName},
		{name: "partition of", err: executor.ErrPartitionOfUnsupported, want: executor.CodePartitionOfUnsupported},
		{name: "unsupported create step", err: executor.ErrUnsupportedCreateStep, want: executor.CodeUnsupportedCreateStep},
		{name: "pool too small", err: executor.ErrPoolTooSmall, want: executor.CodePoolTooSmall},
		{name: "table not found", err: executor.ErrTableNotFound, want: executor.CodeTableNotFound},
		{name: "invariant violation", err: executor.ErrInvariantViolation, want: executor.CodeInvariantViolation},
		{name: "untyped error is the fallback", err: errors.New("connection reset"), want: executor.CodeExecutionFailed},
		{
			name: "wrapped sentinel still maps",
			err:  fmt.Errorf("sequence step 2 of 3: %w", executor.ErrUnsupportedSequenceStep),
			want: executor.CodeUnsupportedSequenceStep,
		},
		{
			name: "step error carries its cause's code",
			err: &executor.SequenceStepError{
				Step: 2, Total: 3, Kind: executor.StepBrief, SQL: "ALTER TABLE s.t ADD c int",
				Err: &executor.BudgetError{Cause: executor.CauseStatement, Budget: time.Second},
			},
			want: executor.CodeBudgetStatementExceeded,
		},
		{
			name: "step error with an untyped cause is the fallback",
			err: &executor.SequenceStepError{
				Step: 1, Total: 1, Kind: executor.StepBrief, SQL: "ALTER TABLE s.t ADD c int",
				Err: errors.New("server closed the connection"),
			},
			want: executor.CodeExecutionFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, executor.OutcomeCode(tt.err))
		})
	}
}

func TestSequenceStepErrorCodeMatchesOutcomeCode(t *testing.T) {
	stepErr := &executor.SequenceStepError{
		Step: 1, Total: 2, Kind: executor.StepConcurrentIndexBuild, SQL: "CREATE INDEX CONCURRENTLY i ON s.t (c)",
		Err: &executor.InvalidIndexError{Schema: "s", Index: "i", Cleanup: executor.ErrBuildLeftInvalidIndex},
	}
	assert.Equal(t, executor.CodeInvalidIndexOwnLeftover, stepErr.Code())
	assert.Equal(t, stepErr.Code(), executor.OutcomeCode(stepErr))
}

// TestSequenceReportJSONContractIsStable pins the report's wire shape:
// consumers parse these exact keys, so a key change is a contract break
// this test makes deliberate.
func TestSequenceReportJSONContractIsStable(t *testing.T) {
	rep := executor.SequenceReport{
		Steps: []executor.StepReport{
			{
				SQL:      "ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID",
				Kind:     executor.StepBrief,
				Duration: 20 * time.Millisecond,
			},
			{
				SQL:      "CREATE INDEX CONCURRENTLY i ON s.t (v)",
				Kind:     executor.StepConcurrentIndexBuild,
				Duration: 1500 * time.Millisecond,
				Index: &executor.IndexBuildReport{
					Schema:        "s",
					Index:         "i",
					IndexOID:      41235,
					Duration:      1400 * time.Millisecond,
					ServerVersion: "16.4",
				},
			},
		},
	}
	got, err := json.Marshal(rep)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"steps": [
			{
				"sql": "ALTER TABLE s.t ADD CONSTRAINT c CHECK (v > 0) NOT VALID",
				"kind": "brief",
				"duration_ns": 20000000
			},
			{
				"sql": "CREATE INDEX CONCURRENTLY i ON s.t (v)",
				"kind": "concurrent-index-build",
				"duration_ns": 1500000000,
				"index": {
					"schema": "s",
					"index": "i",
					"index_oid": 41235,
					"duration_ns": 1400000000,
					"server_version": "16.4"
				}
			}
		]
	}`, string(got))
}
