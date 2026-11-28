package executor

import (
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the attach step's readiness against the server: an ATTACH
// that would scan, build an index, or create and validate a foreign key
// under its locks is refused, typed, before any lock is requested, and the
// tables the ATTACH reaches through foreign keys are waited on like the two
// it takes ACCESS EXCLUSIVE on.

// eventsWithAccountsParent is eventsAttachParent with a foreign key: the
// parent's account_id references accounts. events_2027 numbers its columns
// in another order than the parent, as the attach allows, and has no
// foreign key yet; each test gives it the one it needs.
const eventsWithAccountsParent = `
	CREATE TABLE %[1]s.accounts (
		id bigint PRIMARY KEY
	);
	INSERT INTO %[1]s.accounts VALUES (1), (2), (3);
	CREATE TABLE %[1]s.events (
		id bigint NOT NULL,
		day date NOT NULL,
		account_id bigint REFERENCES %[1]s.accounts (id)
	) PARTITION BY RANGE (day);
	CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;
	INSERT INTO %[1]s.events VALUES (1, '2030-06-01', 1);
	CREATE TABLE %[1]s.events_2027 (
		account_id bigint,
		id bigint NOT NULL,
		day date NOT NULL
	);
	INSERT INTO %[1]s.events_2027 VALUES (2, 2, '2027-03-01'), (3, 3, '2027-09-01')`

// events2027AccountsFK gives events_2027 the parent's foreign key,
// validated.
const events2027AccountsFK = `
	ALTER TABLE %[1]s.events_2027
		ADD CONSTRAINT events_2027_account_id_fkey
		FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`

// foreignKeyOf reads the OID of the table's foreign key under name, and the
// OID of the parent's foreign key it is attached to, 0 for none.
func (f attachProofFixture) foreignKeyOf(t *testing.T, table, name string) (oid, parentOID uint32) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT oid, conparentid FROM pg_catalog.pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2 AND contype = 'f'`,
		pgx.Identifier{f.schema, table}.Sanitize(), name).Scan(&oid, &parentOID))
	return oid, parentOID
}

// A reader holds the DEFAULT partition, so the ACCESS EXCLUSIVE the step
// would request there ends on the lock budget, while the step's catalog
// reads, which take at most ACCESS SHARE, do not wait on it. The table's
// proof is missing: the step refuses from its first read, typed and not as
// a budget failure, having requested no lock.
func TestAttachPartitionOnlineRefusesBeforeRequestingALock(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		target := f.attachTarget(t, attachProofBound)
		release, _ := f.holdLock(t, "events_default", "ACCESS SHARE")
		defer release()

		_, err := attachPartitionOnline(t.Context(), f.pool, target,
			Budget{LockTimeout: 200 * time.Millisecond, StatementTimeout: 5 * time.Second}, 0, nil, nil)
		require.ErrorIs(t, err, errAttachProofNotReady)
		var budgetErr *BudgetError
		assert.NotErrorAs(t, err, &budgetErr)
	})
}

// events_2027 lacks the index the parent's events_id_idx needs, which the
// ATTACH would build under ACCESS EXCLUSIVE on the table and the DEFAULT
// partition, bounded only by the statement budget. The step refuses it from
// its first read, before any lock: the reader holding the DEFAULT
// partition does not turn it into a budget failure. The table stays
// standalone, and both proofs stay for the rerun after the index step.
func TestAttachPartitionOnlineRefusesAMissingIndexBeforeAnyLock(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsAttachParent)
		f.exec(t, `DROP INDEX %[1]s.events_2027_id_idx`)
		target := f.attachTarget(t)
		release, _ := f.holdLock(t, "events_default", "ACCESS SHARE")
		defer release()

		_, err := attachPartitionOnline(t.Context(), f.pool, target,
			Budget{LockTimeout: 200 * time.Millisecond, StatementTimeout: 5 * time.Second}, 0, nil, nil)
		require.ErrorIs(t, err, errAttachIndexNotReady)
		var budgetErr *BudgetError
		assert.NotErrorAs(t, err, &budgetErr)
		assert.Empty(t, f.partitionBound(t, "events_2027"))
		assert.Len(t, f.proofConstraints(t, "events_2027"), 1)
		assert.Len(t, f.proofConstraints(t, "events_default"), 1)
	})
}

// Without a validated foreign key of the parent's definition on the table,
// the ATTACH would create one, or validate a NOT VALID one, and scan the
// table for it under its locks. The step refuses both, typed, and the table
// stays standalone.
func TestAttachPartitionOnlineRefusesATableWithoutTheParentsForeignKey(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("no foreign key", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsWithAccountsParent)
			target := f.attachTarget(t)
			_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachForeignKeyNotReady)
			assert.Empty(t, f.partitionBound(t, "events_2027"))
		})

		t.Run("a NOT VALID foreign key", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsWithAccountsParent)
			f.exec(t, events2027AccountsFK+" NOT VALID")
			target := f.attachTarget(t)
			_, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
			require.ErrorIs(t, err, errAttachForeignKeyNotReady)
			assert.Empty(t, f.partitionBound(t, "events_2027"))
		})
	})
}

// With the parent's foreign key validated on the table, under its own column
// numbering, the ATTACH adopts it: the table keeps the same constraint, now
// attached to the parent's, and nothing is created in its place.
func TestAttachPartitionOnlineAdoptsTheTablesForeignKey(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithAccountsParent)
		f.exec(t, events2027AccountsFK)
		before, _ := f.foreignKeyOf(t, "events_2027", "events_2027_account_id_fkey")
		target := f.attachTarget(t)

		res, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, attachPartitionAttached, res.action)
		after, parentFK := f.foreignKeyOf(t, "events_2027", "events_2027_account_id_fkey")
		assert.Equal(t, before, after)
		assert.NotZero(t, parentFK, "the table's foreign key is attached to the parent's")
	})
}

// A writer holds accounts, which the parent's foreign key references. The
// ATTACH takes SHARE ROW EXCLUSIVE on accounts after ACCESS EXCLUSIVE on the
// table and the DEFAULT partition, so the step waits outside the lock queue
// while the writer holds it, and progress names the writer, rather than
// queue the ATTACH behind it holding the other two. Once the writer ends,
// the step attaches.
func TestAttachPartitionOnlineWaitsOutAWriterOnTheReferencedTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithAccountsParent)
		f.exec(t, events2027AccountsFK)
		f.attachWaitsOutAWriterOn(t, f.attachTarget(t), "accounts")
	})
}

// A writer holds event_notes, whose foreign key references the parent. The
// ATTACH takes SHARE ROW EXCLUSIVE on event_notes to give the new partition
// the foreign key's triggers, so the step waits the writer out the same way.
func TestAttachPartitionOnlineWaitsOutAWriterOnAReferencingTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				day date NOT NULL,
				PRIMARY KEY (id, day)
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT;
			CREATE TABLE %[1]s.events_2027 (
				id bigint NOT NULL,
				day date NOT NULL,
				PRIMARY KEY (id, day)
			);
			INSERT INTO %[1]s.events_2027 VALUES (2, '2027-03-01');
			CREATE TABLE %[1]s.event_notes (
				event_id bigint NOT NULL,
				event_day date NOT NULL,
				note text,
				FOREIGN KEY (event_id, event_day) REFERENCES %[1]s.events (id, day)
			)`)
		f.attachWaitsOutAWriterOn(t, f.attachTarget(t), "event_notes")
	})
}

// attachWaitsOutAWriterOn holds a writer's lock on table, runs the step with
// a holder wait, and requires progress to name the writer while the step
// waits, then the step to attach once the writer ends.
func (f attachProofFixture) attachWaitsOutAWriterOn(t *testing.T, target attachPartitionTarget, table string) {
	t.Helper()
	release, writerPID := f.holdLock(t, table, "ROW EXCLUSIVE")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	type outcome struct {
		res attachPartitionResult
		err error
	}
	done := make(chan outcome, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		res, err := attachPartitionOnline(t.Context(), f.pool, target, attachPartitionBudget, 10*time.Second, tracker, nil)
		done <- outcome{res, err}
	})

	var wait *progress.LockHolderWait
	reportDeadline := time.Now().Add(5 * time.Second)
	for wait == nil && time.Now().Before(reportDeadline) {
		snap, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		wait = snap.Detail.LockHolderWait
		if wait == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	require.NotNil(t, wait, "progress reports the wait")
	assert.Equal(t, writerPID, wait.PID)
	assert.Equal(t, qualifiedName(f.schema, table), wait.Table)

	release()
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, attachPartitionAttached, got.res.action)
}
