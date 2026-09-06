package executor

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// staleProofFixture is the state every proof-staleness test starts from: a
// table with duplicate rows, the invalid leftover of a failed unique build
// on it, the observation the recovery would act on, and the resolved
// target — taken before the test moves the catalog under them.
type staleProofFixture struct {
	pool     *pgxpool.Pool
	conn     *pgxpool.Conn
	schema   string
	target   indexTarget
	existing invalidIndex
}

func newStaleProofFixture(t *testing.T) staleProofFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE %[1]s.t (id int PRIMARY KEY, c int); INSERT INTO %[1]s.t VALUES (1, 7), (2, 7)", schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY idx_left ON %s.t (c)", schema))
	require.Error(t, err, "a unique build over duplicates must fail")

	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	build := concurrentIndexBuild{index: "idx_left", tableSchema: schema, table: "t"}
	target, err := resolveTarget(t.Context(), conn, build)
	require.NoError(t, err)
	existing, found, err := inspectInvalidIndex(t.Context(), conn, schema, "idx_left")
	require.NoError(t, err)
	require.True(t, found, "the failed build must leave an invalid entry to observe")
	return staleProofFixture{pool: pool, conn: conn, schema: schema, target: target, existing: existing}
}

// exec runs one statement that moves the catalog after the observation.
func (f staleProofFixture) exec(t *testing.T, format string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(format, args...))
	require.NoError(t, err)
}

// makeValid turns the fixture's invalid leftover into a valid index under
// whatever name it carries, keeping its identity: with the duplicate gone,
// an in-place reindex of the entry succeeds and flips indisvalid on the
// same OID (a concurrent reindex would build a new entry instead).
func (f staleProofFixture) makeValid(t *testing.T, name string) {
	t.Helper()
	f.exec(t, "DELETE FROM %s.t WHERE id = 2", f.schema)
	f.exec(t, "REINDEX INDEX %s.%s", f.schema, name)
	var valid bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = $1`, f.existing.oid).Scan(&valid))
	require.True(t, valid, "the reindexed entry must be valid under its own OID")
}

// indexName reports the current name of the fixture's leftover by OID, or
// "" when the entry is gone.
func (f staleProofFixture) indexName(t *testing.T) string {
	t.Helper()
	var name *string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT (SELECT relname FROM pg_class WHERE oid = $1)`, f.existing.oid).Scan(&name))
	if name == nil {
		return ""
	}
	return *name
}

// TestQuarantineAbandonedIndexFailsClosedOnStaleObservation pins the
// under-lock re-verification: every way the catalog can move between the
// inspection and the lock grant must stop the rename, and the entry must
// stay exactly as the mover left it.
func TestQuarantineAbandonedIndexFailsClosedOnStaleObservation(t *testing.T) {
	t.Run("table renamed", func(t *testing.T) {
		f := newStaleProofFixture(t)
		f.exec(t, "ALTER TABLE %s.t RENAME TO t2", f.schema)

		err := quarantineAbandonedIndex(t.Context(), f.conn, f.target, "idx_left", f.existing)

		require.ErrorIs(t, err, ErrTargetIdentityChanged)
		var invalidErr *InvalidIndexError
		require.ErrorAs(t, err, &invalidErr)
		assert.Equal(t, "idx_left", invalidErr.Index)
		assert.Equal(t, "idx_left", f.indexName(t), "the entry keeps its name")
	})

	t.Run("table moved to another schema", func(t *testing.T) {
		f := newStaleProofFixture(t)
		other := testutil.NewSchema(t, f.pool)
		f.exec(t, "ALTER TABLE %s.t SET SCHEMA %s", f.schema, other)

		err := quarantineAbandonedIndex(t.Context(), f.conn, f.target, "idx_left", f.existing)

		require.ErrorIs(t, err, ErrTargetIdentityChanged)
		assert.Equal(t, "idx_left", f.indexName(t))
	})

	t.Run("index renamed", func(t *testing.T) {
		f := newStaleProofFixture(t)
		f.exec(t, "ALTER INDEX %s.idx_left RENAME TO idx_moved", f.schema)

		err := quarantineAbandonedIndex(t.Context(), f.conn, f.target, "idx_left", f.existing)

		require.ErrorIs(t, err, ErrAbandonmentUnproven)
		assert.Equal(t, "idx_moved", f.indexName(t), "the entry keeps the mover's name")
	})

	t.Run("index made valid", func(t *testing.T) {
		f := newStaleProofFixture(t)
		f.makeValid(t, "idx_left")

		err := quarantineAbandonedIndex(t.Context(), f.conn, f.target, "idx_left", f.existing)

		require.ErrorIs(t, err, ErrAbandonmentUnproven)
		assert.Equal(t, "idx_left", f.indexName(t), "a valid index is never renamed")
	})

	t.Run("index replaced by a concurrent reindex", func(t *testing.T) {
		f := newStaleProofFixture(t)
		// A concurrent reindex builds a new entry and drops the observed
		// one: the name now carries a valid index of another identity.
		f.exec(t, "DELETE FROM %s.t WHERE id = 2", f.schema)
		f.exec(t, "REINDEX INDEX CONCURRENTLY %s.idx_left", f.schema)
		require.Empty(t, f.indexName(t), "the observed identity is gone")

		err := quarantineAbandonedIndex(t.Context(), f.conn, f.target, "idx_left", f.existing)

		require.NoError(t, err, "a gone entry leaves nothing to quarantine; the build decides on the name")
		var valid bool
		require.NoError(t, f.pool.QueryRow(t.Context(),
			`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			   JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = 'idx_left'`,
			f.schema).Scan(&valid))
		assert.True(t, valid, "the replacement is untouched")
	})

	t.Run("index dropped", func(t *testing.T) {
		f := newStaleProofFixture(t)
		f.exec(t, "DROP INDEX %s.idx_left", f.schema)

		err := quarantineAbandonedIndex(t.Context(), f.conn, f.target, "idx_left", f.existing)

		require.NoError(t, err, "a gone entry leaves nothing to quarantine")
		assert.Empty(t, f.indexName(t))
	})
}

// TestDropQuarantinedIndexFailsClosedOnStaleEntry pins the pre-drop
// re-verification: an entry that stopped being an abandonment candidate
// between the sweep's listing and its drop must survive, and the failure
// must name the entry under its quarantine name.
func TestDropQuarantinedIndexFailsClosedOnStaleEntry(t *testing.T) {
	budget := ConcurrentBudget{Overall: time.Minute}
	quarantine := func(t *testing.T, f staleProofFixture) string {
		t.Helper()
		name := quarantineName(f.existing.oid)
		f.exec(t, "ALTER INDEX %s.idx_left RENAME TO %s", f.schema, name)
		return name
	}

	t.Run("entry made valid under its quarantine name", func(t *testing.T) {
		f := newStaleProofFixture(t)
		name := quarantine(t, f)
		f.makeValid(t, name)

		_, err := dropQuarantinedIndex(t.Context(), f.pool, f.target, f.existing, budget)

		require.ErrorIs(t, err, ErrAbandonmentUnproven)
		var invalidErr *InvalidIndexError
		require.ErrorAs(t, err, &invalidErr)
		assert.Equal(t, name, invalidErr.Index, "the failure names the entry as it is, by its quarantine name")
		assert.Equal(t, "t", invalidErr.Table)
		assert.Equal(t, name, f.indexName(t), "a valid index is never dropped")
	})

	t.Run("entry renamed away", func(t *testing.T) {
		f := newStaleProofFixture(t)
		quarantine(t, f)
		f.exec(t, "ALTER INDEX %s.%s RENAME TO idx_reclaimed", f.schema, quarantineName(f.existing.oid))

		_, err := dropQuarantinedIndex(t.Context(), f.pool, f.target, f.existing, budget)

		require.ErrorIs(t, err, ErrAbandonmentUnproven)
		assert.Equal(t, "idx_reclaimed", f.indexName(t), "the entry keeps the mover's name")
	})

	t.Run("entry still a candidate is dropped", func(t *testing.T) {
		f := newStaleProofFixture(t)
		name := quarantine(t, f)

		dropped, err := dropQuarantinedIndex(t.Context(), f.pool, f.target, f.existing, budget)

		require.NoError(t, err)
		assert.Equal(t, DroppedIndex{Schema: f.schema, Index: name, IndexOID: f.existing.oid, Duration: dropped.Duration}, dropped)
		assert.Positive(t, dropped.Duration)
		assert.Empty(t, f.indexName(t), "the entry is gone")
	})
}

// TestIndexFactsIsAbandonmentCandidate pins every term of the predicate
// both proofs re-check under their lock: a change in any one fact makes the
// entry something the recovery must not act on.
func TestIndexFactsIsAbandonmentCandidate(t *testing.T) {
	target := indexTarget{tableOID: 100, schema: "s"}
	candidate := indexFacts{oid: 7, name: "i", valid: false, tableOID: 100, droppable: true}
	assert.True(t, candidate.isAbandonmentCandidate(7, "i", target))

	tests := []struct {
		name  string
		facts indexFacts
	}{
		{name: "another identity under the name", facts: indexFacts{oid: 8, name: "i", tableOID: 100, droppable: true}},
		{name: "the identity under another name", facts: indexFacts{oid: 7, name: "j", tableOID: 100, droppable: true}},
		{name: "a valid index", facts: indexFacts{oid: 7, name: "i", valid: true, tableOID: 100, droppable: true}},
		{name: "an index on another table", facts: indexFacts{oid: 7, name: "i", tableOID: 200, droppable: true}},
		{name: "an index the server will not drop concurrently", facts: indexFacts{oid: 7, name: "i", tableOID: 100, droppable: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, tt.facts.isAbandonmentCandidate(7, "i", target))
		})
	}
}

// TestRemainingAfterSharesOneBudgetAcrossTheSweep pins the arithmetic the
// sweep bounds itself with: each drop gets what the earlier ones left, a
// spent budget is a statement-budget outcome rather than an unbounded
// hand-off, and a caller-owned budget passes through unchanged.
func TestRemainingAfterSharesOneBudgetAcrossTheSweep(t *testing.T) {
	served := ConcurrentBudget{Overall: 10 * time.Second}

	left, err := served.remainingAfter(4 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, ConcurrentBudget{Overall: 6 * time.Second}, left)

	_, err = served.remainingAfter(10 * time.Second)
	var budgetErr *BudgetError
	require.ErrorAs(t, err, &budgetErr)
	assert.Equal(t, CauseStatement, budgetErr.Cause)
	assert.Equal(t, served.Overall, budgetErr.Budget, "the outcome names the sweep's whole budget")

	_, err = served.remainingAfter(10*time.Second - time.Millisecond/2)
	require.ErrorAs(t, err, &budgetErr, "less than the server's granularity would disable the timeout, so it is spent")

	owned := ConcurrentBudget{CallerOwned: true}
	left, err = owned.remainingAfter(time.Hour)
	require.NoError(t, err)
	assert.Equal(t, owned, left)
}
