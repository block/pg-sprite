package executor_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
)

// A unique build over duplicates fails after it creates its catalog entry.
// The run stops with the server's own error and leaves a valid prefix: the
// parent's index, the partitions built before, and the invalid debris. After
// the data is fixed the rerun proves the debris abandoned, removes it, and
// finishes.
func TestBuildPartitionedIndexRecoversDebrisOfAFailedBuild(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)
		// Only the eu partition holds duplicates of the unique key.
		f.exec(t, `DELETE FROM %s.orders WHERE region <> 'eu'`, f.schema)
		f.exec(t, `INSERT INTO %s.orders SELECT g, 'us', g, 'u' FROM generate_series(1, 10) g`, f.schema)
		f.exec(t, `INSERT INTO %s.orders SELECT g, 'ap', g, 'a' FROM generate_series(1, 10) g`, f.schema)
		stmt := fmt.Sprintf("CREATE UNIQUE INDEX idx_orders_uq ON %s.orders (region, total)", f.schema)

		_, err := f.buildErr(t, "orders", stmt)

		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, sqlstateUniqueViolation, pgErr.Code)
		debrisName := f.derived(t, "idx_orders_uq", "orders_eu")
		debris, ok := f.indexesOn(t, "orders_eu")[debrisName]
		require.True(t, ok, "the failed build leaves its invalid index")
		assert.False(t, debris.valid)
		assert.False(t, f.indexesOn(t, "orders")["idx_orders_uq"].valid)

		f.exec(t, `DELETE FROM %[1]s.orders_eu WHERE id NOT IN (SELECT min(id) FROM %[1]s.orders_eu GROUP BY total)`, f.schema)

		rep := f.build(t, "orders", stmt)

		assert.False(t, rep.ParentCreated)
		recovered := 0
		for _, w := range rep.Worked {
			if w.RecoveredDebris {
				recovered++
				assert.Equal(t, "orders_eu", w.Table)
				assert.Equal(t, executor.PartitionIndexBuilt, w.Action)
				assert.Equal(t, debrisName, w.Index)
			}
		}
		assert.Equal(t, 1, recovered, "only the partition with debris is recovered")
		f.requireCovered(t, "orders", "idx_orders_uq", partitions...)
	})
}

// An invalid index under a derived name that a live backend is still
// building is not debris. The build refuses it with the builder's PID and
// touches nothing; once that build finishes, the rerun reuses its index.
func TestBuildPartitionedIndexRefusesAPartitionIndexAnotherBackendIsBuilding(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)
		stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)
		derived := f.derived(t, "idx_orders_total", "orders_eu")

		// A snapshot held open makes any concurrent build wait for it, which
		// keeps the other backend's build in flight for as long as the test
		// needs.
		holder, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		require.NoError(t, err)
		var one int
		require.NoError(t, holder.QueryRow(t.Context(), `SELECT 1`).Scan(&one))
		released := false
		release := func() {
			if !released {
				released = true
				require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context())))
			}
		}
		t.Cleanup(release)

		manual, err := pgx.Connect(t.Context(), f.cfg.URL)
		require.NoError(t, err)
		t.Cleanup(func() { _ = manual.Close(context.WithoutCancel(t.Context())) })
		manualDone := make(chan error, 1)
		go func() {
			_, buildErr := manual.Exec(t.Context(), fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s.orders_eu (total)", derived, f.schema))
			manualDone <- buildErr
		}()
		require.Eventually(t, func() bool {
			row, ok := f.indexesOn(t, "orders_eu")[derived]
			return ok && !row.valid
		}, 30*time.Second, 20*time.Millisecond, "the other backend's build must reach its catalog entry")

		_, err = f.buildErr(t, "orders", stmt)

		var invalid *executor.InvalidIndexError
		require.ErrorAs(t, err, &invalid)
		require.ErrorIs(t, invalid.Cleanup, executor.ErrInvalidIndexBuildInFlight)
		assert.NotZero(t, invalid.BuilderPID)
		assert.Equal(t, derived, invalid.Index)
		_, stillThere := f.indexesOn(t, "orders_eu")[derived]
		assert.True(t, stillThere, "a live build's index must not be dropped")

		release()
		require.NoError(t, <-manualDone)

		rep := f.build(t, "orders", stmt)

		assert.False(t, rep.ParentCreated)
		reused := 0
		for _, w := range rep.Worked {
			if w.Action == executor.PartitionIndexReused {
				reused++
				assert.Equal(t, "orders_eu", w.Table)
				assert.Equal(t, derived, w.Index)
			}
		}
		assert.Equal(t, 1, reused)
		f.requireCovered(t, "orders", "idx_orders_total", partitions...)
	})
}

// The probe that checks a resumed parent index against the request takes a
// name no relation holds, and never touches an existing object. An index
// that is literally named like the usual probe is another table's index: the
// build leaves it alone, resumes, and a rerun does the same.
func TestBuildPartitionedIndexProbeNeverTouchesAnExistingRelation(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)
		f.exec(t, `CREATE TABLE %s.bystander (id bigint, total int)`, f.schema)
		probeName := fmt.Sprintf("pgsprite_probe_%d", f.tableOID(t, "orders"))
		f.exec(t, `CREATE INDEX %s ON %s.bystander (total)`, probeName, f.schema)
		bystanderIndex := indexOID(t, f.pool, f.schema, probeName)
		f.exec(t, `CREATE INDEX idx_orders_total ON ONLY %s.orders (total)`, f.schema)
		stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)

		for run := 1; run <= 2; run++ {
			rep, err := f.buildErr(t, "orders", stmt)

			require.NoError(t, err, "run %d", run)
			assert.False(t, rep.ParentCreated, "run %d resumes the parent index", run)
			f.requireCovered(t, "orders", "idx_orders_total", partitions...)
			assert.Equal(t, bystanderIndex, indexOID(t, f.pool, f.schema, probeName),
				"run %d must leave the bystander's index as it was", run)
			assert.Equal(t, 1, f.indexCount(t, "bystander"))
		}
		for name := range f.indexesOn(t, "orders") {
			assert.False(t, strings.HasPrefix(name, "pgsprite_probe_"), "the probe %s must not outlive its transaction", name)
		}
	})
}

// A recovery that renamed an abandoned index to its quarantine name and
// stopped before the drop leaves the derived name free and the debris in
// place. The build finds the debris under the partition's own identity,
// removes it, and builds the partition's index.
func TestBuildPartitionedIndexRecoversQuarantineDebrisUnderAFreeName(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		partitions := f.ordersByRegion(t)
		derived := f.derived(t, "idx_orders_total", "orders_eu")
		require.False(t, f.relationExists(t, derived), "the derived name must be free")

		f.exec(t, `CREATE INDEX debris_source ON %s.orders_eu (note)`, f.schema)
		debris := quarantinedName(indexOID(t, f.pool, f.schema, "debris_source"))
		f.exec(t, `ALTER INDEX %s.debris_source RENAME TO %s`, f.schema, debris)
		f.exec(t, `UPDATE pg_catalog.pg_index SET indisvalid = false WHERE indexrelid = '%s.%s'::regclass`, f.schema, debris)
		require.Equal(t, []string{debris}, quarantinedIndexes(t, f.pool, f.schema))

		rep := f.build(t, "orders", fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema))

		f.requireCovered(t, "orders", "idx_orders_total", partitions...)
		assert.Empty(t, quarantinedIndexes(t, f.pool, f.schema), "the quarantined debris must be dropped")
		var recovered []string
		for _, w := range rep.Worked {
			if w.RecoveredDebris {
				recovered = append(recovered, w.Table)
			}
		}
		assert.Equal(t, []string{"orders_eu"}, recovered)
	})
}

// A partition whose attached index is invalid keeps the parent's index
// invalid, and nothing in the build may replace it. The error names the
// partition and the index so an operator knows which one to repair.
func TestBuildPartitionedIndexNamesAnInvalidAttachedIndex(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		f.ordersByRegion(t)
		stmt := fmt.Sprintf("CREATE INDEX idx_orders_total ON %s.orders (total)", f.schema)
		f.build(t, "orders", stmt)
		broken := f.derived(t, "idx_orders_total", "orders_us")
		f.exec(t, `UPDATE pg_catalog.pg_index SET indisvalid = false WHERE indexrelid = '%s.%s'::regclass`, f.schema, broken)

		_, err := f.buildErr(t, "orders", stmt)

		require.ErrorIs(t, err, executor.ErrInvariantViolation)
		assert.Contains(t, err.Error(), f.schema+".orders_us")
		assert.Contains(t, err.Error(), f.schema+"."+broken)
		assert.Contains(t, err.Error(), "invalid")
	})
}

// The partitions can live in other schemas and every name can need quoting:
// the parent in one schema, its partitions in another, with spaces and
// capitals in the table, column, and index names.
func TestBuildPartitionedIndexQuotesEveryName(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		other := "Part " + strings.ToUpper(f.schema)
		quotedOther := pgx.Identifier{other}.Sanitize()
		f.exec(t, `CREATE SCHEMA %s`, quotedOther)
		t.Cleanup(func() {
			_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP SCHEMA `+quotedOther+` CASCADE`)
			assert.NoError(t, err)
		})
		f.exec(t, `CREATE TABLE %s."Mixed Orders" (id bigint NOT NULL, "Region" text NOT NULL, total int) PARTITION BY LIST ("Region")`, f.schema)
		f.exec(t, `CREATE TABLE %s."Orders EU" PARTITION OF %s."Mixed Orders" FOR VALUES IN ('eu')`, quotedOther, f.schema)
		f.exec(t, `CREATE TABLE %s."Orders US" PARTITION OF %s."Mixed Orders" FOR VALUES IN ('us')`, f.schema, f.schema)
		f.exec(t, `INSERT INTO %s."Mixed Orders" SELECT g, CASE g %% 2 WHEN 0 THEN 'eu' ELSE 'us' END, g FROM generate_series(1, 100) g`, f.schema)

		rep := f.build(t, "Mixed Orders", fmt.Sprintf(`CREATE INDEX "Idx Total" ON %s."Mixed Orders" ("Region", total)`, f.schema))

		assert.Equal(t, "Idx Total", rep.Index)
		require.Len(t, rep.Worked, 2)
		schemas := map[string]string{}
		for _, w := range rep.Worked {
			schemas[w.Table] = w.Schema
			assert.Equal(t, executor.PartitionIndexBuilt, w.Action)
			attached := f.indexesOnIn(t, w.Schema, w.Table)[w.Index]
			assert.Equal(t, "Idx Total", attached.parent, "partition %s.%s", w.Schema, w.Table)
			assert.True(t, attached.valid)
		}
		assert.Equal(t, map[string]string{"Orders EU": other, "Orders US": f.schema}, schemas)
		assert.True(t, f.indexesOn(t, "Mixed Orders")["Idx Total"].valid)
	})
}

// A sub-partitioned partition whose index an earlier run created but did not
// attach is resumed, and it still gets a progress step of its own: the run
// ends on its last step, one for the parent's index and one per partition.
func TestBuildPartitionedIndexGivesAResumedSubPartitionItsOwnStep(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		f.exec(t, `CREATE TABLE %s.events (id bigint NOT NULL, region text NOT NULL) PARTITION BY LIST (region)`, f.schema)
		f.exec(t, `CREATE TABLE %[1]s.events_eu PARTITION OF %[1]s.events FOR VALUES IN ('eu') PARTITION BY HASH (id)`, f.schema)
		f.exec(t, `CREATE TABLE %[1]s.events_eu_0 PARTITION OF %[1]s.events_eu FOR VALUES WITH (MODULUS 2, REMAINDER 0)`, f.schema)
		f.exec(t, `CREATE TABLE %[1]s.events_eu_1 PARTITION OF %[1]s.events_eu FOR VALUES WITH (MODULUS 2, REMAINDER 1)`, f.schema)
		f.exec(t, `CREATE TABLE %[1]s.events_us PARTITION OF %[1]s.events FOR VALUES IN ('us')`, f.schema)
		f.exec(t, `CREATE INDEX idx_events_id ON ONLY %s.events USING btree (id)`, f.schema)
		resumed := f.derived(t, "idx_events_id", "events_eu")
		f.exec(t, `CREATE INDEX %s ON ONLY %s.events_eu USING btree (id)`, resumed, f.schema)
		tracker, err := progress.NewTracker(progress.WallClock{})
		require.NoError(t, err)

		rep, err := executor.BuildPartitionedIndexWithProgress(t.Context(), f.cfg, f.pool, mustPreflight(t, f.pool, f.schema, "events"),
			fmt.Sprintf("CREATE INDEX idx_events_id ON %s.events (id)", f.schema), buildBudget, tracker)

		require.NoError(t, err)
		assert.Contains(t, workedTables(rep), "events_eu", "the resumed partition is reported")
		snapshot, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 5, snapshot.TotalSteps, "the parent's index and four partitions")
		assert.Equal(t, snapshot.TotalSteps, snapshot.Step, "every partition, the resumed one too, takes its own step")
		f.requireCovered(t, "events", "idx_events_id", "events_eu", "events_us")
	})
}

// An index under the derived name that backs a constraint is not the build's
// to attach, even when its definition matches: attaching it under a parent
// index that is not a constraint's would change what it is. The build refuses
// the name and leaves the constraint as it stands.
func TestBuildPartitionedIndexRefusesAConstraintIndexUnderTheDerivedName(t *testing.T) {
	withPartitionedServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFixture) {
		f := newFixture(t)
		f.ordersByRegion(t)
		taken := f.derived(t, "idx_orders_uq", "orders_eu")
		f.exec(t, `ALTER TABLE %s.orders_eu ADD CONSTRAINT %s UNIQUE (total, region, id)`, f.schema, taken)

		_, err := f.buildErr(t, "orders", fmt.Sprintf("CREATE UNIQUE INDEX idx_orders_uq ON %s.orders (total, region, id)", f.schema))

		require.ErrorIs(t, err, executor.ErrPartitionedIndexNameTaken)
		assert.Contains(t, err.Error(), "backs a constraint")
		idx := f.indexesOn(t, "orders_eu")[taken]
		assert.True(t, idx.valid, "the constraint's index is left as it stands")
	})
}
