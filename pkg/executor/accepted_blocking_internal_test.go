package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestBlockingBudgetValidation(t *testing.T) {
	valid := BlockingBudget{LockTimeout: time.Second, StatementTimeout: time.Minute}
	tests := []struct {
		name   string
		budget BlockingBudget
		ok     bool
	}{
		{name: "zero lock", budget: BlockingBudget{StatementTimeout: time.Second}},
		{name: "negative lock", budget: BlockingBudget{LockTimeout: -time.Second, StatementTimeout: time.Second}},
		{name: "lock over ceiling", budget: BlockingBudget{LockTimeout: maxOverallBudget + time.Millisecond, StatementTimeout: time.Second}},
		{name: "zero statement", budget: BlockingBudget{LockTimeout: time.Second}},
		{name: "negative statement", budget: BlockingBudget{LockTimeout: time.Second, StatementTimeout: -time.Second}},
		{name: "statement over ceiling", budget: BlockingBudget{LockTimeout: time.Second, StatementTimeout: maxOverallBudget + time.Millisecond}},
		{name: "valid", budget: valid, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.budget.validate()
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidBlockingBudget)
			assert.Equal(t, CodeInvalidBlockingBudget, OutcomeCode(err))
		})
	}
}

func TestAcceptedBlockingStatementErrorMapsSQLSTATE(t *testing.T) {
	b := BlockingBudget{LockTimeout: time.Second, StatementTimeout: time.Minute}
	tests := []struct {
		name string
		code string
		want Code
	}{
		{name: "lock", code: sqlstateLockNotAvailable, want: CodeBudgetLockExceeded},
		{name: "statement", code: sqlstateQueryCanceled, want: CodeBudgetStatementExceeded},
		{name: "other postgres", code: "23505", want: CodeExecutionFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := acceptedBlockingStatementError(t.Context(), &pgconn.PgError{Code: tt.code}, b)
			assert.Equal(t, tt.want, OutcomeCode(err))
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := acceptedBlockingStatementError(ctx, errors.New("connection lost"), b)
	var unknown *BlockingOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, CodeBlockingOutcomeUnknown, OutcomeCode(err))
}

func TestAcceptedBlockingAdmission(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{sql: "DROP INDEX app.i", want: true},
		{sql: "REINDEX INDEX app.i", want: true},
		{sql: "REINDEX TABLE app.t", want: true},
		{sql: "CREATE INDEX i ON app.t (id)", want: true},
		{sql: "DROP INDEX CONCURRENTLY app.i"},
		{sql: "DROP INDEX app.i, app.j"},
		{sql: "REINDEX SCHEMA app"},
		{sql: "DELETE FROM app.t"},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			st, err := statement.ParseOne(tt.sql)
			require.NoError(t, err)
			assert.Equal(t, tt.want, acceptedBlockingShape(st))
		})
	}
}
