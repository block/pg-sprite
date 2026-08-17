package executor_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// The *WithProgress entry points exist for callers that poll; a nil tracker
// is a caller bug they must refuse with a typed error before anything else
// runs — never a panic, and never a silent fallback to unobserved execution.

func TestExecuteNativeWithProgressRequiresTracker(t *testing.T) {
	err := executor.ExecuteNativeWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		statement.Statement{}, executor.Budget{LockTimeout: time.Second, StatementTimeout: time.Second},
		executor.DefaultRetryPolicy(), nil)
	require.Error(t, err)
}

func TestRunSequenceWithProgressRequiresTracker(t *testing.T) {
	_, err := executor.RunSequenceWithProgress(t.Context(), nil, preflight.PreflightedTable{},
		[]string{"ALTER TABLE s.t ADD COLUMN v int"},
		executor.SequenceBudget{}, executor.DefaultRetryPolicy(), nil)
	require.Error(t, err)
}

func TestBuildIndexConcurrentlyWithProgressRequiresTracker(t *testing.T) {
	_, err := executor.BuildIndexConcurrentlyWithProgress(t.Context(), nil,
		"CREATE INDEX CONCURRENTLY i ON s.t (c)", executor.ConcurrentBudget{Overall: time.Minute}, nil)
	require.Error(t, err)
}
