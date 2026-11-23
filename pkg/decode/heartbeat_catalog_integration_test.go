package decode

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// The heartbeat paces itself by wal_sender_timeout as the replication
// session reads it, and that session takes its search_path from the
// caller's connection settings, which the engine does not repair. Here
// they list, ahead of pg_catalog, an = on text that answers false for
// every pair. The read spells its comparison OPERATOR(pg_catalog.=), so it
// still finds the server's 2s and the heartbeat runs at a quarter of it
// (CO-9). A bare = would match no setting, and the stream would refuse to
// open.
func TestHeartbeatReadsTheSenderTimeoutUnderALookalikeEquals(t *testing.T) {
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical", "wal_sender_timeout=2s")
	databaseURL := testutil.NewDatabase(t, serverURL)
	owner, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(owner.Close)
	lookalike := testutil.NewLookalikeTextMismatchPool(t, databaseURL, owner)
	path := lookalike.Config().ConnConfig.RuntimeParams["search_path"]

	conn, err := dbconn.ConnectReplication(t.Context(), dbconn.Config{
		URL: databaseURL,
		BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.RuntimeParams["search_path"] = path
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, conn.Close(context.WithoutCancel(t.Context())))
	})
	shown, err := conn.Exec(t.Context(), "SHOW search_path").ReadAll()
	require.NoError(t, err)
	require.Equal(t, path, string(shown[0].Rows[0][0]), "the replication session must run under the lookalike path")

	timeout, err := readSenderTimeout(t.Context(), conn)

	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, timeout)
	assert.Equal(t, 500*time.Millisecond, heartbeatInterval(timeout))
}
