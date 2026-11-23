package executor_test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
)

// inFlow is a stepClock hook condition: it is true while the build's own
// goroutine is inside the named function, which is where the tracker reads
// the clock for the step that function starts. A test that needs to act at
// one exact point of the build names that point instead of counting clock
// readings.
func inFlow(function string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, function) {
			return true
		}
		if !more {
			return false
		}
	}
}

// swapTable renames a table away and puts an unrelated table of the same
// name in its place, which is what a name-resolving statement cannot tell
// from the table it was proved against.
func (f partitionedFixture) swapTable(t *testing.T, table, replacement string) {
	t.Helper()
	f.exec(t, `ALTER TABLE %s.%s RENAME TO %s_old`, f.schema, table, table)
	f.exec(t, replacement, f.schema, table)
}

func (f partitionedFixture) runWithHook(t *testing.T, table, stmt string, hook func() bool) (executor.PartitionedIndexReport, error) {
	t.Helper()
	return executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, table), stmt, buildBudget,
		f.trackerWithHook(t, hook))
}

// Every statement the build runs names its table, the server resolves the
// name when the statement runs, and a name can move to another table between
// the proof and the statement. Each path that creates something checks what
// it created and refuses, with ST-7, when it is not on the table it proved.
func TestBuildPartitionedIndexRefusesATableSwappedAfterItWasProved(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		// The concurrent build cannot run in a transaction, so nothing stops
		// it creating the index on the table now holding the name. The build
		// finds that out from the index and refuses. It cannot prove that
		// index is its own, so it drops nothing and names it for an operator.
		t.Run("a concurrent build that landed on a replacement table is refused and leaves the index for an operator", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			fired := false
			hook := func() bool {
				if !inFlow("buildIndexConcurrentlyOn") {
					return false
				}
				for _, p := range partitions {
					f.swapTable(t, p, `CREATE TABLE %s.%s (id bigint, region text, total int, note text)`)
				}
				fired = true
				return true
			}

			_, err := f.runWithHook(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), hook)

			require.True(t, fired, "the swap must happen between the build's check and its statement")
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			assert.Contains(t, err.Error(), "left in place for an operator to inspect")
			landed := 0
			for _, p := range partitions {
				landed += f.indexCount(t, p)
				for name, r := range f.indexesOn(t, p) {
					assert.Empty(t, r.parent, "index %s on the replacement table %s must not be attached", name, p)
				}
				for name, r := range f.indexesOn(t, p+"_old") {
					assert.Empty(t, r.parent, "index %s on the original partition must not be attached", name)
				}
			}
			assert.Equal(t, 1, landed, "the first build is refused, and its index stays where it landed")
		})

		// The parent's ON ONLY index is created in a transaction that locks
		// the table by name and proves its identity first, so a table
		// swapped in before the statement refuses the run and the
		// replacement gets no index.
		t.Run("the parent index is not created on a replacement table", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			fired := false
			hook := func() bool {
				if !inFlow("createParentIndex") {
					return false
				}
				f.swapTable(t, "orders", `CREATE TABLE %s.%s (id bigint NOT NULL, region text NOT NULL, total int, note text) PARTITION BY LIST (region)`)
				fired = true
				return true
			}

			_, err := f.runWithHook(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), hook)

			require.True(t, fired)
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			assert.Zero(t, f.indexCount(t, "orders"), "the replacement table must get no index")
			assert.Zero(t, f.indexCount(t, "orders_old"), "the original table was not built either")
		})

		// A sub-partitioned partition gets an ON ONLY index of its own
		// through the same checked path.
		t.Run("a sub-partition index is not created on a replacement table", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `CREATE TABLE %[1]s.orders (id bigint NOT NULL, region text NOT NULL, total int, note text) PARTITION BY LIST (region)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.orders_eu PARTITION OF %[1]s.orders FOR VALUES IN ('eu') PARTITION BY LIST (note)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.orders_eu_a PARTITION OF %[1]s.orders_eu FOR VALUES IN ('a')`, f.schema)
			fired := false
			hook := func() bool {
				if !inFlow("ensureSubPartitionIndex") {
					return false
				}
				f.swapTable(t, "orders_eu", `CREATE TABLE %s.%s (id bigint, region text, total int, note text)`)
				fired = true
				return true
			}

			_, err := f.runWithHook(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), hook)

			require.True(t, fired)
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			assert.Zero(t, f.indexCount(t, "orders_eu"), "the replacement table must get no index")
			assert.Zero(t, f.indexCount(t, "orders_eu_a"), "nothing below the replaced node was built")
		})

		// A parent index resumed from an earlier run is checked against the
		// request once, and every later step relies on that check. An index
		// dropped and recreated under its name, even with the same
		// definition, is a different index and refuses the run.
		t.Run("a resumed parent index replaced after it was checked is refused", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders (total)`, f.schema)
			resumed := f.indexesOn(t, "orders")["idx_orders_total"]
			require.Equal(t, "I", resumed.kind)
			fired := false
			hook := func() bool {
				if !inFlow("checkResumedParentIndex") {
					return false
				}
				f.exec(t, `DROP INDEX %s.idx_orders_total`, f.schema)
				f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders (total)`, f.schema)
				fired = true
				return true
			}

			_, err := f.runWithHook(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), hook)

			require.True(t, fired)
			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			assert.Contains(t, err.Error(), "ST-7")
			assert.Contains(t, err.Error(), "replaced")
			for _, p := range []string{"orders_eu", "orders_us", "orders_ap"} {
				assert.Zero(t, f.indexCount(t, p), "partition %s must have no index", p)
			}
		})
	})
}

// The table lock is taken on a session of its own, dialed from the config,
// while every step runs on the pool. A config and a pool that point at
// different databases would hold the lock somewhere the build's tables are
// not, so the build confirms from the pool that the lock holder holds the
// lock in the pool's database, and refuses before any step otherwise.
func TestBuildPartitionedIndexRefusesALockHeldInAnotherDatabase(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)

		other := "pgsprite_other_" + strings.ToLower(f.schema)
		f.exec(t, `CREATE DATABASE %s`, pgx.Identifier{other}.Sanitize())
		t.Cleanup(func() {
			_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP DATABASE IF EXISTS `+pgx.Identifier{other}.Sanitize()+` WITH (FORCE)`)
			assert.NoError(t, err)
		})
		elsewhere := f.cfg
		elsewhere.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.Database = other
			return nil
		}

		_, err := executor.BuildPartitionedIndex(t.Context(), elsewhere, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
			fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget)

		require.ErrorIs(t, err, executor.ErrInvariantViolation)
		assert.Contains(t, err.Error(), "LK-1")
		f.requireNothingBuilt(t, "orders", partitions...)
	})
}

// A pool whose role cannot see when another role's session started cannot
// tell the lock session apart from one with the same PID on another server.
// The build refuses with its own reason, which names the role setup to fix,
// rather than report a lock held in another database, and creates nothing.
func TestBuildPartitionedIndexRefusesWhenThePoolCannotSeeTheLockSession(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)

		role := "pool_" + strings.ToLower(f.schema)
		f.exec(t, `CREATE ROLE %s LOGIN PASSWORD 'pool'`, pgx.Identifier{role}.Sanitize())
		t.Cleanup(func() {
			_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize())
			assert.NoError(t, err)
		})
		asRole := f.cfg
		asRole.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
			cc.User = role
			cc.Password = "pool"
			return nil
		}
		pool, err := dbconn.NewPool(t.Context(), asRole)
		require.NoError(t, err)
		t.Cleanup(pool.Close)

		_, err = executor.BuildPartitionedIndex(t.Context(), f.cfg, pool, mustPreflight(t, f.pool, f.schema, "orders"),
			fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget)

		require.ErrorIs(t, err, executor.ErrTableLockUnconfirmed)
		assert.NotErrorIs(t, err, executor.ErrInvariantViolation)
		assert.Equal(t, executor.CodeTableLockUnconfirmed, executor.OutcomeCode(err))
		f.requireNothingBuilt(t, "orders", partitions...)
	})
}
