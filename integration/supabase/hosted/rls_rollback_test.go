package hosted_test

import (
	"context"
	"fmt"
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
	dropSQL string
	reached atomic.Bool
}

type rlsDropQueryKey struct{}

func (c *cancelAfterRLSDrop) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, rlsDropQueryKey{}, data.SQL == c.dropSQL)
}
func (c *cancelAfterRLSDrop) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	// Match the fully qualified live statement, not a DROP in a scratch schema.
	liveDrop, _ := ctx.Value(rlsDropQueryKey{}).(bool)
	if liveDrop && data.Err == nil && data.CommandTag.String() == "DROP POLICY" {
		c.reached.Store(true)
		c.cancel()
	}
}

func TestHostedRLSCancellationPreservesAccess(t *testing.T) {
	f := newFixture(t)
	f.seedUnpublished(t)
	pool, name := f.pool, f.name
	before, err := schemadiff.Introspect(t.Context(), pool, "public", name)
	require.NoError(t, err)
	f.assertAccess(t)
	desired, err := statement.ParseDesiredWithRowSecurity(fmt.Sprintf(`CREATE TABLE %s (
     id int PRIMARY KEY,
     owner_id uuid NOT NULL,
     body text NOT NULL
 );
 ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
 CREATE POLICY nobody ON %s
     FOR SELECT TO authenticated USING (false);`, pgx.Identifier{name}.Sanitize(), pgx.Identifier{name}.Sanitize(), pgx.Identifier{name}.Sanitize()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trace := &cancelAfterRLSDrop{
		cancel:  cancel,
		dropSQL: `DROP POLICY "own_rows" ON ` + f.table,
	}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = trace
	changing, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer changing.Close()
	_, err = executor.ExecuteRowSecurity(ctx, changing, "public", desired, executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second})
	require.True(t, trace.reached.Load(), "cancellation must happen after live DROP POLICY succeeds")
	require.ErrorIs(t, err, context.Canceled)
	after, err := schemadiff.Introspect(t.Context(), pool, "public", name)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	f.assertAccess(t)
	status, _, err := f.request(t.Context(), http.MethodPost, "/rest/v1/"+name, f.keys.Publishable, f.users[0].Token, map[string]any{"id": 90, "owner_id": f.users[1].ID, "body": "cross tenant"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, status)
	status, _, err = f.request(t.Context(), http.MethodPost, "/rest/v1/"+name, f.keys.Publishable, "", map[string]any{"id": 91, "owner_id": f.users[0].ID, "body": "anonymous"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, _, err = f.request(t.Context(), http.MethodPost, "/rest/v1/"+name, f.keys.Publishable, f.users[0].Token, map[string]any{"id": 3, "owner_id": f.users[0].ID, "body": "after rollback"})
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	f.assertRows(t, f.users[0].Token, 1, 3)
	f.assertRows(t, f.users[1].Token, 2)
	f.assertRows(t, "")
}
