package applier

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
)

// Every row write — the upsert, the present-columns UPDATE, and the
// fallback's insert — goes through execRow, so a column with no value is
// refused before its statement reaches the server (CO-8). The statement
// here would fail at the server with a division by zero if it ran, so the
// refusal being the typed invariant violation, and no server error at all,
// shows it never ran.
func TestExecRowRefusesAColumnWithNoValueBeforeTheStatementRuns(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) })

	_, err = execRow(t.Context(), tx, `SELECT 1 / 0`, 7,
		[]decode.Column{decode.PresentColumn("qty", "2"), decode.AbsentColumn("blob")})

	require.ErrorIs(t, err, ErrInvariantViolation)
	assert.ErrorContains(t, err, "column blob")
	var pgErr *pgconn.PgError
	assert.False(t, errors.As(err, &pgErr), "the statement never reached the server")
}
