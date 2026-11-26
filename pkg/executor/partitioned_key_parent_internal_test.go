package executor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// statementCounter is a pgx tracer that counts every statement its pool's
// connections send.
type statementCounter struct{ n atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// proveLeafKeyRecorded reads one partition and judges it as the parent's
// step judges every partition of the tree.
func proveLeafKeyRecorded(ctx context.Context, q querier, spec partitionedKeySpec, n partitionNode, rec leafKeyResult) error {
	keyColumns, err := readKeyColumnsOf(ctx, q, []partitionNode{n}, spec.columns)
	if err != nil {
		return err
	}
	keyConstraints, err := readLeafConstraintsOf(ctx, q, []partitionNode{n})
	if err != nil {
		return err
	}
	return judgeLeafKeyRecorded(spec, n, keyColumns[n.oid], keyConstraints[n.oid], rec)
}

// dailyEvents creates a range-partitioned table with one leaf per day, each
// given its key by the leaf step, and returns what the parent's step proves.
func (f keyFixture) dailyEvents(t *testing.T, leaves int) (indexTarget, map[uint32]leafKeyResult) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %[1]s.events (
		    id         bigint      NOT NULL,
		    created_at timestamptz NOT NULL
		) PARTITION BY RANGE (created_at)`)
	for day := range leaves {
		f.exec(t, fmt.Sprintf(`
			CREATE TABLE %%[1]s.events_d%03d PARTITION OF %%[1]s.events
			    FOR VALUES FROM ('2026-01-01'::date + %d) TO ('2026-01-01'::date + %d)`, day, day, day+1))
	}
	target := indexTarget{tableOID: f.oid(t, "events"), schema: f.schema, table: "events"}
	tree, err := listPartitionTree(t.Context(), f.pool, target.tableOID)
	require.NoError(t, err)
	require.Len(t, tree, leaves)
	recorded := map[uint32]leafKeyResult{}
	for _, n := range tree {
		res, err := ensureLeafKey(t.Context(), f.pool, eventsPrimaryKey, n, leafKeyBudget)
		require.NoError(t, err)
		recorded[n.oid] = res
	}
	return target, recorded
}

// parentStepStatements runs the parent's step in a transaction on a pool
// that counts statements, and returns how many the step sent between its
// LOCK TABLE and its last read, and how long that took: the time the tree's
// ACCESS EXCLUSIVE is held before COMMIT.
func (f keyFixture) parentStepStatements(t *testing.T, target indexTarget, leaves map[uint32]leafKeyResult) (int64, time.Duration) {
	t.Helper()
	counter := &statementCounter{}
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	tx, err := pool.Begin(t.Context())
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

	before := counter.n.Load()
	start := time.Now()
	out, err := adoptLeafKeys(t.Context(), tx, target, eventsPrimaryKey, leaves)
	held := time.Since(start)

	require.NoError(t, err)
	require.True(t, out.added)
	assert.Equal(t, len(leaves), out.partitions)
	statements := counter.n.Load() - before
	// The open transaction would hold off the next fixture's concurrent
	// index builds, which wait out every older transaction.
	require.NoError(t, tx.Rollback(t.Context()))
	return statements, held
}

// The proof and the read-back read every partition at once and still judge
// each partition on its own: a mismatch on any one of them rolls the step
// back.
func TestAddPartitionedKeyParentStepJudgesEveryLeaf(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		// Dropping the leaf's constraint drops the index it owns, so the
		// parent's ADD CONSTRAINT would build the February index under
		// ACCESS EXCLUSIVE; the proof finds no key constraint on the leaf.
		t.Run("the proof refuses a leaf that lost its key constraint", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			feb := f.leaf(t, "events", "events_2026_02")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{leaves[feb.oid].constraint}.Sanitize())

			_, err := adoptLeafKeys(t.Context(), f.begin(t), target, eventsPrimaryKey, leaves)

			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		// A UNIQUE constraint over the key's columns beside the leaf's
		// primary key leaves the parent's adoption to chance, so the
		// proof refuses it as the leaf step does.
		t.Run("the proof refuses a leaf with a second constraint over the key's columns", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT events_feb_unique UNIQUE (id, created_at)`)

			_, err := adoptLeafKeys(t.Context(), f.begin(t), target, eventsPrimaryKey, leaves)

			require.ErrorIs(t, err, ErrPartitionedKeyConflict)
		})

		// Detaching February after the ADD CONSTRAINT, in the same
		// transaction, frees its constraint from the parent's and its index
		// from the parent's index, so the read-back finds no adopted
		// constraint on a partition of the tree it listed.
		t.Run("the read-back refuses a partition whose constraint is not adopted", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentKeySQL(target, eventsPrimaryKey))
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %[1]s.events DETACH PARTITION %[1]s.events_2026_02`, f.schema))
			require.NoError(t, err)

			_, err = readBackParentKey(t.Context(), tx, target, eventsPrimaryKey, tree, leaves)

			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		// Each leaf's recorded key judged against another leaf's: the
		// read-back compares every partition with its own record.
		t.Run("the read-back refuses records swapped between partitions", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			jan, feb := f.leaf(t, "events", "events_2026_01"), f.leaf(t, "events", "events_2026_02")
			swapped := map[uint32]leafKeyResult{jan.oid: leaves[feb.oid], feb.oid: leaves[jan.oid]}
			tx := f.begin(t)
			tree, err := listPartitionTree(t.Context(), tx, target.tableOID)
			require.NoError(t, err)
			_, err = tx.Exec(t.Context(), parentKeySQL(target, eventsPrimaryKey))
			require.NoError(t, err)

			_, err = readBackParentKey(t.Context(), tx, target, eventsPrimaryKey, tree, swapped)

			require.ErrorIs(t, err, ErrInvariantViolation)
		})
	})
}

// A partition carries the key's index and an index attached to another
// partitioned index of the parent; the read-back reads only the index
// attached to the key's, so the step adopts the key.
func TestAddPartitionedKeyParentStepWithAnotherPartitionedIndex(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `CREATE INDEX events_payload_idx ON %[1]s.events (payload)`)
		target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)

		out, err := adoptLeafKeys(t.Context(), f.begin(t), target, eventsPrimaryKey, leaves)

		require.NoError(t, err)
		assert.True(t, out.added)
		assert.Equal(t, 2, out.partitions)
	})
}

// The parent's step proves again, under the tree's lock, what the leaf steps
// and the build's first read proved, so a change made in between is refused
// with the same typed cause.
func TestAddPartitionedKeyParentStepRefusesChangesSinceTheLeafSteps(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		// February's key is replaced by a DEFERRABLE constraint on the same
		// columns. It has the key's index definition and the parent's
		// immediate constraint would adopt it, leaving February checked only
		// at commit, so the proof refuses it as the leaf step does.
		t.Run("a deferrable constraint in place of a leaf's key", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsUniqueKey)
			feb := f.leaf(t, "events", "events_2026_02")
			f.exec(t, `ALTER TABLE %[1]s.events_2026_02 DROP CONSTRAINT `+pgx.Identifier{leaves[feb.oid].constraint}.Sanitize())
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_02
				    ADD CONSTRAINT events_feb_deferrable UNIQUE (id, created_at)
				    DEFERRABLE INITIALLY DEFERRED`)

			_, err := adoptLeafKeys(t.Context(), f.begin(t), target, eventsUniqueKey, leaves)

			require.ErrorIs(t, err, ErrPartitionedKeyConflict)
			assert.Equal(t, CodePartitionedKeyConflict, OutcomeCode(err))
		})

		// A cancelled DETACH PARTITION ... CONCURRENTLY after February's leaf
		// step leaves it detach-pending: it is leaving the tree, so the key
		// the parent would adopt there would not stay the parent's.
		t.Run("a partition left detach-pending after its leaf step", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			target, leaves := f.recordLeafKeys(t, eventsPrimaryKey)
			f.cancelDetach(t, "events", "events_2026_02")

			_, err := adoptLeafKeys(t.Context(), f.begin(t), target, eventsPrimaryKey, leaves)

			require.ErrorIs(t, err, ErrPartitionDetachPending)
			assert.Equal(t, CodePartitionDetachPending, OutcomeCode(err))
		})

		// From PostgreSQL 18 a NOT NULL can be added NOT VALID: the parent's
		// id is then marked attnotnull though the constraint was never
		// checked, and the parent's ADD CONSTRAINT ... PRIMARY KEY fails on
		// it under the tree's lock. The step counts the column as nullable
		// and refuses before the statement, as the build's first read does.
		t.Run("a NOT VALID not-null constraint on the parent's key column", func(t *testing.T) {
			f := newFixture(t)
			if f.serverMajor(t) < 18 {
				t.Skip("NOT VALID not-null constraints exist from PostgreSQL 18")
			}
			f.exec(t, `
				CREATE TABLE %[1]s.visits (
				    id         bigint,
				    created_at timestamptz NOT NULL
				) PARTITION BY RANGE (created_at)`)
			f.exec(t, `
				CREATE TABLE %[1]s.visits_2026_01 PARTITION OF %[1]s.visits (
				    id NOT NULL
				) FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')`)
			f.exec(t, `INSERT INTO %[1]s.visits VALUES (1, '2026-01-02'), (2, '2026-01-03')`)
			spec := partitionedKeySpec{kind: partitionedPrimaryKey, name: "visits_pkey", columns: []string{"id", "created_at"}}
			jan := f.leaf(t, "visits", "visits_2026_01")
			res, err := ensureLeafKey(t.Context(), f.pool, spec, jan, leafKeyBudget)
			require.NoError(t, err)
			f.exec(t, `ALTER TABLE %[1]s.visits ADD CONSTRAINT visits_id_nn NOT NULL id NOT VALID`)
			target := indexTarget{tableOID: f.oid(t, "visits"), schema: f.schema, table: "visits"}

			_, err = adoptLeafKeys(t.Context(), f.begin(t), target, spec, map[uint32]leafKeyResult{jan.oid: res})

			require.ErrorIs(t, err, ErrPartitionedKeyNullableColumn)
			assert.Equal(t, CodePartitionedKeyNullableColumn, OutcomeCode(err))
		})
	})
}

// The parent's step holds ACCESS EXCLUSIVE on the whole tree while it proves
// and reads back every partition, so the statements it sends under that lock
// do not grow with the number of partitions: the proof and the read-back
// read every partition at once.
func TestAddPartitionedKeyParentStepRoundTrips(t *testing.T) {
	withPartitionedKeyServer(t, func(t *testing.T, newFixture func(t *testing.T) keyFixture) {
		statements := map[int]int64{}
		for _, leaves := range []int{3, 12, 50} {
			f := newFixture(t)
			target, recorded := f.dailyEvents(t, leaves)

			n, held := f.parentStepStatements(t, target, recorded)

			statements[leaves] = n
			t.Logf("parent step with %d leaves: %d statements, lock held %s", leaves, n, held)
		}
		assert.Equal(t, statements[3], statements[12], "statements with 3 and with 12 leaves")
		assert.Equal(t, statements[3], statements[50], "statements with 3 and with 50 leaves")
	})
}
