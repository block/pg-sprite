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
