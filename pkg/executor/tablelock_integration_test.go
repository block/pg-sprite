package executor_test

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
)

// lockedTable is a table on a real server together with the proofs its
// mutating entry points require, so a test can swap one proof for a bad one
// and leave everything else admissible.
type lockedTable struct {
	pool   *pgxpool.Pool
	schema string
	pt     preflight.PreflightedTable
	lock   *dbconn.TableLock
}

func newLockedTable(t *testing.T) lockedTable {
	t.Helper()
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", schema))
	require.NoError(t, err)
	pt, lock := mustPreflight(t, pool, schema, "t")
	return lockedTable{pool: pool, schema: schema, pt: pt, lock: lock}
}

// addColumn is an instant change that would certainly succeed if it ran, so
// a refusal is the guard's doing rather than the statement's.
func (f lockedTable) addColumn(column string) string {
	return fmt.Sprintf("ALTER TABLE %s.t ADD COLUMN %s int", f.schema, column)
}

// The lock is a proof, and the core does not trust its callers to have one:
// every mutating entry point re-verifies it and refuses a change without
// one, naming the invariant it is enforcing. The refusal is an invariant
// violation rather than an ordinary failure because a caller reaching the
// executor without a lock is a bug in pg-sprite, not a condition in the
// database. Nothing is executed.
func TestMutatingEntryPointsRefuseAMissingTableLock(t *testing.T) {
	f := newLockedTable(t)

	t.Run("native", func(t *testing.T) {
		st := mustParse(t, f.addColumn("a"))
		err := executor.ExecuteNative(t.Context(), f.pool, f.pt, nil, st, budget, executor.DefaultRetryPolicy())
		requireLK1Refusal(t, err)
		assert.False(t, columnExists(t, f.pool, f.schema, "t", "a"))
	})

	t.Run("sequence", func(t *testing.T) {
		_, err := executor.RunSequence(t.Context(), f.pool, f.pt, nil,
			[]string{f.addColumn("b")}, runBudget, executor.DefaultRetryPolicy())
		requireLK1Refusal(t, err)
		assert.False(t, columnExists(t, f.pool, f.schema, "t", "b"))
	})

	t.Run("concurrent index build", func(t *testing.T) {
		_, err := executor.BuildIndexConcurrently(t.Context(), f.pool, nil,
			fmt.Sprintf("CREATE INDEX CONCURRENTLY idx_no_lock ON %s.t (id)", f.schema), buildBudget)
		requireLK1Refusal(t, err)
		assert.False(t, relationExists(t, f.pool, f.schema, "idx_no_lock"))
	})

	t.Run("create", func(t *testing.T) {
		c := newCreateFixture(t, "created")
		_, err := executor.ExecuteCreate(t.Context(), c.pool, c.at, c.cr, nil,
			desired(t, "CREATE TABLE created (id int PRIMARY KEY)"),
			budget, executor.DefaultRetryPolicy())
		requireLK1Refusal(t, err)
		assert.False(t, relationExists(t, c.pool, c.schema, "created"))
	})
}

// Holding a lock is not enough — it has to be the lock for the table being
// changed. A proof taken on one table cannot license a change to another,
// which is what keeps the per-table key from degrading into a per-process
// "some lock is held" flag.
func TestMutatingEntryPointsRefuseALockOnAnotherTable(t *testing.T) {
	f := newLockedTable(t)
	elsewhere := testutil.TableLock(t, f.pool, f.schema, "other")

	st := mustParse(t, f.addColumn("a"))
	err := executor.ExecuteNative(t.Context(), f.pool, f.pt, elsewhere, st, budget, executor.DefaultRetryPolicy())

	requireLK1Refusal(t, err)
	assert.Contains(t, err.Error(), "other", "the refusal must name the table the lock is actually held on")
	assert.False(t, columnExists(t, f.pool, f.schema, "t", "a"))
}

// A proof minted earlier says nothing about now. When the session holding
// the lock is gone the lock is gone with it, so the re-verification reads
// the server rather than the value in hand, and a change asked for after
// that point is refused instead of running with exclusion it lost.
func TestMutatingEntryPointsRefuseALostTableLock(t *testing.T) {
	f := newLockedTable(t)

	_, err := f.pool.Exec(t.Context(), "SELECT pg_terminate_backend($1)", f.lock.BackendPID())
	require.NoError(t, err)
	require.Error(t, f.lock.Confirm(t.Context()))

	st := mustParse(t, f.addColumn("a"))
	err = executor.ExecuteNative(t.Context(), f.pool, f.pt, f.lock, st, budget, executor.DefaultRetryPolicy())

	requireLK1Refusal(t, err)
	require.ErrorIs(t, err, dbconn.ErrLockLost)
	assert.False(t, columnExists(t, f.pool, f.schema, "t", "a"))
}

// requireLK1Refusal asserts a refusal is the fail-closed invariant kind and
// says which invariant, so an operator reading it knows the change did not
// run and why it was not allowed to.
func requireLK1Refusal(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	require.Contains(t, err.Error(), "LK-1")
}
