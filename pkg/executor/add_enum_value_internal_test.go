// White-box test for the bound on the read that classifies a rejected
// enum ADD VALUE.

package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/statement"
)

// The read that classifies a rejected ADD VALUE runs after the attempt has
// rolled back, outside the transaction whose timeouts bounded it. When no
// pooled session frees up, the read gives up at the budget's statement
// timeout with the deadline in its error, rather than holding the schema
// change call open. A type the read could not see is classified as neither
// typed code: the outcome is execution-failed.
func TestClassifyEnumValueAddRejectionIsBoundedByTheStatementTimeout(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testutil.StartPostgres(t))
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TYPE %s.mood AS ENUM ('happy', 'sad')`, schema))
	require.NoError(t, err)
	st, err := statement.ParseOne(fmt.Sprintf(`ALTER TYPE %s.mood ADD VALUE 'sad'`, schema))
	require.NoError(t, err)
	et, err := preflight.CheckEnumTarget(t.Context(), pool, st.EnumType())
	require.NoError(t, err)

	held, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	defer held.Release()

	rejection := &pgconn.PgError{Code: sqlstateDuplicateObject, Message: `enum label "sad" already exists`}
	budget := Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 200 * time.Millisecond}
	start := time.Now()
	err = classifyEnumValueAddRejection(t.Context(), pool, et, st, budget, rejection)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, errors.Is(err, rejection), "the server's rejection stays in the chain")
	assert.NotErrorIs(t, err, ErrEnumLabelExists, "an unread type is not classified")
	assert.Equal(t, CodeExecutionFailed, OutcomeCode(err), "an unreadable type is reported as execution-failed, not as either typed code")
	assert.Less(t, elapsed, 5*time.Second, "the read stops at the statement timeout")
}
