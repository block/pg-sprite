package executor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// What the build refuses it refuses before it runs a statement, with a typed
// error, and creates nothing.
func TestBuildPartitionedIndexRefusesWhatItCannotBuild(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		t.Run("a statement that is not a build of one named index on the table", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			f.exec(t, `CREATE TABLE %s.plain (id int, total int)`, f.schema)
			s := f.schema
			cases := []struct {
				name  string
				table string
				sql   string
				want  error
			}{
				{"not an index", "orders", fmt.Sprintf("ALTER TABLE %s.orders ADD COLUMN extra int", s), executor.ErrNotCreateIndex},
				{"unnamed", "orders", fmt.Sprintf("CREATE INDEX ON %s.orders (total)", s), executor.ErrUnnamedIndex},
				{"if not exists", "orders", fmt.Sprintf("CREATE INDEX IF NOT EXISTS i ON %s.orders (total)", s), executor.ErrIfNotExistsUnsupported},
				// The leaf indexes are built from the definition the server
				// prints for the parent's index, and that never names a
				// tablespace, so the clause would reach the parent alone.
				{"tablespace", "orders", fmt.Sprintf("CREATE INDEX i ON %s.orders (total) TABLESPACE pg_default", s), executor.ErrPartitionedIndexTablespace},
				{"unqualified", "orders", "CREATE INDEX i ON orders (total)", executor.ErrUnqualifiedTable},
				{"two statements", "orders", fmt.Sprintf("CREATE INDEX i ON %[1]s.orders (total); CREATE INDEX j ON %[1]s.orders (note)", s), statement.ErrNotOneStatement},
				{"another table than the preflighted one", "orders", fmt.Sprintf("CREATE INDEX i ON %s.plain (total)", s), executor.ErrInvariantViolation},
				{"a table that is not partitioned", "plain", fmt.Sprintf("CREATE INDEX i ON %s.plain (total)", s), executor.ErrNotPartitionedTable},
			}
			for _, c := range cases {
				_, err := f.buildErr(t, c.table, c.sql)
				assert.ErrorIs(t, err, c.want, c.name)
			}
			f.requireNothingBuilt(t, "orders", partitions...)
			assert.Zero(t, f.indexCount(t, "plain"))
		})

		// A foreign table cannot carry an index, so the parent's index would
		// never become valid. The refusal comes before the parent's index
		// exists.
		t.Run("a tree with a foreign table partition", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			wrapper := "fdw_" + f.schema
			f.exec(t, `CREATE FOREIGN DATA WRAPPER %s`, wrapper)
			t.Cleanup(func() {
				_, err := f.pool.Exec(context.WithoutCancel(t.Context()), fmt.Sprintf(`DROP FOREIGN DATA WRAPPER IF EXISTS %s CASCADE`, wrapper))
				assert.NoError(t, err)
			})
			f.exec(t, `CREATE SERVER srv_%s FOREIGN DATA WRAPPER %s`, f.schema, wrapper)
			f.exec(t, `CREATE FOREIGN TABLE %[1]s.orders_fx PARTITION OF %[1]s.orders FOR VALUES IN ('fx') SERVER srv_%[1]s`, f.schema)

			_, err := f.buildErr(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))

			require.ErrorIs(t, err, executor.ErrForeignTablePartition)
			f.requireNothingBuilt(t, "orders", partitions...)
		})

		// A DETACH PARTITION CONCURRENTLY that was interrupted leaves its
		// partition in the tree, and the server leaves that partition out
		// when it attaches an index, so the parent's index could never
		// become valid. The refusal names the partition and comes before the
		// parent's index exists. The test marks the detach pending in the
		// catalog, which is the state an interrupted detach leaves.
		t.Run("a tree with a partition whose detach did not finish", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			f.exec(t, `UPDATE pg_catalog.pg_inherits SET inhdetachpending = true WHERE inhrelid = '%s.orders_us'::regclass`, f.schema)

			_, err := f.buildErr(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))

			require.ErrorIs(t, err, executor.ErrPartitionDetachPending)
			assert.Equal(t, executor.CodePartitionDetachPending, executor.OutcomeCode(err))
			assert.Contains(t, err.Error(), f.schema+".orders_us")
			f.requireNothingBuilt(t, "orders", partitions...)
		})

		// PostgreSQL checks a unique index against each partitioned table's
		// partition key only when that table gets its index, so a key that
		// misses a level below the parent would fail part-way through the
		// tree. The build checks every level first and creates nothing.
		t.Run("a unique index that misses a partition key below the parent", func(t *testing.T) {
			for name, c := range map[string]struct {
				subKey string
				// covering is a unique key that contains every level's
				// partition key; empty when no key can.
				covering string
			}{
				"a column":      {subKey: "LIST (region)", covering: "(id, ts, region)"},
				"an expression": {subKey: "LIST (lower(region))"},
			} {
				t.Run(name, func(t *testing.T) {
					f := newFixture(t)
					f.exec(t, `CREATE TABLE %s.events (
	id bigint NOT NULL,
	region text NOT NULL,
	ts date NOT NULL
) PARTITION BY RANGE (ts)`, f.schema)
					f.exec(t, `CREATE TABLE %[1]s.events_2023 PARTITION OF %[1]s.events
	FOR VALUES FROM ('2023-01-01') TO ('2024-01-01')`, f.schema)
					f.exec(t, `CREATE TABLE %[1]s.events_2024 PARTITION OF %[1]s.events
	FOR VALUES FROM ('2024-01-01') TO ('2025-01-01')
	PARTITION BY `+c.subKey, f.schema)
					f.exec(t, `CREATE TABLE %[1]s.events_2024_eu PARTITION OF %[1]s.events_2024
	FOR VALUES IN ('eu')`, f.schema)
					partitions := []string{"events_2023", "events_2024", "events_2024_eu"}

					_, err := f.buildErr(t, "events", fmt.Sprintf("CREATE UNIQUE INDEX idx_events_id ON %s.events (id, ts)", f.schema))

					require.ErrorIs(t, err, executor.ErrUniqueIndexPartitionKey)
					f.requireNothingBuilt(t, "events", partitions...)
					if c.covering == "" {
						return
					}
					f.build(t, "events", fmt.Sprintf("CREATE UNIQUE INDEX idx_events_id ON %s.events %s", f.schema, c.covering))
					f.requireCovered(t, "events", "idx_events_id", "events_2023", "events_2024")
					f.requireCovered(t, "events_2024", f.derived(t, "idx_events_id", "events_2024"), "events_2024_eu")
				})
			}
		})

		// Each of these holds a name the build needs. None is renamed,
		// dropped, or attached; the build says so and stops.
		t.Run("a name held by something that is not the requested index", func(t *testing.T) {
			cases := []struct {
				name  string
				setup func(t *testing.T, f partitionedFixture, derivedEU string)
				// held names the index the build must leave as it found it;
				// empty when the occupant is not an index.
				heldTable string
				held      func(derivedEU string) string
			}{
				{
					name: "a table holds the parent index's name",
					setup: func(t *testing.T, f partitionedFixture, _ string) {
						f.exec(t, `CREATE TABLE %s.idx_orders_total (x int)`, f.schema)
					},
				},
				{
					name: "a partitioned index of another definition holds the parent index's name",
					setup: func(t *testing.T, f partitionedFixture, _ string) {
						f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders (note)`, f.schema)
					},
					heldTable: "orders",
					held:      func(string) string { return "idx_orders_total" },
				},
				{
					name: "a partitioned index with another predicate holds the parent index's name",
					setup: func(t *testing.T, f partitionedFixture, _ string) {
						f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders (total) WHERE total > 0`, f.schema)
					},
					heldTable: "orders",
					held:      func(string) string { return "idx_orders_total" },
				},
				{
					name: "an index on another table holds the parent index's name",
					setup: func(t *testing.T, f partitionedFixture, _ string) {
						f.exec(t, `CREATE TABLE %s.other (total int)`, f.schema)
						f.exec(t, `CREATE INDEX idx_orders_total ON %s.other (total)`, f.schema)
					},
					heldTable: "other",
					held:      func(string) string { return "idx_orders_total" },
				},
				{
					name: "an index of another definition holds a partition's derived name",
					setup: func(t *testing.T, f partitionedFixture, derivedEU string) {
						f.exec(t, `CREATE INDEX %s ON %s.orders_eu (note)`, derivedEU, f.schema)
					},
					heldTable: "orders_eu",
					held:      func(derivedEU string) string { return derivedEU },
				},
				{
					name: "an index on another partition holds a partition's derived name",
					setup: func(t *testing.T, f partitionedFixture, derivedEU string) {
						f.exec(t, `CREATE INDEX %s ON %s.orders_us (total)`, derivedEU, f.schema)
					},
					heldTable: "orders_us",
					held:      func(derivedEU string) string { return derivedEU },
				},
				{
					name: "a sequence holds a partition's derived name",
					setup: func(t *testing.T, f partitionedFixture, derivedEU string) {
						f.exec(t, `CREATE SEQUENCE %s.%s`, f.schema, derivedEU)
					},
				},
				{
					name: "an index attached to another partitioned index holds a partition's derived name",
					setup: func(t *testing.T, f partitionedFixture, derivedEU string) {
						f.exec(t, `CREATE INDEX idx_other ON ONLY %s.orders (total)`, f.schema)
						f.exec(t, `CREATE INDEX %s ON %s.orders_eu (total)`, derivedEU, f.schema)
						f.exec(t, `ALTER INDEX %s.idx_other ATTACH PARTITION %s.%s`, f.schema, f.schema, derivedEU)
					},
					heldTable: "orders_eu",
					held:      func(derivedEU string) string { return derivedEU },
				},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					f := newFixture(t)
					f.ordersByRegion(t)
					derivedEU := f.derived(t, "idx_orders_total", "orders_eu")
					c.setup(t, f, derivedEU)
					var before indexRow
					if c.held != nil {
						before = f.indexesOn(t, c.heldTable)[c.held(derivedEU)]
						require.NotEmpty(t, before.name)
					}

					_, err := f.buildErr(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))

					require.ErrorIs(t, err, executor.ErrPartitionedIndexNameTaken)
					assert.Zero(t, f.indexCount(t, "orders"), "the parent must have no index")
					assert.Empty(t, f.attachedTo(t, "orders_eu", "idx_orders_total"), "orders_eu has no index attached to idx_orders_total")
					if c.held != nil {
						assert.Equal(t, before, f.indexesOn(t, c.heldTable)[c.held(derivedEU)], "the occupant must be left as it was")
					}
				})
			}
		})

		// The table's advisory lock is the build's exclusion: a build that
		// finds it held says who holds it and creates nothing.
		t.Run("a table lock held by another session", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			held, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, "orders")
			require.NoError(t, err)
			stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)

			_, err = f.buildErr(t, "orders", stmt)

			var lockErr *dbconn.TableLockHeldError
			require.ErrorAs(t, err, &lockErr)
			assert.Equal(t, f.schema, lockErr.Schema)
			assert.Equal(t, "orders", lockErr.Table)
			f.requireNothingBuilt(t, "orders", partitions...)

			require.NoError(t, held.Release(t.Context()))
			f.build(t, "orders", stmt)
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		})

		t.Run("a pool that cannot hold the build's two sessions", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			small, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: f.cfg.URL, MaxConns: 1})
			require.NoError(t, err)
			t.Cleanup(small.Close)

			_, err = executor.BuildPartitionedIndex(t.Context(), f.cfg, small, mustPreflight(t, f.pool, f.schema, "orders"),
				fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget)

			require.ErrorIs(t, err, executor.ErrPoolTooSmall)
			f.requireNothingBuilt(t, "orders", partitions...)
		})

		t.Run("a budget that bounds nothing", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)

			_, err := executor.BuildPartitionedIndex(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
				fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), executor.ConcurrentBudget{})

			require.ErrorIs(t, err, executor.ErrUnboundedBudget)
			f.requireNothingBuilt(t, "orders", partitions...)
		})

		// A caller-owned budget leaves each partition's build to the caller's
		// cancellable context.
		t.Run("a caller-owned budget needs a cancellable context", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)
			caller := executor.ConcurrentBudget{CallerOwned: true}

			_, err := executor.BuildPartitionedIndex(context.WithoutCancel(t.Context()), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"), stmt, caller)
			require.ErrorIs(t, err, executor.ErrCallerOwnedNeedsCancellableContext)
			f.requireNothingBuilt(t, "orders", partitions...)

			rep, err := executor.BuildPartitionedIndex(ctx, f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"), stmt, caller)
			require.NoError(t, err)
			assert.Len(t, rep.Worked, 3)
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		})
	})
}

// The progress variant reports one step for the parent's index and one for
// each partition, every statement it runs, and a finished or failed phase that
// matches the outcome.
func TestBuildPartitionedIndexWithProgress(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		t.Run("a finished build ends on its last step", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)
			tracker, err := progress.NewTracker(progress.WallClock{})
			require.NoError(t, err)

			rep, err := executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
				fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget, tracker)

			require.NoError(t, err)
			require.Len(t, rep.Worked, 3)
			snapshot, err := tracker.Progress(t.Context())
			require.NoError(t, err)
			assert.Equal(t, progress.PhaseFinished, snapshot.Phase)
			assert.Equal(t, 4, snapshot.TotalSteps, "the parent's index and three partitions")
			assert.Equal(t, 4, snapshot.Step)
			assert.NotEmpty(t, snapshot.Detail.Statement)
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		})

		t.Run("a sub-partitioned tree counts every partition that needs an index", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, `CREATE TABLE %s.events (id bigint NOT NULL, region text NOT NULL) PARTITION BY LIST (region)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events FOR VALUES IN ('eu') PARTITION BY HASH (id)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_eu_0 PARTITION OF %[1]s.events_eu FOR VALUES WITH (MODULUS 2, REMAINDER 0)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_eu_1 PARTITION OF %[1]s.events_eu FOR VALUES WITH (MODULUS 2, REMAINDER 1)`, f.schema)
			f.exec(t, `CREATE TABLE %[1]s.events_us PARTITION OF %[1]s.events FOR VALUES IN ('us')`, f.schema)
			tracker, err := progress.NewTracker(progress.WallClock{})
			require.NoError(t, err)

			_, err = executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "events"),
				fmt.Sprintf("CREATE INDEX idx_events_id ON %s.events (id)", f.schema), buildBudget, tracker)

			require.NoError(t, err)
			snapshot, err := tracker.Progress(t.Context())
			require.NoError(t, err)
			assert.Equal(t, progress.PhaseFinished, snapshot.Phase)
			assert.Equal(t, 5, snapshot.TotalSteps, "the parent's index and four partitions")
			assert.LessOrEqual(t, snapshot.Step, snapshot.TotalSteps)
		})

		t.Run("a failed build ends failed", func(t *testing.T) {
			f := newFixture(t)
			f.ordersByRegion(t)
			f.exec(t, `CREATE TABLE %s.idx_orders_total (x int)`, f.schema)
			tracker, err := progress.NewTracker(progress.WallClock{})
			require.NoError(t, err)

			_, err = executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
				fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget, tracker)

			require.ErrorIs(t, err, executor.ErrPartitionedIndexNameTaken)
			snapshot, err := tracker.Progress(t.Context())
			require.NoError(t, err)
			assert.Equal(t, progress.PhaseFailed, snapshot.Phase)
		})

		t.Run("a missing tracker is refused", func(t *testing.T) {
			f := newFixture(t)
			partitions := f.ordersByRegion(t)

			_, err := executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "orders"),
				fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema), buildBudget, nil)

			require.ErrorIs(t, err, executor.ErrInvariantViolation)
			f.requireNothingBuilt(t, "orders", partitions...)
		})
	})
}
