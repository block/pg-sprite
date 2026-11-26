// White-box tests for the partitioned column drop's check: the lock it
// holds on the whole tree is visible only from inside the check's
// transaction, and the refusal's list of objects is rendered by a helper.

package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// The check locks the target and every partition below it, a
// sub-partition's own partitions included, before it reads the catalog, so
// no partition gains an object between the read and the drop. After the
// check passes for
//
//	ALTER TABLE events DROP COLUMN legacy
//
// the check's transaction holds ACCESS EXCLUSIVE on all four tables.
func TestRefusePartitionedColumnDropHoldsTheWholeTreeLocked(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.events (
			id bigint NOT NULL,
			created_at timestamptz NOT NULL,
			legacy text
		) PARTITION BY RANGE (created_at);
		CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
		CREATE TABLE %[1]s.events_2025 PARTITION OF %[1]s.events
			FOR VALUES FROM ('2025-01-01') TO ('2026-01-01')
			PARTITION BY LIST (id);
		CREATE TABLE %[1]s.events_2025_a PARTITION OF %[1]s.events_2025
			FOR VALUES IN (1)`, schema))
	require.NoError(t, err)

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back the check's transaction: %v", err)
		}
	})
	st, err := statement.ParseOne(fmt.Sprintf("ALTER TABLE %s.events DROP COLUMN legacy", schema))
	require.NoError(t, err)
	require.NoError(t, refusePartitionedColumnDrop(t.Context(), tx, st,
		schemadiff.DroppedObject{Kind: schemadiff.DroppedColumn, Name: "legacy"}, schema+".events"))

	rows, err := tx.Query(t.Context(), `
		SELECT c.relname
		  FROM pg_catalog.pg_locks l
		  JOIN pg_catalog.pg_class c ON c.oid = l.relation
		 WHERE l.pid = pg_catalog.pg_backend_pid()
		   AND l.mode = 'AccessExclusiveLock'
		   AND c.relnamespace = $1::regnamespace
		   AND c.relkind IN ('r', 'p')
		 ORDER BY 1`, schema)
	require.NoError(t, err)
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"events", "events_2024", "events_2025", "events_2025_a"}, locked)
}

// A refusal names at most a handful of objects and counts the rest, so its
// text stays short on a table with hundreds of partitions.
func TestBoundedNamesCountsWhatItDoesNotName(t *testing.T) {
	assert.Equal(t, "a; b", boundedNames([]string{"a", "b"}))
	assert.Equal(t, "a; b; c; d; e", boundedNames([]string{"a", "b", "c", "d", "e"}))
	assert.Equal(t, "a; b; c; d; e; and 2 more", boundedNames([]string{"a", "b", "c", "d", "e", "f", "g"}))
}
