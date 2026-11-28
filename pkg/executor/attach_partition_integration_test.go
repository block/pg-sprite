package executor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// These tests run the attach step against the server: with the proofs
// validated and the parent's index already on the table, the ATTACH scans
// neither the table nor the DEFAULT partition, and the proof CHECKs go in
// the same transaction.

var attachPartitionBudget = Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}

// eventsAttachParent is a range-partitioned table with a DEFAULT partition
// and one index. events_2027 is the table to attach, already carrying the
// parent's index, with rows inside the bound; the DEFAULT partition holds a
// row outside it.
const eventsAttachParent = `
	CREATE TABLE %[1]s.events (
		id bigint NOT NULL,
		day date NOT NULL
	) PARTITION BY RANGE (day);
	CREATE INDEX events_id_idx ON %[1]s.events (id);
	CREATE TABLE %[1]s.events_2026 PARTITION OF %[1]s.events
		FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
	CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;
	INSERT INTO %[1]s.events VALUES (1, '2030-06-01');
	CREATE TABLE %[1]s.events_2027 (
		id bigint NOT NULL,
		day date NOT NULL
	);
	CREATE INDEX events_2027_id_idx ON %[1]s.events_2027 (id);
	INSERT INTO %[1]s.events_2027 VALUES (2, '2027-03-01'), (3, '2027-09-01')`

const attachEvents2027Unqualified = `ALTER TABLE events
	ATTACH PARTITION events_2027
	FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`

// attachTarget derives the proofs of attachEvents2027Unqualified, adds and validates
// them unless skip names one, and returns the step's target, with the
// statement unqualified under the fixture's schema.
func (f attachProofFixture) attachTarget(t *testing.T, skip ...attachProofKind) attachPartitionTarget {
	t.Helper()
	st, err := statement.ParseOne(attachEvents2027Unqualified)
	require.NoError(t, err)
	attach, ok, err := statement.AttachedPartition(attachEvents2027Unqualified)
	require.NoError(t, err)
	require.True(t, ok)
	parentOID := f.parentOIDOf(t, "events")
	check, exclusion, err := deriveAttachBoundCheck(t.Context(), briefTx{pool: f.pool}, parentOID, attach.Bound)
	require.NoError(t, err)
	target := attachPartitionTarget{
		st:         st,
		searchPath: AttemptSearchPath(f.schema, nil),
		parentOID:  parentOID,
		bound:      f.proofOn(t, attachProofBound, "events_2027", check),
		deflt:      f.proofOn(t, attachProofDefault, "events_default", exclusion),
	}
	for _, p := range target.proofs() {
		if !slices.Contains(skip, p.kind) {
			_, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil, nil)
			require.NoError(t, err)
		}
	}
	return target
}

// parentOIDOf resolves a table of the fixture's schema.
func (f attachProofFixture) parentOIDOf(t *testing.T, table string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT $1::regclass::oid",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&oid))
	return oid
}

// debugPool is a pool whose sessions run at client_min_messages = debug1,
// with the notices the server sends collected in order.
func (f attachProofFixture) debugPool(t *testing.T) (*pgxpool.Pool, func() []string) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.url)
	require.NoError(t, err)
	var mu sync.Mutex
	var notices []string
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, n.Message)
	}
	cfg.ConnConfig.RuntimeParams["client_min_messages"] = "debug1"
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "5000"
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), notices...)
	}
}

// constraintsNamed lists the table's constraints whose names start with the
// attach proof prefix.
func (f attachProofFixture) proofConstraints(t *testing.T, table string) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT conname FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND conname LIKE 'pgsprite_attach_%' ORDER BY conname`,
		pgx.Identifier{f.schema, table}.Sanitize())
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

// partitionBound is the table's FOR VALUES clause, empty when it is not a
// partition.
func (f attachProofFixture) partitionBound(t *testing.T, table string) string {
	t.Helper()
	var bound string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		"SELECT COALESCE(pg_get_expr(relpartbound, oid), '') FROM pg_class WHERE oid = $1::regclass",
		pgx.Identifier{f.schema, table}.Sanitize()).Scan(&bound))
	return bound
}

const events2027Bound = "FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')"

// With both proofs validated and the index in place, the step attaches the
// table scanning neither it nor the DEFAULT partition, and drops both proofs
// in the same transaction. A rerun finds the partition and runs nothing.
func TestAttachPartitionOnlineAttachesWithoutAScanAndDropsTheProofs(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		target := f.attachTarget(t)
		pool, notices := f.debugPool(t)

		tracker, err := progress.NewTracker(progress.WallClock{})
		require.NoError(t, err)
		var ran []string
		res, err := attachPartitionOnline(t.Context(), pool, target, attachPartitionBudget, time.Second, tracker,
			func(_ progress.Operation, sql string) { ran = append(ran, sql) })
		require.NoError(t, err)
		assert.Equal(t, attachPartitionResult{action: attachPartitionAttached, bound: events2027Bound}, res)
		assert.Equal(t, []string{attachEvents2027Unqualified, target.bound.dropSQL(), target.deflt.dropSQL()}, ran)
		requireNoScan(t, notices(), "events_2027", "events_default")
		assert.Equal(t, events2027Bound, f.partitionBound(t, "events_2027"))
		assert.Empty(t, f.proofConstraints(t, "events_2027"))
		assert.Empty(t, f.proofConstraints(t, "events_default"))

		ran = nil
		again, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil,
			func(_ progress.Operation, sql string) { ran = append(ran, sql) })
		require.NoError(t, err)
		assert.Equal(t, attachPartitionResult{action: attachPartitionFound, bound: events2027Bound}, again)
		assert.Empty(t, ran)
	})
}

// A parent without a DEFAULT partition needs only the table's proof.
func TestAttachPartitionOnlineWithoutADefaultPartition(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, strings.Replace(eventsAttachParent,
			"CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;\n\tINSERT INTO %[1]s.events VALUES (1, '2030-06-01');", "", 1))
		st, err := statement.ParseOne(attachEvents2027Unqualified)
		require.NoError(t, err)
		attach, _, err := statement.AttachedPartition(attachEvents2027Unqualified)
		require.NoError(t, err)
		parentOID := f.parentOIDOf(t, "events")
		check, _, err := deriveAttachBoundCheck(t.Context(), briefTx{pool: f.pool}, parentOID, attach.Bound)
		require.NoError(t, err)
		target := attachPartitionTarget{st: st, searchPath: AttemptSearchPath(f.schema, nil), parentOID: parentOID,
			bound: f.proofOn(t, attachProofBound, "events_2027", check)}
		_, err = ensureAttachProof(t.Context(), f.pool, target.bound, attachProofTestBudget, 0, nil, nil)
		require.NoError(t, err)
		pool, notices := f.debugPool(t)

		res, err := attachPartitionOnline(t.Context(), pool, target, attachPartitionBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, attachPartitionAttached, res.action)
		requireNoScan(t, notices(), "events_2027")
		assert.Empty(t, f.proofConstraints(t, "events_2027"))
	})
}

// Without a validated proof the ATTACH would scan under its locks, so the
// step refuses, typed, before it requests any lock, and the table stays
// standalone. The same holds when the parent's DEFAULT partition is not the
// one the run proved.
func TestAttachPartitionOnlineRefusesAnUnprovenAttach(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("the table's proof is missing", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsAttachParent)
			target := f.attachTarget(t, attachProofBound)
			_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachProofNotReady)
			assert.Empty(t, f.partitionBound(t, "events_2027"))
		})

		t.Run("the DEFAULT partition's proof is NOT VALID", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsAttachParent)
			target := f.attachTarget(t, attachProofDefault)
			f.exec(t, "ALTER TABLE %[1]s.events_default ADD CONSTRAINT "+
				pgx.Identifier{target.deflt.name()}.Sanitize()+" CHECK ("+target.deflt.expr+") NOT VALID")
			_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachProofNotReady)
			assert.Empty(t, f.partitionBound(t, "events_2027"))
		})

		t.Run("the run proved no DEFAULT partition and the parent has one", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsAttachParent)
			target := f.attachTarget(t)
			target.deflt = attachProof{}
			_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachDefaultPartitionChanged)
			assert.Empty(t, f.partitionBound(t, "events_2027"))
		})
	})
}

// A session holding the DEFAULT partition outlasts the lock budget: the
// attempt ends as a lock budget failure, rolled back, with the table
// standalone and both proofs still validated for the rerun.
func TestAttachPartitionOnlineEndsOnTheLockBudgetAndRollsBack(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		target := f.attachTarget(t)
		release, _ := f.holdLock(t, "events_default", "ROW EXCLUSIVE")
		defer release()

		_, err := attachPartitionOnline(t.Context(), f.pool, target,
			Budget{LockTimeout: 200 * time.Millisecond, StatementTimeout: 5 * time.Second}, 0, nil, nil)
		var budgetErr *BudgetError
		require.ErrorAs(t, err, &budgetErr)
		assert.Equal(t, CauseLock, budgetErr.Cause)
		assert.Empty(t, f.partitionBound(t, "events_2027"))
		assert.Len(t, f.proofConstraints(t, "events_2027"), 1)
		assert.Len(t, f.proofConstraints(t, "events_default"), 1)

		release()
		res, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, attachPartitionAttached, res.action)
	})
}

// A table someone else attached is found, and the proofs left on it and on
// the DEFAULT partition are dropped. A table attached to another parent, or
// a statement whose name resolves elsewhere, is not the table the run
// resolved.
func TestAttachPartitionOnlineFindsAnAttachedTableAndRefusesAMovedOne(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("attached by hand", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsAttachParent)
			target := f.attachTarget(t)
			f.exec(t, `ALTER TABLE %[1]s.events
				ATTACH PARTITION %[1]s.events_2027
				FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
			require.Len(t, f.proofConstraints(t, "events_2027"), 1)

			res, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, attachPartitionResult{action: attachPartitionFound, bound: events2027Bound, dropped: target.proofs()}, res)
			assert.Empty(t, f.proofConstraints(t, "events_2027"))
			assert.Empty(t, f.proofConstraints(t, "events_default"))
		})

		t.Run("attached to another parent", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsAttachParent)
			target := f.attachTarget(t)
			f.exec(t, `
				CREATE TABLE %[1]s.archive (
					id bigint NOT NULL,
					day date NOT NULL
				) PARTITION BY RANGE (day);
				ALTER TABLE %[1]s.archive
					ATTACH PARTITION %[1]s.events_2027
					FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
			_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, ErrInvariantViolation)
		})

		// The statement names events_2027 unqualified; a table of that
		// name earlier on the search path is not the one the run proved.
		t.Run("the statement's name resolves to another table", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsAttachParent)
			target := f.attachTarget(t)
			shadow := f.shadowSchema(t)
			_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`
				CREATE TABLE %s.events_2027 (
					id bigint NOT NULL,
					day date NOT NULL
				)`, pgx.Identifier{shadow}.Sanitize()))
			require.NoError(t, err)
			target.searchPath = append([]string{shadow}, target.searchPath...)
			_, err = attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, ErrInvariantViolation)
			assert.Empty(t, f.partitionBound(t, "events_2027"))
		})
	})
}

// After an attach that committed with its drops, a rerun has nothing to
// drop, so it neither waits for a reader of the DEFAULT partition, nor locks
// the DEFAULT partition by name: it finds the partition with a reader still
// holding the DEFAULT partition, and again once the DEFAULT partition is
// renamed.
func TestAttachPartitionOnlineRerunTouchesNeitherTableWhenTheProofsAreGone(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		target := f.attachTarget(t)
		res, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.NoError(t, err)
		require.Equal(t, attachPartitionAttached, res.action)
		want := attachPartitionResult{action: attachPartitionFound, bound: events2027Bound}

		release, _ := f.holdLock(t, "events_default", "ACCESS SHARE")
		// The rerun is given the full holder wait; the deadline, a third
		// of it, ends any rerun that waits on the reader.
		ctx, cancel := context.WithTimeout(t.Context(), DefaultHolderWait/3)
		defer cancel()
		again, err := attachPartitionOnline(ctx, f.pool, target, attachPartitionBudget, DefaultHolderWait, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, want, again)
		release()

		f.exec(t, `ALTER TABLE %[1]s.events_default RENAME TO events_other`)
		again, err = attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, want, again)
	})
}

// An interrupted DETACH PARTITION ... CONCURRENTLY leaves events_2027 in
// pg_inherits under the parent, detach-pending: neither a partition the
// step finds, nor the standalone table it resolved. The step refuses it as
// not the table the run resolved, and drops neither proof. The parent has a
// DEFAULT partition, which DETACH ... CONCURRENTLY refuses, so the test marks
// the detach pending in the catalog, the state an interrupted detach leaves.
func TestAttachPartitionOnlineRefusesADetachPendingTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		target := f.attachTarget(t)
		f.exec(t, `ALTER TABLE %[1]s.events
			ATTACH PARTITION %[1]s.events_2027
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`)
		f.exec(t, `UPDATE pg_catalog.pg_inherits SET inhdetachpending = true
			WHERE inhrelid = '%[1]s.events_2027'::regclass`)

		_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.ErrorIs(t, err, ErrInvariantViolation)
		assert.Len(t, f.proofConstraints(t, "events_2027"), 1)
		assert.Len(t, f.proofConstraints(t, "events_default"), 1)
	})
}

// shadowSchema makes a second, empty schema beside the fixture's.
func (f attachProofFixture) shadowSchema(t *testing.T) string {
	t.Helper()
	name := f.schema + "_shadow"
	_, err := f.pool.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()), "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	return name
}
