package statement_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/statement"
)

func TestTransactionControlOf(t *testing.T) {
	cases := []struct {
		sql  string
		want statement.Transaction
	}{
		{"BEGIN", statement.Transaction{Control: statement.TransactionBegin}},
		{"BEGIN ISOLATION LEVEL SERIALIZABLE", statement.Transaction{Control: statement.TransactionBegin}},
		{"START TRANSACTION", statement.Transaction{Control: statement.TransactionBegin}},
		{"COMMIT", statement.Transaction{Control: statement.TransactionEnd}},
		{"END", statement.Transaction{Control: statement.TransactionEnd}},
		{"ROLLBACK", statement.Transaction{Control: statement.TransactionEnd}},
		{"ABORT", statement.Transaction{Control: statement.TransactionEnd}},
		{"PREPARE TRANSACTION 'gid'", statement.Transaction{Control: statement.TransactionEnd}},
		{"COMMIT AND CHAIN", statement.Transaction{Control: statement.TransactionChain}},
		{"ROLLBACK AND CHAIN", statement.Transaction{Control: statement.TransactionChain}},
		{"SAVEPOINT s", statement.Transaction{Control: statement.TransactionSavepoint, Savepoint: "s"}},
		{"RELEASE SAVEPOINT s", statement.Transaction{Control: statement.TransactionRelease, Savepoint: "s"}},
		{"ROLLBACK TO SAVEPOINT s", statement.Transaction{Control: statement.TransactionRollbackTo, Savepoint: "s"}},
		{"ROLLBACK TO s", statement.Transaction{Control: statement.TransactionRollbackTo, Savepoint: "s"}},
		{"RELEASE \"Mixed Case\"", statement.Transaction{Control: statement.TransactionRelease, Savepoint: "Mixed Case"}},
		{"COMMIT PREPARED 'gid'", statement.Transaction{}},
		{"CREATE INDEX CONCURRENTLY i ON t (c)", statement.Transaction{}},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			got, err := statement.TransactionControlOf(tc.sql)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTransactionControlOfRequiresOneStatement(t *testing.T) {
	_, err := statement.TransactionControlOf("BEGIN; COMMIT")
	require.ErrorIs(t, err, statement.ErrNotOneStatement)
}
