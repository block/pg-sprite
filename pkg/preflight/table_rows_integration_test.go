package preflight_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// TableHasRows reports whether the table, counting its partitions and
// inheritance children, holds a row, for names that need quoting too. A
// read that another session's ACCESS EXCLUSIVE lock blocks returns
// ErrTableRowsLocked at once instead of waiting for the lock.
func TestTableHasRows(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.empty (id bigint);
		CREATE TABLE %[1]s."Customer List" (id bigint);
		INSERT INTO %[1]s."Customer List" VALUES (1);
		CREATE TABLE %[1]s.events (id bigint) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (10);
		INSERT INTO %[1]s.events VALUES (1);
		CREATE TABLE %[1]s.parent (id bigint);
		CREATE TABLE %[1]s.child () INHERITS (%[1]s.parent);
		INSERT INTO %[1]s.child VALUES (1)`, schema))
	require.NoError(t, err)

	for table, want := range map[string]bool{"empty": false, "Customer List": true, "events": true, "parent": true} {
		rows, err := preflight.TableHasRows(t.Context(), pool, schema, table)
		require.NoError(t, err, table)
		assert.Equal(t, want, rows, table)
	}

	locker, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = locker.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = locker.Exec(t.Context(), fmt.Sprintf("LOCK TABLE %s.empty IN ACCESS EXCLUSIVE MODE", schema))
	require.NoError(t, err)
	_, err = preflight.TableHasRows(t.Context(), pool, schema, "empty")
	require.ErrorIs(t, err, preflight.ErrTableRowsLocked)
	assert.Contains(t, err.Error(), schema+".empty")
}
