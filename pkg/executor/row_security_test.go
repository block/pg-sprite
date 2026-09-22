package executor_test

import (
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/stretchr/testify/require"
)

func TestExecuteRowSecurityRejectsInvalidInputsBeforeConnecting(t *testing.T) {
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	_, err = executor.ExecuteRowSecurity(t.Context(), nil, "public", desired, executor.Budget{})
	require.Error(t, err)
	budget := executor.Budget{LockTimeout: time.Millisecond, StatementTimeout: time.Second}
	_, err = executor.ExecuteRowSecurity(t.Context(), nil, "public", statement.DesiredWithRowSecurity{}, budget)
	require.ErrorIs(t, err, statement.ErrRowSecurityDeclaration)
	_, err = executor.ExecuteRowSecurity(t.Context(), nil, "", desired, budget)
	require.ErrorIs(t, err, statement.ErrRowSecurityDeclaration)
}
