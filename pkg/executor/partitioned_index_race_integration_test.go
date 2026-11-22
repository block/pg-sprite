package executor_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
)

// stepClock is a progress clock that lets a test act at a step boundary of
// the build: the tracker reads the clock when a step starts, which is after
// the build has decided what to do and before it does it. The hook runs on
// each reading until it reports that it acted, and the build sees nothing
// but a clock.
type stepClock struct {
	mu   sync.Mutex
	done bool
	hook func() bool
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.done = c.hook()
	}
	return time.Now()
}

func (f partitionedFixture) trackerWithHook(t *testing.T, hook func() bool) *progress.Tracker {
	t.Helper()
	tracker, err := progress.NewTracker(&stepClock{hook: hook})
	require.NoError(t, err)
	return tracker
}

// tableLockHeld reports whether a session holds the build's table lock on
// the table. The test database is shared with other packages' tests, which
// hold advisory locks of their own while this one runs, so the lock is
// identified by its key: pg-sprite's keyspace and the table's name.
func (f partitionedFixture) tableLockHeld(t *testing.T, table string) bool {
	t.Helper()
	var held bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			 WHERE locktype = 'advisory' AND granted
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			   AND classid::bigint = $1
			   AND objid::bigint = (hashtext(quote_ident($2) || '.' || quote_ident($3)))::bigint & 4294967295)`,
		int64(uint32(dbconn.TableLockClassID)), f.schema, table).Scan(&held))
	return held
}

func (f partitionedFixture) relationExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT to_regclass($1) IS NOT NULL`, pgx.Identifier{f.schema, name}.Sanitize()).Scan(&exists))
	return exists
}

// A partition created after the tree was listed and before the parent's index
// exists has no index, and no pass has it in its list. The next pass lists the
// tree again, builds it, and a third confirms nothing is left.
func TestBuildPartitionedIndexPicksUpAPartitionCreatedDuringARun(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)
		hook := func() bool {
			if !f.tableLockHeld(t, "orders") {
				return false
			}
			require.False(t, f.relationExists(t, "idx_orders_total"), "the hook must act before the parent's index exists")
			f.exec(t, `CREATE TABLE %[1]s.orders_late PARTITION OF %[1]s.orders FOR VALUES IN ('late')`, f.schema)
			return true
		}
		tracker := f.trackerWithHook(t, hook)

		rep, err := executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
			fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget, tracker)

		require.NoError(t, err)
		assert.Equal(t, 4, rep.Partitions)
		assert.Equal(t, 3, rep.Passes)
		assert.ElementsMatch(t, append(partitions, "orders_late"), workedTables(rep))
		f.requireCovered(t, "orders", "idx_orders_total", append(partitions, "orders_late")...)
	})
}
