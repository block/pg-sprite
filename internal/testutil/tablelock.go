package testutil

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// TableLock takes the table's change lock for the duration of the test and
// registers its release. Every mutating executor entry point requires the
// proof (LK-1), so a test that executes anything holds one — the same way
// the front doors do.
func TableLock(t *testing.T, pool *pgxpool.Pool, schema, table string) *dbconn.TableLock {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), pool, schema, table, dbconn.TableLockOptions{})
	require.NoError(t, err, "take the table change lock for %s.%s", schema, table)
	t.Cleanup(func() {
		// t.Context is cancelled by cleanup time; strip the cancellation so
		// the release still reaches the server.
		if err := lock.Release(context.WithoutCancel(t.Context())); err != nil {
			t.Logf("release the table change lock for %s.%s: %v", schema, table, err)
		}
	})
	return lock
}
