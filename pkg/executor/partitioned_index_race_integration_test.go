package executor_test

import (
	"context"
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

// buildOutcome is what a build running in the background returned.
type buildOutcome struct {
	rep executor.PartitionedIndexReport
	err error
}

// stallFirstPartitionBuild starts the build of stmt in the background and
// returns once the first partition's index build is in flight. A held
// snapshot keeps that build waiting for old snapshots until release is
// called, which is idempotent and also runs when the test ends.
func (f partitionedFixture) stallFirstPartitionBuild(t *testing.T, stmt string) (release func(), done <-chan buildOutcome) {
	t.Helper()
	holder, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	var one int
	require.NoError(t, holder.QueryRow(t.Context(), `SELECT 1`).Scan(&one))
	released := false
	release = func() {
		if !released {
			released = true
			require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
		}
	}
	t.Cleanup(release)

	result := make(chan buildOutcome, 1)
	go func() {
		rep, err := f.buildErr(t, "orders", stmt)
		result <- buildOutcome{rep, err}
	}()
	firstIndex := f.derived(t, "idx_orders_total", "orders_eu")
	require.Eventually(t, func() bool {
		row, ok := f.indexesOn(t, "orders_eu")[firstIndex]
		return ok && !row.valid
	}, 30*time.Second, 20*time.Millisecond, "the first partition's build must be in flight")
	return release, result
}

func TestBuildPartitionedIndexTreeChangesDuringARun(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		// A partition created after the tree was listed and before the
		// parent's index exists has no index, and no pass has it in its
		// list. The next pass lists the tree again, builds it, and a third
		// confirms nothing is left.
		t.Run("a partition created before the parent index exists is picked up on the next pass", func(t *testing.T) {
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

		// A partition renamed away and replaced by an unrelated table of the
		// same name after the tree was listed is not the partition the build
		// listed. The build refuses before it builds, and the unrelated
		// table gets no index.
		t.Run("a partition replaced by an unrelated table of the same name is not indexed", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)

			// The held snapshot leaves the build holding a listing the test
			// can make stale.
			release, done := f.stallFirstPartitionBuild(t, stmt)

			f.exec(t, `ALTER TABLE %s.orders_us RENAME TO orders_us_old`, f.schema)
			f.exec(t, `CREATE TABLE %s.orders_us (id bigint, region text, total int, note text)`, f.schema)
			release()
			got := <-done

			require.ErrorIs(t, got.err, executor.ErrInvariantViolation)
			assert.Contains(t, got.err.Error(), "ST-7")
			assert.Contains(t, got.err.Error(), f.schema+".orders_us")
			assert.Zero(t, f.indexCount(t, "orders_us"), "the unrelated table must get no index")
			assert.Zero(t, f.indexCount(t, "orders_us_old"), "the detached-by-name partition was not built either")
		})

		// An index renamed away and replaced under its name after the build
		// chose it is not the index the build chose. The attach refuses it
		// and attaches nothing.
		t.Run("an index swapped under its name before the attach is refused", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			f.exec(t, `CREATE INDEX orders_eu_total ON %s.orders_eu (total)`, f.schema)
			hook := func() bool {
				if !f.relationExists(t, "idx_orders_total") {
					return false
				}
				f.exec(t, `ALTER INDEX %s.orders_eu_total RENAME TO orders_eu_total_old`, f.schema)
				f.exec(t, `CREATE INDEX orders_eu_total ON %s.orders_eu (total)`, f.schema)
				return true
			}
			tracker := f.trackerWithHook(t, hook)

			_, err := executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
				fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget, tracker)

			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			for name, r := range f.indexesOn(t, "orders_eu") {
				assert.Empty(t, r.parent, "index %s must not be attached", name)
			}
		})
	})
}

// A tree with no partitions when it is listed can gain its first one before
// the parent's index exists. The first pass has nothing to build, so the
// build still lists the tree again: the second pass builds the new
// partition's index and a third confirms nothing is left.
func TestBuildPartitionedIndexPicksUpAFirstPartitionCreatedDuringARun(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		f.exec(t, `CREATE TABLE %[1]s.orders (id bigint NOT NULL, region text NOT NULL, total int, note text) PARTITION BY LIST (region)`, f.schema)
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
		assert.Equal(t, 1, rep.Partitions)
		assert.Equal(t, 3, rep.Passes)
		assert.ElementsMatch(t, []string{"orders_late"}, workedTables(rep))
		f.requireCovered(t, "orders", "idx_orders_total", "orders_late")
	})
}
