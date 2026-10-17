package executor_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// heldProgressRead stands in for the reserved verdict session, running the
// tracker's own progress query on the real connection after the server has
// slept for two seconds, so the read is provably in flight — executing on
// the server, its client waiting for the answer — for a window a test can
// act in.
type heldProgressRead struct{ conn *pgxpool.Conn }

func (s heldProgressRead) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.conn.QueryRow(ctx, `
		SELECT progress.*
		  FROM (SELECT pg_catalog.pg_sleep(2)) AS held,
		       LATERAL (`+sql+`) AS progress`, args...)
}

// An observer whose context ends while its progress read is in flight must
// not take the verdict session with it: pgx closes a connection whose query's
// context ends mid-read, and the build's failure verdict — the invalid-index
// own-leftover classification that unblocks automated recovery — runs on this
// session after the build fails.
func TestProgressReadCancelledMidQueryLeavesTheVerdictSessionUsable(t *testing.T) {
	pool, _ := newPool(t)
	build, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(build.Release)
	verdict, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(verdict.Release)
	verdictPID := verdict.Conn().PgConn().PID()

	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationConcurrentIndex)
	tracker.SetConcurrentBuild(heldProgressRead{conn: verdict}, build.Conn().PgConn().PID())

	observer, giveUp := context.WithCancel(t.Context())
	defer giveUp()
	polled := make(chan error, 1)
	go func() {
		_, pollErr := tracker.Progress(observer)
		polled <- pollErr
	}()
	require.Eventually(t, func() bool {
		var held bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT FROM pg_catalog.pg_stat_activity
			                 WHERE pid = $1 AND state = 'active' AND wait_event = 'PgSleep')`,
			int32(verdictPID)).Scan(&held)
		return err == nil && held
	}, 30*time.Second, 20*time.Millisecond, "the progress read must be executing on the verdict session")

	giveUp()

	select {
	case pollErr := <-polled:
		require.NoError(t, pollErr, "the read runs to its own end; the observer giving up does not abort it")
	case <-time.After(30 * time.Second):
		t.Fatal("the progress read did not return")
	}
	var pid int32
	require.NoError(t, verdict.QueryRow(t.Context(), "SELECT pg_catalog.pg_backend_pid()").Scan(&pid),
		"the verdict session must still run queries after an observer gave up mid-read")
	assert.Equal(t, int32(verdictPID), pid, "the session must be the same backend, not a replacement")
}
