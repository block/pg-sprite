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

// keyFixture is leafKeyFixture plus the connection target the build's table
// lock dials.
type keyFixture struct {
	leafKeyFixture
	cfg dbconn.Config
}

// withPartitionedKeyServer runs the subtests against one server, each in a
// schema of its own.
func withPartitionedKeyServer(t *testing.T, run func(t *testing.T, newFixture func(t *testing.T) keyFixture)) {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	run(t, func(t *testing.T) keyFixture {
		t.Helper()
		return keyFixture{leafKeyFixture: leafKeyFixture{pool: pool, schema: testutil.NewSchema(t, pool)}, cfg: cfg}
	})
}

// add runs the build of sql on the fixture's events table, every %[1]s in
// sql replaced by the fixture's schema.
func (f keyFixture) add(t *testing.T, sql string) (PartitionedKeyReport, error) {
	t.Helper()
	return f.addAt(t, sql, RunStep{})
}

func (f keyFixture) addAt(t *testing.T, sql string, at RunStep) (PartitionedKeyReport, error) {
	t.Helper()
	pt, err := preflight.CheckTable(t.Context(), f.pool, f.schema, "events", 1<<30)
	require.NoError(t, err)
	return AddPartitionedKeyAsStep(t.Context(), f.cfg, f.pool, pt, fmt.Sprintf(sql, f.schema), leafKeyBudget, at)
}

// requireAdopted asserts the parent has the constraint the report names, of
// kind contype, over a valid partitioned index, and that every leaf the
// report lists has exactly one key constraint: the one its leaf step left,
// adopted by the parent's constraint, on the index OID the leaf step
// reported, which is now attached to the parent's index. An unchanged index
// OID is the proof the parent's ADD CONSTRAINT built nothing.
func (f keyFixture) requireAdopted(t *testing.T, rep PartitionedKeyReport, contype string) {
	t.Helper()
	parent, ok := f.keyConstraints(t, "events")[rep.Constraint]
	require.True(t, ok, "the parent has constraint %s", rep.Constraint)
	assert.Equal(t, rep.ConstraintOID, parent.oid)
	assert.Equal(t, contype, parent.contype)
	assert.True(t, parent.indexValid, "the parent's index is valid")
	for _, leaf := range rep.Leaves {
		got := f.keyConstraints(t, leaf.Table)
		require.Len(t, got, 1, "partition %s has only the adopted constraint", leaf.Table)
		assert.Equal(t, keyConstraint{
			oid: leaf.ConstraintOID, contype: contype, parentOID: parent.oid,
			index: leaf.Index, indexOID: leaf.IndexOID, indexValid: true,
		}, got[leaf.Constraint], "partition %s", leaf.Table)
		assert.Len(t, f.indexes(t, leaf.Table), 1, "partition %s has no second index", leaf.Table)
		var attachedTo uint32
		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT inhparent FROM pg_inherits WHERE inhrelid = $1`, leaf.IndexOID).Scan(&attachedTo))
		assert.Equal(t, parent.indexOID, attachedTo, "partition %s's index is attached to the parent's", leaf.Table)
	}
}

// requireNothingCreated asserts the tables have no index and no key
// constraint: the build refused before it created anything.
func (f keyFixture) requireNothingCreated(t *testing.T, tables ...string) {
	t.Helper()
	for _, table := range tables {
		assert.Empty(t, f.indexes(t, table), "table %s has no index", table)
		assert.Empty(t, f.keyConstraints(t, table), "table %s has no key constraint", table)
	}
}

// leafActions maps each reported leaf to its action.
func leafActions(rep PartitionedKeyReport) map[string]PartitionKeyAction {
	out := map[string]PartitionKeyAction{}
	for _, l := range rep.Leaves {
		out[l.Table] = l.Action
	}
	return out
}

const addEventsPrimaryKey = `ALTER TABLE %[1]s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at)`

// The primary key is added online: each leaf's unique index is built
// concurrently and given its constraint, and the parent's ADD CONSTRAINT then
// adopts them all in one brief transaction. A writer under a one-second
// lock_timeout keeps committing for the whole run; it waits only for the
// parent's step, which changes the catalog and builds nothing.
func TestAddPartitionedKeyPrimaryKeyOnline(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `
			INSERT INTO %[1]s.events (id, created_at, payload)
			SELECT g, '2026-01-01'::timestamptz + mod(g, 59) * interval '1 day', repeat('x', 200)
			  FROM generate_series(10001, 200000) g`)

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
		insert := fmt.Sprintf(`INSERT INTO %s.events (id, created_at) VALUES ($1, '2026-01-15'), ($1, '2026-02-15')`, f.schema)
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

		rep, err := f.add(t, addEventsPrimaryKey)
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
		assert.Equal(t, 2, rep.Partitions)
		assert.Equal(t, map[string]PartitionKeyAction{
			"events_2026_01": PartitionKeyBuilt,
			"events_2026_02": PartitionKeyBuilt,
		}, leafActions(rep))
		assert.NotEmpty(t, rep.ServerVersion)
		f.requireAdopted(t, rep, "p")
	})
}

func TestAddPartitionedKeyUnique(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		f := newFixture(t)
		f.events(t)

		rep, err := f.add(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_id_key UNIQUE (id, created_at)`)

		require.NoError(t, err)
		assert.True(t, rep.ParentAdded)
		assert.Equal(t, "events_id_key", rep.Constraint)
		assert.Len(t, rep.Leaves, 2)
		f.requireAdopted(t, rep, "u")
	})
}

// A rerun after the parent's constraint was added, the state a run that
// stopped after its last COMMIT leaves, finds it and reports it done. It
// changes nothing and does not mistake the partitions' adopted constraints
// for conflicts.
func TestAddPartitionedKeyRerunAfterSuccessIsDone(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		f := newFixture(t)
		f.events(t)
		first, err := f.add(t, addEventsPrimaryKey)
		require.NoError(t, err)
		before := map[string]map[string]keyConstraint{}
		for _, table := range []string{"events", "events_2026_01", "events_2026_02"} {
			before[table] = f.keyConstraints(t, table)
		}

		again, err := f.add(t, addEventsPrimaryKey)

		require.NoError(t, err)
		assert.False(t, again.ParentAdded)
		assert.Equal(t, first.ConstraintOID, again.ConstraintOID)
		assert.Empty(t, again.Leaves)
		assert.Zero(t, again.Passes)
		assert.Equal(t, 2, again.Partitions)
		for table, want := range before {
			assert.Equal(t, want, f.keyConstraints(t, table), "table %s is unchanged", table)
		}
	})
}

// A run resumes from what an interrupted run or an operator left on the
// leaves: a leaf that already has the key constraint is adopted as it is,
// and only the rest are built.
func TestAddPartitionedKeyResumes(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		t.Run("a leaf an interrupted run finished", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			done, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, f.leaf(t, "events", "events_2026_01"), leafKeyBudget)
			require.NoError(t, err)

			rep, err := f.add(t, addEventsPrimaryKey)

			require.NoError(t, err)
			assert.Equal(t, map[string]PartitionKeyAction{
				"events_2026_01": PartitionKeyFound,
				"events_2026_02": PartitionKeyBuilt,
			}, leafActions(rep))
			assert.Equal(t, done.indexOID, rep.Leaves[0].IndexOID, "the finished leaf's index is adopted, not rebuilt")
			f.requireAdopted(t, rep, "p")
		})

		// An operator's own primary key on a leaf, of the requested
		// definition and under a name of their choosing, is adopted under
		// that name.
		t.Run("a leaf with an operator's matching constraint", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `ALTER TABLE %[1]s.events_2026_02 ADD CONSTRAINT events_feb_pkey PRIMARY KEY (id, created_at)`)

			rep, err := f.add(t, addEventsPrimaryKey)

			require.NoError(t, err)
			assert.Equal(t, map[string]PartitionKeyAction{
				"events_2026_01": PartitionKeyBuilt,
				"events_2026_02": PartitionKeyFound,
			}, leafActions(rep))
			assert.Equal(t, "events_feb_pkey", rep.Leaves[1].Constraint)
			f.requireAdopted(t, rep, "p")
		})
	})
}

// hookClock is a progress clock that lets a test act at a step boundary: the
// tracker reads it when a statement starts, after the build decided what to
// run and before it runs it. The hook runs on each reading until it reports
// that it acted.
type hookClock struct {
	mu   sync.Mutex
	done bool
	hook func() bool
}

func (c *hookClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.done {
		c.done = c.hook()
	}
	return time.Now()
}

// atParentStep returns a run step whose tracker runs act once, when the
// parent's step is about to start: every listed leaf then has its key
// constraint.
func (f keyFixture) atParentStep(t *testing.T, act func()) RunStep {
	t.Helper()
	tracker, err := progress.NewTracker(&hookClock{hook: func() bool {
		for _, leaf := range []string{"events_2026_01", "events_2026_02"} {
			if len(f.keyConstraints(t, leaf)) == 0 {
				return false
			}
		}
		act()
		return true
	}})
	require.NoError(t, err)
	tracker.Start(1, progress.OperationBrief)
	return RunStep{Tracker: tracker, Step: 1}
}

// A partition created after the leaves were listed has no key constraint,
// so the parent's step adds nothing; the next pass gives it the key, and the
// parent's constraint then adopts all three.
func TestAddPartitionedKeySettlesAPartitionCreatedMidRun(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		f := newFixture(t)
		f.events(t)
		at := f.atParentStep(t, func() {
			f.exec(t, `
				CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
				    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')`)
			f.exec(t, `INSERT INTO %[1]s.events (id, created_at) VALUES (1, '2026-03-02'), (2, '2026-03-03')`)
		})

		rep, err := f.addAt(t, addEventsPrimaryKey, at)

		require.NoError(t, err)
		assert.Equal(t, 2, rep.Passes)
		assert.Equal(t, 3, rep.Partitions)
		assert.Equal(t, map[string]PartitionKeyAction{
			"events_2026_01": PartitionKeyBuilt,
			"events_2026_02": PartitionKeyBuilt,
			"events_2026_03": PartitionKeyBuilt,
		}, leafActions(rep))
		f.requireAdopted(t, rep, "p")
	})
}

// Every refusal comes before the build creates anything, and each is typed.
func TestAddPartitionedKeyRefusals(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		// The table is partitioned by created_at, which a unique key must
		// include. The refusal is the partitioned unique index build's own.
		t.Run("a key without the partition key column", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)

			_, err := f.add(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_id_key UNIQUE (id)`)

			var keyErr *UniqueIndexPartitionKeyError
			require.ErrorAs(t, err, &keyErr)
			assert.Equal(t, CodeUniqueIndexPartitionKey, OutcomeCode(err))
			assert.Equal(t, []MissingPartitionKey{{Schema: f.schema, Table: "events", Columns: []string{"created_at"}}}, keyErr.Missing)
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02")
		})

		t.Run("a parent constraint under the name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_pkey CHECK (id > 0)`)

			_, err := f.add(t, addEventsPrimaryKey)

			require.ErrorIs(t, err, ErrPartitionedKeyParentConflict)
			assert.Equal(t, CodePartitionedKeyParentConflict, OutcomeCode(err))
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02")
		})

		// The parent's index takes the constraint's name, so the ADD
		// CONSTRAINT would fail only once every leaf was built.
		t.Run("an index in the schema under the name", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `CREATE TABLE %[1]s.audit (id bigint)`)
			f.exec(t, `CREATE INDEX events_pkey ON %[1]s.audit (id)`)

			_, err := f.add(t, addEventsPrimaryKey)

			require.ErrorIs(t, err, ErrPartitionedKeyParentConflict)
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02")
		})

		t.Run("a primary key when the parent has one", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_old_pkey PRIMARY KEY (id, created_at)`)
			before := map[string]map[string]keyConstraint{}
			for _, table := range []string{"events", "events_2026_01", "events_2026_02"} {
				before[table] = f.keyConstraints(t, table)
			}

			_, err := f.add(t, addEventsPrimaryKey)

			require.ErrorIs(t, err, ErrPartitionedKeyParentConflict)
			for table, want := range before {
				assert.Equal(t, want, f.keyConstraints(t, table), "table %s is unchanged", table)
				assert.Len(t, f.indexes(t, table), 1, "table %s has only the existing key's index", table)
			}
		})

		// payload allows NULL, so the parent's ADD CONSTRAINT would set NOT
		// NULL and scan every partition under ACCESS EXCLUSIVE.
		t.Run("a primary key over a column the parent allows to be NULL", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)

			_, err := f.add(t, `ALTER TABLE %[1]s.events ADD CONSTRAINT events_pkey PRIMARY KEY (id, created_at, payload)`)

			require.ErrorIs(t, err, ErrPartitionedKeyNullableColumn)
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02")
		})

		t.Run("a sub-partitioned partition", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				CREATE TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
				    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')
				    PARTITION BY RANGE (created_at)`)

			_, err := f.add(t, addEventsPrimaryKey)

			require.ErrorIs(t, err, ErrPartitionedKeySubPartitioned)
			assert.Equal(t, CodePartitionedKeySubPartitioned, OutcomeCode(err))
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02", "events_2026_03")
		})

		// A foreign table cannot carry the index a key needs.
		t.Run("a foreign table partition", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `CREATE FOREIGN DATA WRAPPER fdw_%[1]s`)
			t.Cleanup(func() {
				_, err := f.pool.Exec(context.WithoutCancel(t.Context()), `DROP FOREIGN DATA WRAPPER IF EXISTS fdw_`+f.schema+` CASCADE`)
				assert.NoError(t, err)
			})
			f.exec(t, `CREATE SERVER srv_%[1]s FOREIGN DATA WRAPPER fdw_%[1]s`)
			f.exec(t, `
				CREATE FOREIGN TABLE %[1]s.events_2026_03 PARTITION OF %[1]s.events
				    FOR VALUES FROM ('2026-03-01') TO ('2026-04-01')
				    SERVER srv_%[1]s`)

			_, err := f.add(t, addEventsPrimaryKey)

			require.ErrorIs(t, err, ErrForeignTablePartition)
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02")
		})

		// A cancelled DETACH PARTITION ... CONCURRENTLY leaves events_2026_02
		// detach-pending: it is leaving the tree, so a key given to it would
		// not stay the parent's. The tree read refuses it before any leaf
		// step, so events_2026_01, listed first, gets no index either.
		t.Run("a partition with its detach pending", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.cancelDetach(t, "events", "events_2026_02")

			_, err := f.add(t, addEventsPrimaryKey)

			require.ErrorIs(t, err, ErrPartitionDetachPending)
			assert.Equal(t, CodePartitionDetachPending, OutcomeCode(err))
			f.requireNothingCreated(t, "events", "events_2026_01", "events_2026_02")
		})
	})
}

// recordLeafKeys runs the leaf step on both leaves and returns what the
// parent's step proves: the target and each leaf's recorded key.
func (f keyFixture) recordLeafKeys(t *testing.T, spec partitionedKeySpec) (indexTarget, map[uint32]leafKeyResult) {
	t.Helper()
	leaves := map[uint32]leafKeyResult{}
	for _, name := range []string{"events_2026_01", "events_2026_02"} {
		leaf := f.leaf(t, "events", name)
		res, err := ensureLeafKey(t.Context(), f.pool, spec, leaf, leafKeyBudget)
		require.NoError(t, err)
		leaves[leaf.oid] = res
	}
	return indexTarget{tableOID: f.oid(t, "events"), schema: f.schema, table: "events"}, leaves
}

// begin opens a transaction under the bounds a brief step runs with.
func (f keyFixture) begin(t *testing.T) pgx.Tx {
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

// replaceFebruaryKey swaps the February leaf's key constraint for an
// identical one on a new index, the change an operator could make between
// the leaf step and the parent's step.
func (f keyFixture) replaceFebruaryKey(t *testing.T, recorded leafKeyResult) {
	t.Helper()
	f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{recorded.constraint}.Sanitize())
	f.exec(t, `CREATE UNIQUE INDEX events_feb_other ON %[1]s.events_2026_02 (id, created_at)`)
	f.exec(t, `ALTER TABLE %[1]s.events_2026_02 ADD CONSTRAINT events_feb_other PRIMARY KEY USING INDEX events_feb_other`)
}

// The parent's step adds the parent's constraint only over exactly the leaf
// constraints and indexes the leaf steps recorded, and reads back that it
// adopted them; any other state rolls it back with an invariant violation.
func TestAddPartitionedKeyParentStepFailsClosed(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		// A leaf whose key constraint was replaced after its leaf step still
		// has a key the parent could adopt, but not the one the run made, so
		// the proof under the tree's lock refuses before the ADD CONSTRAINT.
		t.Run("the proof refuses a leaf key replaced after its leaf step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			feb := f.leaf(t, "events", "events_2026_02")
			f.replaceFebruaryKey(t, leaves[feb.oid])

			err := proveLeafKeyRecorded(t.Context(), f.pool, eventsPrimaryKey, feb, leaves[feb.oid])

			require.ErrorIs(t, err, ErrInvariantViolation)
			_, err = adoptLeafKeys(t.Context(), f.begin(t), target, eventsPrimaryKey, leaves)
			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		// The same change made while a whole build runs, between its leaf
		// steps and its parent's step: the build fails closed and the
		// parent gets no constraint.
		t.Run("a build whose leaf key is replaced before the parent's step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			at := f.atParentStep(t, func() {
				recorded := f.keyConstraints(t, "events_2026_02")
				require.Len(t, recorded, 1)
				for name := range recorded {
					f.replaceFebruaryKey(t, leafKeyResult{constraint: name})
				}
			})

			_, err := f.addAt(t, addEventsPrimaryKey, at)

			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Equal(t, CodeInvariantViolation, OutcomeCode(err))
			assert.Empty(t, f.keyConstraints(t, "events"), "the parent has no constraint")
			for _, leaf := range []string{"events_2026_01", "events_2026_02"} {
				for name, k := range f.keyConstraints(t, leaf) {
					assert.Zero(t, k.parentOID, "constraint %s on %s is adopted by nothing", name, leaf)
				}
			}
		})

		// A leaf without its recorded key makes the parent's ADD CONSTRAINT
		// build that leaf's index under ACCESS EXCLUSIVE. The read-back sees
		// the leaf's adopted index is not the recorded one and fails, so the
		// step's transaction rolls back.
		t.Run("the read-back refuses an ADD CONSTRAINT that built a leaf's index", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			feb := f.leaf(t, "events", "events_2026_02")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{leaves[feb.oid].constraint}.Sanitize())
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentKeySQL(target, eventsPrimaryKey))
			require.NoError(t, err)

			_, err = readBackParentKey(t.Context(), tx, target, eventsPrimaryKey, tree, leaves)

			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		t.Run("the read-back accepts an ADD CONSTRAINT that adopted every leaf", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentKeySQL(target, eventsPrimaryKey))
			require.NoError(t, err)

			oid, err := readBackParentKey(t.Context(), tx, target, eventsPrimaryKey, tree, leaves)

			require.NoError(t, err)
			assert.NotZero(t, oid)
		})
	})
}

// The parent's step holds ACCESS EXCLUSIVE on the parent and on every leaf
// until it commits, the locks the ADD CONSTRAINT takes itself, taken first by
// the step's own LOCK TABLE under the transaction's lock_timeout.
func TestAddPartitionedKeyParentStepLocks(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		f := newFixture(t)
		f.events(t)
		target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
		tx := f.begin(t)

		out, err := adoptLeafKeys(t.Context(), tx, target, eventsPrimaryKey, leaves)

		require.NoError(t, err)
		require.True(t, out.added)
		modes := relationLockModes(t, tx, "events", "events_2026_01", "events_2026_02")
		for _, table := range []string{"events", "events_2026_01", "events_2026_02"} {
			assert.Contains(t, modes[table], "AccessExclusiveLock", "table %s", table)
		}
	})
}

// relationLockModes lists, for each named relation, the lock modes the
// transaction's own backend holds on it.
func relationLockModes(t *testing.T, tx pgx.Tx, names ...string) map[string][]string {
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
