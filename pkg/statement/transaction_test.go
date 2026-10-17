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
		want statement.TransactionControl
	}{
		{"BEGIN", statement.TransactionBegin},
		{"BEGIN ISOLATION LEVEL SERIALIZABLE", statement.TransactionBegin},
		{"START TRANSACTION", statement.TransactionBegin},
		{"COMMIT", statement.TransactionEnd},
		{"END", statement.TransactionEnd},
		{"ROLLBACK", statement.TransactionEnd},
		{"ABORT", statement.TransactionEnd},
		{"PREPARE TRANSACTION 'gid'", statement.TransactionEnd},
		{"COMMIT AND CHAIN", statement.TransactionNone},
		{"ROLLBACK AND CHAIN", statement.TransactionNone},
		{"SAVEPOINT s", statement.TransactionNone},
		{"RELEASE SAVEPOINT s", statement.TransactionNone},
		{"ROLLBACK TO SAVEPOINT s", statement.TransactionNone},
		{"COMMIT PREPARED 'gid'", statement.TransactionNone},
		{"CREATE INDEX CONCURRENTLY i ON t (c)", statement.TransactionNone},
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
