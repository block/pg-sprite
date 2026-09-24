package supabase_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cancel only after the server has successfully dropped a live policy. This
// exercises partial work inside a real transaction, not a pre-execution refusal.
type cancelAfterRLSDrop struct {
	cancel  context.CancelFunc
	reached atomic.Bool
}

func (c *cancelAfterRLSDrop) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (c *cancelAfterRLSDrop) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	// The command tag distinguishes the successful live DROP from scratch work.
	if data.Err == nil && data.CommandTag.String() == "DROP POLICY" {
		c.reached.Store(true)
		c.cancel()
	}
}

func TestAtomicRLSAPICancellationPreservesAccess(t *testing.T) {
	const name = "pgsprite_rls_api_rollback"
	pool := rlsAPITable(t, name)
	before, err := schemadiff.Introspect(t.Context(), pool, "public", name)
	require.NoError(t, err)
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	assertRLSRead(t, name, 0)
	assertRLSInsertDenied(t, name, 1, 90, 2)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE pgsprite_rls_api_rollback (
     id int PRIMARY KEY,
     owner_id uuid NOT NULL,
     body text NOT NULL
 );
 ALTER TABLE pgsprite_rls_api_rollback ENABLE ROW LEVEL SECURITY;
 CREATE POLICY nobody ON pgsprite_rls_api_rollback
     FOR SELECT TO authenticated USING (false);`)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trace := &cancelAfterRLSDrop{cancel: cancel}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = trace
	changing, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer changing.Close()
	report, err := executor.ExecuteRowSecurity(ctx, changing, "public", desired, executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, trace.reached.Load(), "cancellation must happen after live DROP POLICY succeeds")
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, "public", name)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assertRLSRead(t, name, 1, 1)
	assertRLSRead(t, name, 2, 2)
	assertRLSRead(t, name, 0)
	assertRLSInsertDenied(t, name, 1, 91, 2)
	assertRLSInsertDenied(t, name, 0, 92, 1)
	// The original write policy survives too, not just its read behavior.
	status, body := rlsRequest(t, http.MethodPost, name+"?select=id", 1, `{"id":3,"owner_id":"00000000-0000-0000-0000-000000000001","body":"after rollback"}`)
	assertRLSRows(t, status, body, http.StatusCreated, 3)
}
