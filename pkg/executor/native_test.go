package executor_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
)

// concurrentBudget is generous for unit tests; admission refusals return
// before any database access.
var concurrentBudget = executor.ConcurrentBudget{Overall: time.Minute}

func TestBuildIndexConcurrentlyRejectsUnboundedBudget(t *testing.T) {
	tests := []struct {
		name    string
		overall time.Duration
	}{
		{name: "zero disables the deadline", overall: 0},
		{name: "negative disables the deadline", overall: -time.Second},
		{name: "below a millisecond rounds to disabled", overall: 500 * time.Microsecond},
		{name: "above the server's 32-bit millisecond ceiling", overall: (math.MaxInt32 + 1) * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executor.BuildIndexConcurrently(t.Context(), nil,
				"CREATE INDEX CONCURRENTLY idx ON t (c)", executor.ConcurrentBudget{Overall: tt.overall})
			require.ErrorIs(t, err, executor.ErrUnboundedBudget)
		})
	}
}

func TestBuildIndexConcurrentlyRejectsCallerOwnedOverallBudget(t *testing.T) {
	_, err := executor.BuildIndexConcurrently(t.Context(), nil,
		"CREATE INDEX CONCURRENTLY idx ON public.t (c)",
		executor.ConcurrentBudget{Overall: time.Second, CallerOwned: true})
	require.ErrorIs(t, err, executor.ErrCallerOwnedOverallBudget)
}

func TestBuildIndexConcurrentlyCallerOwnedNeedsCancellableContext(t *testing.T) {
	_, err := executor.BuildIndexConcurrently(context.WithoutCancel(t.Context()), nil,
		"CREATE INDEX CONCURRENTLY idx ON public.t (c)",
		executor.ConcurrentBudget{CallerOwned: true})
	require.ErrorIs(t, err, executor.ErrCallerOwnedNeedsCancellableContext)
}

func TestBuildIndexConcurrentlyAdmission(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want error
	}{
		{
			name: "plain create index is refused",
			sql:  "CREATE INDEX idx ON t (c)",
			want: executor.ErrNotConcurrentIndexBuild,
		},
		{
			name: "alter table is refused",
			sql:  "ALTER TABLE t ADD COLUMN c int",
			want: executor.ErrNotConcurrentIndexBuild,
		},
		{
			name: "drop index concurrently is refused: this executor only builds",
			sql:  "DROP INDEX CONCURRENTLY idx",
			want: executor.ErrNotConcurrentIndexBuild,
		},
		{
			name: "reindex concurrently is refused",
			sql:  "REINDEX (CONCURRENTLY) TABLE t",
			want: executor.ErrNotConcurrentIndexBuild,
		},
		{
			name: "unnamed concurrent build is refused: no identity, no recovery",
			sql:  "CREATE INDEX CONCURRENTLY ON t (c)",
			want: executor.ErrUnnamedIndex,
		},
		{
			name: "if not exists is refused: a name-only no-op proves nothing about the existing index",
			sql:  "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON t (c)",
			want: executor.ErrIfNotExistsUnsupported,
		},
		{
			name: "unqualified table is refused: the verdict re-resolves the name on another session",
			sql:  "CREATE INDEX CONCURRENTLY idx ON t (c)",
			want: executor.ErrUnqualifiedTable,
		},
		{
			name: "multiple statements are refused",
			sql:  "CREATE INDEX CONCURRENTLY a ON t (c); CREATE INDEX CONCURRENTLY b ON t (d)",
			want: statement.ErrNotOneStatement,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executor.BuildIndexConcurrently(t.Context(), nil, tt.sql, concurrentBudget)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestBuildIndexConcurrentlyRejectsUnparsableSQL(t *testing.T) {
	_, err := executor.BuildIndexConcurrently(t.Context(), nil, "CREATE INDEX CONCURRENTLY WHERE", concurrentBudget)
	require.Error(t, err)
}
