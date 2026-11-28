package executor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
)

// partitionedFKFixture is fkFixture plus the connection target the build's
// table lock dials.
type partitionedFKFixture struct {
	fkFixture
	cfg dbconn.Config
}

// withPartitionedFKServer runs the subtests against one server, each in a
// schema of its own. The build refuses PostgreSQL 18 and later, where the
// ordinary NOT VALID route works on a partitioned table, so a test that
// expects the build to run is skipped there;
// TestAddPartitionedForeignKeyRefusesWhereNotValidWorks covers those majors.
func withPartitionedFKServer(t *testing.T, run func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture)) {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	if major >= partitionedFKNotValidMajor {
		t.Skipf("PostgreSQL %d accepts NOT VALID on a partitioned table's foreign key, and the build refuses it", major)
	}
	run(t, func(t *testing.T) partitionedFKFixture {
		t.Helper()
		return partitionedFKFixture{fkFixture: fkFixture{pool: pool, schema: testutil.NewSchema(t, pool)}, cfg: cfg}
	})
}

const addEventsAccountFK = `ALTER TABLE %[1]s.events ADD CONSTRAINT events_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`

// add runs the build of sql on the fixture's events table, every %[1]s in
// sql replaced by the fixture's schema.
func (f partitionedFKFixture) add(t *testing.T, sql string) (PartitionedForeignKeyReport, error) {
	t.Helper()
	return f.addAt(t, sql, RunStep{})
}

func (f partitionedFKFixture) addAt(t *testing.T, sql string, at RunStep) (PartitionedForeignKeyReport, error) {
	t.Helper()
	pt, err := preflight.CheckTable(t.Context(), f.pool, f.schema, "events", 1<<30)
	require.NoError(t, err)
	return AddPartitionedForeignKeyAsStep(t.Context(), f.cfg, f.pool, pt, fmt.Sprintf(sql, f.schema), leafFKBudget, at)
}

// requireAdopted asserts the parent has the validated foreign key the report
// names and that every leaf the report lists has exactly one foreign key:
// the one its leaf step left, under the OID the leaf step reported, now
// adopted by the parent's. An unchanged OID is the proof the parent's ADD
// FOREIGN KEY created and validated nothing.
func (f partitionedFKFixture) requireAdopted(t *testing.T, rep PartitionedForeignKeyReport) {
	t.Helper()
	parent, ok := f.foreignKeys(t, "events")[rep.Constraint]
	require.True(t, ok, "the parent has foreign key %s", rep.Constraint)
	assert.Equal(t, rep.ConstraintOID, parent.oid)
	assert.True(t, parent.validated)
	require.Len(t, rep.Leaves, rep.Partitions, "the report lists every partition")
	for _, leaf := range rep.Leaves {
		got := f.foreignKeys(t, leaf.Table)
		require.Len(t, got, 1, "partition %s has only the adopted foreign key", leaf.Table)
		assert.Equal(t, fkRow{
			oid:       leaf.ConstraintOID,
			def:       fmt.Sprintf("FOREIGN KEY (account_id) REFERENCES %s.accounts(id)", f.schema),
			validated: true,
			parentOID: parent.oid,
		}, got[leaf.Constraint], "partition %s", leaf.Table)
	}
}

// partitionFKActions maps each reported leaf to its action.
func partitionFKActions(rep PartitionedForeignKeyReport) map[string]PartitionForeignKeyAction {
	out := map[string]PartitionForeignKeyAction{}
	for _, l := range rep.Leaves {
		out[l.Table] = l.Action
	}
	return out
}

// allFKs reads every foreign key on the parent and its three leaves.
func (f partitionedFKFixture) allFKs(t *testing.T) map[string]map[string]fkRow {
	t.Helper()
	out := map[string]map[string]fkRow{}
	for _, table := range []string{"events", "events_2026_01", "events_2026_02", "events_2026_03"} {
		out[table] = f.foreignKeys(t, table)
	}
	return out
}

// The foreign key is added online: each leaf gets it NOT VALID and then
// validated, and the parent's ADD FOREIGN KEY then adopts them all in one
// brief transaction. A writer under a one-second lock_timeout keeps
// committing for the whole run; it waits only for the brief steps, which
// change the catalog and scan nothing.
func TestAddPartitionedForeignKeyOnline(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `
			INSERT INTO %[1]s.events (id, account_id, created_at)
			SELECT g, 1 + mod(g, 100), '2026-01-01'::timestamptz + mod(g, 89) * interval '1 day'
			  FROM generate_series(10001, 300000) g`)

		writer, err := f.pool.Acquire(t.Context())
		require.NoError(t, err)
		defer writer.Release()
		_, err = writer.Exec(t.Context(), `SET lock_timeout = '1s'`)
		require.NoError(t, err)
		ctx, stop := context.WithCancel(t.Context())
		var (
			wg      sync.WaitGroup
			commits atomic.Int64
			failure atomic.Pointer[error]
		)
		insert := fmt.Sprintf(`
			INSERT INTO %s.events (id, account_id, created_at)
			VALUES ($1, 7, '2026-01-15'), ($1, 8, '2026-02-15'), ($1, 9, '2026-03-15')`, f.schema)
		wg.Go(func() {
			for id := int64(1_000_000); ctx.Err() == nil; id++ {
				if _, err := writer.Exec(ctx, insert, id); err != nil {
					if ctx.Err() == nil {
						failure.Store(&err)
					}
					return
				}
				commits.Add(1)
			}
		})

		rep, err := f.add(t, addEventsAccountFK)
		during := commits.Load()
		stop()
		wg.Wait()

		require.NoError(t, err)
		if p := failure.Load(); p != nil {
			require.NoError(t, *p, "the writer must not fail while the build runs")
		}
		assert.Positive(t, during, "the writer commits while the build runs")
		assert.True(t, rep.ParentAdded)
		assert.Equal(t, 1, rep.Passes)
		assert.Equal(t, 3, rep.Partitions)
		assert.Equal(t, map[string]PartitionForeignKeyAction{
			"events_2026_01": PartitionForeignKeyAdded,
			"events_2026_02": PartitionForeignKeyAdded,
			"events_2026_03": PartitionForeignKeyAdded,
		}, partitionFKActions(rep))
		assert.NotEmpty(t, rep.ServerVersion)
		assert.Positive(t, rep.Duration)
		f.requireAdopted(t, rep)
	})
}

// ON DELETE SET NULL (account_id) clears only account_id when an account is
// deleted, and keeps region. Every partition's foreign key carries the same
// list, so the parent's adopts them, including the partition whose columns
// are numbered differently because it was created on its own and attached.
// PostgreSQL 14 has no column list, so the build refuses there before it
// adds anything.
func TestAddPartitionedForeignKeyWithAnOnDeleteColumnList(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.accounts (
			    id     bigint NOT NULL,
			    region text   NOT NULL,
			    PRIMARY KEY (id, region)
			);
			INSERT INTO %[1]s.accounts VALUES (1, 'eu'), (2, 'us');
			CREATE TABLE %[1]s.events (
			    id         bigint      NOT NULL,
			    account_id bigint,
			    region     text,
			    created_at timestamptz NOT NULL
			) PARTITION BY RANGE (created_at);
			CREATE TABLE %[1]s.events_2026_01 PARTITION OF %[1]s.events
			    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
			CREATE TABLE %[1]s.events_2026_02 (
			    created_at timestamptz NOT NULL,
			    dropped    int,
			    region     text,
			    id         bigint      NOT NULL,
			    account_id bigint
			);
			ALTER TABLE %[1]s.events_2026_02 DROP COLUMN dropped;
			ALTER TABLE %[1]s.events ATTACH PARTITION %[1]s.events_2026_02
			    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
			INSERT INTO %[1]s.events (id, account_id, region, created_at) VALUES
			    (1, 1, 'eu', '2026-01-10'),
			    (2, 1, 'eu', '2026-02-10'),
			    (3, 2, 'us', '2026-02-11')`)
		const addWithList = `
			ALTER TABLE %[1]s.events
			    ADD CONSTRAINT events_account_fk
			    FOREIGN KEY (account_id, region) REFERENCES %[1]s.accounts (id, region)
			    ON DELETE SET NULL (account_id)`
		major, err := dbconn.ServerMajor(t.Context(), f.pool)
		require.NoError(t, err)
		if major < deleteSetColumnsMajor {
			_, err := f.add(t, addWithList)
			require.ErrorIs(t, err, ErrPartitionedForeignKeyUnsupportedShape)
			for _, table := range []string{"events", "events_2026_01", "events_2026_02"} {
				assert.Empty(t, f.foreignKeys(t, table), "table %s", table)
			}
			return
		}

		rep, err := f.add(t, addWithList)
		require.NoError(t, err)
		parent := f.foreignKeys(t, "events")["events_account_fk"]
		require.Len(t, rep.Leaves, 2)
		want := fmt.Sprintf("FOREIGN KEY (account_id, region) REFERENCES %s.accounts(id, region) ON DELETE SET NULL (account_id)", f.schema)
		assert.Equal(t, want, parent.def)
		for _, leaf := range rep.Leaves {
			assert.Equal(t, PartitionForeignKeyAdded, leaf.Action)
			assert.Equal(t, fkRow{oid: leaf.ConstraintOID, def: want, validated: true, parentOID: parent.oid},
				f.foreignKeys(t, leaf.Table)[leaf.Constraint], "partition %s", leaf.Table)
		}

		f.exec(t, `DELETE FROM %[1]s.accounts WHERE id = 1`)
		rows, err := f.pool.Query(t.Context(), fmt.Sprintf(`SELECT id, account_id, region FROM %s.events ORDER BY id`, f.schema))
		require.NoError(t, err)
		type event struct {
			ID        int64
			AccountID *int64
			Region    string
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[event])
		require.NoError(t, err)
		two := int64(2)
		assert.Equal(t, []event{{1, nil, "eu"}, {2, nil, "eu"}, {3, &two, "us"}}, got,
			"deleting account 1 cleared account_id on both partitions and kept region")
	})
}

// A rerun after the parent's foreign key was added, the state a run that
// stopped after its last COMMIT leaves, finds it and reports it done. It
// changes nothing and does not mistake the adopted leaf foreign keys for
// conflicts.
func TestAddPartitionedForeignKeyRerunAfterSuccessIsDone(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		first, err := f.add(t, addEventsAccountFK)
		require.NoError(t, err)
		before := f.allFKs(t)

		again, err := f.add(t, addEventsAccountFK)

		require.NoError(t, err)
		assert.False(t, again.ParentAdded)
		assert.Equal(t, first.ConstraintOID, again.ConstraintOID)
		assert.Empty(t, again.Leaves)
		assert.Zero(t, again.Passes)
		assert.Equal(t, 3, again.Partitions)
		assert.Equal(t, before, f.allFKs(t))
	})
}

// A run resumes from what an interrupted run or an operator left on the
// leaves, and the parent adopts each under the OID and name it has: a leaf
// an earlier run finished is found, an operator's NOT VALID foreign key is
// validated, and an operator's validated one is found.
func TestAddPartitionedForeignKeyResumes(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		done, err := f.ensure(t, f.accountFK(), "events_2026_01")
		require.NoError(t, err)
		f.exec(t, `
			ALTER TABLE %[1]s.events_2026_02
			    ADD CONSTRAINT feb_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
			    NOT VALID`)
		f.exec(t, `
			ALTER TABLE %[1]s.events_2026_03
			    ADD CONSTRAINT mar_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)

		rep, err := f.add(t, addEventsAccountFK)

		require.NoError(t, err)
		assert.Equal(t, map[string]PartitionForeignKeyAction{
			"events_2026_01": PartitionForeignKeyFound,
			"events_2026_02": PartitionForeignKeyValidated,
			"events_2026_03": PartitionForeignKeyFound,
		}, partitionFKActions(rep))
		assert.Equal(t, done.constraintOID, rep.Leaves[0].ConstraintOID, "the finished leaf's foreign key is adopted, not replaced")
		assert.Equal(t, "feb_account_fk", rep.Leaves[1].Constraint)
		assert.Equal(t, "mar_account_fk", rep.Leaves[2].Constraint)
		f.requireAdopted(t, rep)
	})
}

// fkHookClock is a progress clock that lets a test act at a step boundary:
// the tracker reads it when a statement starts, after the build decided what
// to run and before it runs it. The hook runs on each reading until it
// reports that it acted.
type fkHookClock struct {
	mu   sync.Mutex
	done bool
	hook func() bool
}

func (c *fkHookClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.done = c.hook()
	}
	return time.Now()
}

// atParentStep returns a run step whose tracker runs act once, when the
// parent's step is about to start: every listed leaf then has its validated
// foreign key.
func (f partitionedFKFixture) atParentStep(t *testing.T, act func()) RunStep {
	t.Helper()
	tracker, err := progress.NewTracker(&fkHookClock{hook: func() bool {
		for _, leaf := range []string{"events_2026_01", "events_2026_02", "events_2026_03"} {
			fks := f.foreignKeys(t, leaf)
			if len(fks) == 0 {
				return false
			}
			for _, k := range fks {
				if !k.validated {
					return false
				}
			}
		}
		act()
		return true
	}})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationBrief)
	return RunStep{Tracker: tracker, Step: 1}
}

// A partition created after the leaves were listed has no foreign key, so
// the parent's step adds nothing; the next pass gives it the foreign key,
// and the parent's then adopts all four.
func TestAddPartitionedForeignKeySettlesAPartitionCreatedMidRun(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		at := f.atParentStep(t, func() {
			f.exec(t, `
				CREATE TABLE %[1]s.events_2026_04 PARTITION OF %[1]s.events
				    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01')`)
			f.exec(t, `INSERT INTO %[1]s.events (id, account_id, created_at) VALUES (1, 1, '2026-04-02'), (2, 2, '2026-04-03')`)
		})

		rep, err := f.addAt(t, addEventsAccountFK, at)

		require.NoError(t, err)
		assert.Equal(t, 2, rep.Passes)
		assert.Equal(t, 4, rep.Partitions)
		assert.Equal(t, map[string]PartitionForeignKeyAction{
			"events_2026_01": PartitionForeignKeyAdded,
			"events_2026_02": PartitionForeignKeyAdded,
			"events_2026_03": PartitionForeignKeyAdded,
			"events_2026_04": PartitionForeignKeyAdded,
		}, partitionFKActions(rep))
		f.requireAdopted(t, rep)
	})
}

// A partition created before every parent's step, each time every partition
// already has its validated foreign key, keeps the parent's step from ever
// adding: the build runs out of passes with this build's own retryable
// error, keeps every partition's foreign key, and a rerun once partitions
// stop appearing adopts them all.
func TestAddPartitionedForeignKeyRunsOutOfPasses(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		created := 0
		tracker, err := progress.NewTracker(&fkHookClock{hook: func() bool {
			if !f.everyLeafHasValidatedFK(t) {
				return false
			}
			created++
			f.exec(t, fmt.Sprintf(`
				CREATE TABLE %%[1]s.events_2026_%02[1]d PARTITION OF %%[1]s.events
				    FOR VALUES FROM ('2026-%02[1]d-01') TO ('2026-%02[2]d-01')`, 3+created, 4+created))
			return false
		}})
		require.NoError(t, err)
		tracker.Start(1, progress.OperationBrief)

		rep, err := f.addAt(t, addEventsAccountFK, RunStep{Tracker: tracker, Step: 1})

		require.ErrorIs(t, err, ErrPartitionedForeignKeyUnsettled)
		assert.Equal(t, CodePartitionedForeignKeyUnsettled, OutcomeCode(err))
		assert.False(t, OutcomeCode(err).Permanent())
		assert.Equal(t, maxPartitionedForeignKeyPasses, rep.Passes)
		assert.Equal(t, maxPartitionedForeignKeyPasses, created, "one partition appeared before each parent's step")
		assert.Empty(t, f.foreignKeys(t, "events"), "the parent has no foreign key")

		again, err := f.add(t, addEventsAccountFK)

		require.NoError(t, err)
		assert.Equal(t, 3+maxPartitionedForeignKeyPasses, again.Partitions)
		f.requireAdopted(t, again)
	})
}

// everyLeafHasValidatedFK reports whether every partition of events has at
// least one foreign key and all of them are validated.
func (f partitionedFKFixture) everyLeafHasValidatedFK(t *testing.T) bool {
	t.Helper()
	tree, err := listPartitionTree(t.Context(), f.pool, f.oid(t, "events"))
	require.NoError(t, err)
	for _, n := range tree {
		fks := f.foreignKeys(t, n.name)
		if len(fks) == 0 {
			return false
		}
		for _, k := range fks {
			if !k.validated {
				return false
			}
		}
	}
	return true
}

// A rerun finds the parent's foreign key done only when every partition
// carries one the parent's adopted; a parent foreign key no partition's
// foreign key belongs to is refused, not reported done.
func TestRequireAdoptedFKEverywhereRefusesAPartitionTheParentsDidNotAdopt(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		_, err := f.add(t, addEventsAccountFK)
		require.NoError(t, err)
		target := indexTarget{schema: f.schema, table: "events", tableOID: f.oid(t, "events")}
		parent := f.foreignKeys(t, "events")["events_account_fk"]
		january := f.foreignKeys(t, "events_2026_01")

		partitions, err := requireAdoptedFKEverywhere(t.Context(), f.pool, target, catalogFK{oid: parent.oid, name: "events_account_fk"})

		require.NoError(t, err)
		assert.Equal(t, 3, partitions)
		for name, k := range january {
			// A leaf's own constraint OID adopted nothing on the partitions.
			_, err = requireAdoptedFKEverywhere(t.Context(), f.pool, target, catalogFK{oid: k.oid, name: name})
			require.ErrorIs(t, err, ErrInvariantViolation)
		}
	})
}

// Every refusal is typed, and each comes before the build creates anything.
func TestAddPartitionedForeignKeyRefusals(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		refuse := func(t *testing.T, f partitionedFKFixture, sql string, want error, code Code) {
			t.Helper()
			before := f.allFKs(t)

			_, err := f.add(t, sql)

			require.ErrorIs(t, err, want)
			assert.Equal(t, code, OutcomeCode(err))
			assert.Equal(t, before, f.allFKs(t), "no foreign key was added, validated, or adopted")
		}

		t.Run("a NOT VALID statement", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			refuse(t, f, addEventsAccountFK+` NOT VALID`,
				ErrPartitionedForeignKeyUnsupportedShape, CodePartitionedForeignKeyUnsupportedShape)
		})

		t.Run("another constraint under the parent's name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_account_fk CHECK (id > 0)`)
			refuse(t, f, addEventsAccountFK, ErrPartitionedForeignKeyParentConflict, CodePartitionedForeignKeyParentConflict)
		})

		// The parent would end up with two identical foreign keys, and the
		// leaves' are already adopted by the first.
		t.Run("the parent already has the foreign key under another name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events
				    ADD CONSTRAINT events_account_other_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
			refuse(t, f, addEventsAccountFK, ErrPartitionedForeignKeyParentConflict, CodePartitionedForeignKeyParentConflict)
		})

		t.Run("a sub-partitioned partition", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events_2026_04 PARTITION OF %[1]s.events
				    FOR VALUES FROM ('2026-04-01') TO ('2026-05-01')
				    PARTITION BY LIST (account_id)`)
			refuse(t, f, addEventsAccountFK, ErrPartitionedForeignKeySubPartitioned, CodePartitionedForeignKeySubPartitioned)
		})

		// The parent's statement would clone the foreign key onto every
		// partition of the referenced table, which the build neither
		// proves nor reads back.
		t.Run("a partitioned referenced table", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				CREATE TABLE %[1]s.regions (
				    id bigint PRIMARY KEY
				) PARTITION BY RANGE (id)`)
			refuse(t, f, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_region_fk FOREIGN KEY (account_id) REFERENCES %[1]s.regions (id)`,
				ErrPartitionedForeignKeyReferencedTable, CodePartitionedForeignKeyReferencedTable)
		})

		// A partition of the target is a plain table the resolve takes, but
		// the parent's foreign key would then reference a part of itself,
		// so the tree read refuses it before any leaf step.
		t.Run("a referenced table that is a partition of the target", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `CREATE UNIQUE INDEX events_2026_01_id ON %[1]s.events_2026_01 (id)`)
			refuse(t, f, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_self_fk FOREIGN KEY (account_id) REFERENCES %[1]s.events_2026_01 (id)`,
				ErrPartitionedForeignKeyReferencedTable, CodePartitionedForeignKeyReferencedTable)
		})

		// The first leaf in the tree carries two foreign keys of the
		// requested definition, so the run stops there before it touches
		// any leaf.
		t.Run("a leaf with two matching foreign keys", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_account_fk_2 FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
			refuse(t, f, addEventsAccountFK, ErrPartitionedForeignKeyConflict, CodePartitionedForeignKeyConflict)
		})

		// PostgreSQL adopts a partition's foreign key whatever its ON
		// DELETE column list says, so the parent's statement could adopt
		// this one instead of the leaf step's, and with it a delete rule
		// that differs from the parent's. The run stops before it touches
		// any leaf. A column list exists from PostgreSQL 15.
		t.Run("a leaf foreign key that differs only in its ON DELETE column list", func(t *testing.T) {
			f := newFixture(t)
			if major, err := dbconn.ServerMajor(t.Context(), f.pool); assert.NoError(t, err) && major < 15 {
				t.Skipf("PostgreSQL %d has no ON DELETE column list", major)
			}
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT a_jan_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
				    ON DELETE SET NULL (account_id)`)
			refuse(t, f, addEventsAccountFK+` ON DELETE SET NULL`, ErrPartitionedForeignKeyConflict, CodePartitionedForeignKeyConflict)
		})
	})
}

// On PostgreSQL 18 and later, the build refuses and points at the ordinary
// NOT VALID then VALIDATE route, which works there on a partitioned table;
// nothing is created.
func TestAddPartitionedForeignKeyRefusesWhereNotValidWorks(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	major, err := dbconn.ServerMajor(t.Context(), pool)
	require.NoError(t, err)
	if major < partitionedFKNotValidMajor {
		t.Skipf("PostgreSQL %d refuses NOT VALID on a partitioned table's foreign key, and the build runs", major)
	}
	f := partitionedFKFixture{fkFixture: fkFixture{pool: pool, schema: testutil.NewSchema(t, pool)}, cfg: cfg}
	f.events(t)

	_, err = f.add(t, addEventsAccountFK)

	require.ErrorIs(t, err, ErrPartitionedForeignKeyNotValidSupported)
	assert.Equal(t, CodePartitionedForeignKeyNotValidSupported, OutcomeCode(err))
	for table, fks := range f.allFKs(t) {
		assert.Empty(t, fks, "table %s", table)
	}
}

// recordLeafFKs runs the leaf step on every leaf and returns what the
// parent's step proves: the target, the resolved foreign key, and each
// leaf's recorded foreign key.
func (f partitionedFKFixture) recordLeafFKs(t *testing.T) (indexTarget, resolvedFK, map[uint32]leafFKResult) {
	t.Helper()
	fk := f.resolve(t, f.accountFK())
	leaves := map[uint32]leafFKResult{}
	for _, name := range []string{"events_2026_01", "events_2026_02", "events_2026_03"} {
		leaf := f.leaf(t, name)
		res, err := ensureLeafForeignKey(t.Context(), f.pool, fk, leaf, leafFKBudget, nil)
		require.NoError(t, err)
		leaves[leaf.oid] = res
	}
	return indexTarget{tableOID: f.oid(t, "events"), schema: f.schema, table: "events"}, fk, leaves
}

// begin opens a transaction under the bounds a brief step runs with.
func (f partitionedFKFixture) begin(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			assert.NoError(t, err)
		}
	})
	_, err = tx.Exec(t.Context(), `SET LOCAL lock_timeout = '1s'`)
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), dbconn.LocalSearchPath(catalogReadPath()...))
	require.NoError(t, err)
	return tx
}

// replaceFebruaryFK swaps the February leaf's foreign key for an identical
// one under another name, the change an operator could make between the leaf
// step and the parent's step.
func (f partitionedFKFixture) replaceFebruaryFK(t *testing.T, constraint string) {
	t.Helper()
	f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{constraint}.Sanitize())
	f.exec(t, `
		ALTER TABLE %[1]s.events_2026_02
		    ADD CONSTRAINT feb_other_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
}

// The parent's step adds the parent's foreign key only over exactly the leaf
// foreign keys the leaf steps recorded, and reads back that it adopted them.
// A leaf changed since its leaf step is refused, before the ADD, as
// ErrPartitionedForeignKeyLeafChanged, which a rerun gets past; an ADD that
// did anything but adopt the recorded foreign keys rolls back with an
// invariant violation.
func TestAddPartitionedForeignKeyParentStepFailsClosed(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		// A leaf whose foreign key was replaced after its leaf step still has
		// one the parent could adopt, but not the one the run validated, so
		// the proof under the tree's lock refuses before the ADD. The read-back
		// after the ADD would refuse too, as an invariant violation; the typed
		// refusal shows the proof, not the read-back, stopped the step.
		t.Run("the proof refuses a leaf foreign key replaced after its leaf step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, fk, leaves := f.recordLeafFKs(t)
			feb := f.leaf(t, "events_2026_02")
			f.replaceFebruaryFK(t, leaves[feb.oid].constraint)

			err := proveLeafFKRecorded(t.Context(), f.pool, fk, feb, leaves[feb.oid])

			require.ErrorIs(t, err, ErrPartitionedForeignKeyLeafChanged)
			_, err = adoptLeafFKs(t.Context(), f.begin(t), target, fk, leaves)
			require.ErrorIs(t, err, ErrPartitionedForeignKeyLeafChanged)
			assert.NotErrorIs(t, err, ErrInvariantViolation)
		})

		// A second foreign key of the definition beside the recorded one would
		// leave the parent's statement to choose which one to adopt.
		t.Run("the proof refuses a second matching foreign key added after the leaf step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			_, fk, leaves := f.recordLeafFKs(t)
			feb := f.leaf(t, "events_2026_02")
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT feb_other_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)

			err := proveLeafFKRecorded(t.Context(), f.pool, fk, feb, leaves[feb.oid])

			require.ErrorIs(t, err, ErrPartitionedForeignKeyLeafChanged)
		})

		// The same change made while a whole build runs, between its leaf
		// steps and its parent's step: the build fails closed and the parent
		// gets no foreign key.
		t.Run("a build whose leaf foreign key is replaced before the parent's step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			at := f.atParentStep(t, func() {
				recorded := f.foreignKeys(t, "events_2026_02")
				require.Len(t, recorded, 1)
				for name := range recorded {
					f.replaceFebruaryFK(t, name)
				}
			})

			_, err := f.addAt(t, addEventsAccountFK, at)

			require.ErrorIs(t, err, ErrPartitionedForeignKeyLeafChanged)
			assert.Equal(t, CodePartitionedForeignKeyLeafChanged, OutcomeCode(err))
			assert.False(t, OutcomeCode(err).Permanent(), "a rerun gets past the change")
			assert.Empty(t, f.foreignKeys(t, "events"), "the parent has no foreign key")
			for _, leaf := range []string{"events_2026_01", "events_2026_02", "events_2026_03"} {
				for name, k := range f.foreignKeys(t, leaf) {
					assert.Zero(t, k.parentOID, "foreign key %s on %s is adopted by nothing", name, leaf)
				}
			}

			// The rerun takes the operator's foreign key on February as that
			// leaf's and adopts every leaf.
			rep, err := f.add(t, addEventsAccountFK)

			require.NoError(t, err)
			assert.Equal(t, PartitionForeignKeyFound, partitionFKActions(rep)["events_2026_02"])
			f.requireAdopted(t, rep)
		})

		// A foreign key that differs from the recorded one only in its ON
		// DELETE column list, added between the leaf steps and the parent's,
		// sorts first by name, so the parent's ADD would adopt it. The proof
		// refuses before the ADD. A column list exists from PostgreSQL 15.
		t.Run("a build given a column-list foreign key before the parent's step", func(t *testing.T) {
			f := newFixture(t)
			if major, err := dbconn.ServerMajor(t.Context(), f.pool); assert.NoError(t, err) && major < 15 {
				t.Skipf("PostgreSQL %d has no ON DELETE column list", major)
			}
			f.events(t)
			at := f.atParentStep(t, func() {
				f.exec(t, `
					ALTER TABLE %[1]s.events_2026_02
					    ADD CONSTRAINT a_feb_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
					    ON DELETE SET NULL (account_id)`)
			})

			_, err := f.addAt(t, addEventsAccountFK+` ON DELETE SET NULL`, at)

			require.ErrorIs(t, err, ErrPartitionedForeignKeyLeafChanged)
			assert.Empty(t, f.foreignKeys(t, "events"), "the parent has no foreign key")
		})

		// A leaf whose recorded foreign key is gone makes the parent's ADD
		// create and validate one there under the tree's locks. The read-back
		// sees the leaf's adopted foreign key is not the recorded one and
		// fails, so the step's transaction rolls back.
		t.Run("the read-back refuses an ADD that created a leaf's foreign key", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, fk, leaves := f.recordLeafFKs(t)
			feb := f.leaf(t, "events_2026_02")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{leaves[feb.oid].constraint}.Sanitize())
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentFKSQL(target, fk.spec))
			require.NoError(t, err)

			_, err = readBackParentFK(t.Context(), tx, target, fk, tree, leaves)

			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		// A leaf foreign key with another ON DELETE is not adopted: the
		// parent's ADD creates and validates a new one beside it, which the
		// read-back sees.
		t.Run("the read-back refuses an ADD that passed over a leaf's foreign key of another definition", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, fk, leaves := f.recordLeafFKs(t)
			feb := f.leaf(t, "events_2026_02")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{leaves[feb.oid].constraint}.Sanitize())
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT `+pgx.Identifier{leaves[feb.oid].constraint}.Sanitize()+`
				    FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id) ON DELETE CASCADE`)
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentFKSQL(target, fk.spec))
			require.NoError(t, err)

			_, err = readBackParentFK(t.Context(), tx, target, fk, tree, leaves)

			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		t.Run("the read-back accepts an ADD that adopted every leaf", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, fk, leaves := f.recordLeafFKs(t)
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentFKSQL(target, fk.spec))
			require.NoError(t, err)

			oid, err := readBackParentFK(t.Context(), tx, target, fk, tree, leaves)

			require.NoError(t, err)
			assert.NotZero(t, oid)
		})
	})
}

// The parent's step holds SHARE ROW EXCLUSIVE on the parent and on every
// leaf until it commits, taken first by its own LOCK TABLE under the
// transaction's lock_timeout, and on the referenced table both SHARE ROW
// EXCLUSIVE, which it takes itself, and ACCESS EXCLUSIVE, which the ADD takes
// to drop each adopted foreign key's own triggers there.
func TestAddPartitionedForeignKeyParentStepLocks(t *testing.T) {
	withPartitionedFKServer(t, func(t *testing.T, newFixture func(t *testing.T) partitionedFKFixture) {
		f := newFixture(t)
		f.events(t)
		target, fk, leaves := f.recordLeafFKs(t)
		tx := f.begin(t)

		out, err := adoptLeafFKs(t.Context(), tx, target, fk, leaves)

		require.NoError(t, err)
		require.True(t, out.added)
		modes := fkRelationLockModes(t, tx, "events", "events_2026_01", "events_2026_02", "events_2026_03", "accounts")
		for _, table := range []string{"events", "events_2026_01", "events_2026_02", "events_2026_03"} {
			assert.Contains(t, modes[table], "ShareRowExclusiveLock", "table %s", table)
			assert.NotContains(t, modes[table], "AccessExclusiveLock", "table %s", table)
		}
		assert.Contains(t, modes["accounts"], "ShareRowExclusiveLock")
		assert.Contains(t, modes["accounts"], "AccessExclusiveLock")
	})
}

// fkRelationLockModes lists, for each named relation, the lock modes the
// transaction's own backend holds on it.
func fkRelationLockModes(t *testing.T, tx pgx.Tx, names ...string) map[string][]string {
	t.Helper()
	rows, err := tx.Query(t.Context(), `
		SELECT c.relname::text, l.mode
		  FROM pg_catalog.pg_locks l
		  JOIN pg_catalog.pg_class c ON c.oid = l.relation
		 WHERE l.pid = pg_catalog.pg_backend_pid()
		   AND l.granted
		   AND c.relname::text = ANY($1)`, names)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, mode string
		require.NoError(t, rows.Scan(&name, &mode))
		out[name] = append(out[name], mode)
	}
	require.NoError(t, rows.Err())
	for _, modes := range out {
		sort.Strings(modes)
	}
	return out
}
