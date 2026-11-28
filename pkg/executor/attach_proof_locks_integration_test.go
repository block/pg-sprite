package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/progress"
)

// These tests run the proof step where another session holds its table, or
// changes the proof's constraint between the step's decisions: the step's
// lock waits stay bounded, a stopped step leaves what a rerun resumes, and
// each decision under the table's lock catches what changed since the last.

// poolWithLockTimeout opens a second pool on the fixture's server whose
// sessions run with the given lock_timeout.
func (f attachProofFixture) poolWithLockTimeout(t *testing.T, lockTimeout time.Duration) *pgxpool.Pool {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: f.url, LockTimeout: lockTimeout})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// holdLock opens a transaction that takes mode on the fixture's table and
// keeps it until the returned release runs, or the test ends. It returns
// the backend PID of the holding session too.
func (f attachProofFixture) holdLock(t *testing.T, table, mode string) (release func(), pid uint32) {
	t.Helper()
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{f.schema, table}.Sanitize()+" IN "+mode+" MODE")
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT pg_backend_pid()::oid").Scan(&pid))
	var once sync.Once
	release = func() {
		once.Do(func() {
			assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context())))
		})
	}
	t.Cleanup(release)
	return release, pid
}

// A reader holds the new table past the ADD's lock_timeout: the ADD fails on
// its lock budget, and nothing is added.
func TestEnsureAttachProofAddStopsOnItsLockBudget(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		pool := f.poolWithLockTimeout(t, 200*time.Millisecond)
		f.holdLock(t, "events_2027_01", "ACCESS SHARE")

		_, err = ensureAttachProof(t.Context(), pool, p, attachProofTestBudget, 0, nil, nil)
		var budget *BudgetError
		require.ErrorAs(t, err, &budget)
		assert.Equal(t, CauseLock, budget.Cause)
		_, exists := f.proofValidated(t, p)
		assert.False(t, exists, "nothing was added")
	})
}

// A reader holds the new table for a second, five times the ADD's
// lock_timeout. With a holder wait the step waits outside the lock queue,
// and progress names the reader it waits for. Once the reader ends, the
// step adds and validates the CHECK, and the wait leaves progress.
func TestEnsureAttachProofWaitsOutALongReaderBeforeItsAdd(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		pool := f.poolWithLockTimeout(t, 200*time.Millisecond)
		release, readerPID := f.holdLock(t, "events_2027_01", "ACCESS SHARE")
		tracker, err := progress.NewTracker(progress.WallClock{})
		require.NoError(t, err)

		type outcome struct {
			res attachProofResult
			err error
		}
		done := make(chan outcome, 1)
		var wg sync.WaitGroup
		defer wg.Wait()
		wg.Go(func() {
			res, err := ensureAttachProof(t.Context(), pool, p, attachProofTestBudget, 10*time.Second, tracker, nil)
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
		assert.Equal(t, readerPID, wait.PID)
		assert.Equal(t, p.qualified(), wait.Table)
		assert.Equal(t, 10*time.Second, wait.Limit)

		release()
		got := <-done
		require.NoError(t, got.err)
		assert.Equal(t, attachProofAdded, got.res.action)
		snap, err := tracker.Progress(t.Context())
		require.NoError(t, err)
		assert.Nil(t, snap.Detail.LockHolderWait, "the wait leaves progress once it ends")
	})
}

// A session holds SHARE UPDATE EXCLUSIVE on the table past the VALIDATE's
// lock budget: the VALIDATE fails on that budget and rolls back, the CHECK
// the ADD committed stays NOT VALID, and the next call validates it.
func TestEnsureAttachProofValidateStoppedOnItsLockBudgetResumes(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		_, err = f.pool.Exec(t.Context(), p.addSQL())
		require.NoError(t, err)
		release, _ := f.holdLock(t, "events_2027_01", "SHARE UPDATE EXCLUSIVE")

		short := ValidateBudget{LockTimeout: 200 * time.Millisecond, Overall: time.Minute}
		_, err = ensureAttachProof(t.Context(), f.pool, p, short, 0, nil, nil)
		var budget *BudgetError
		require.ErrorAs(t, err, &budget)
		assert.Equal(t, CauseLock, budget.Cause)
		validated, exists := f.proofValidated(t, p)
		assert.True(t, exists && !validated, "the CHECK stays NOT VALID")

		release()
		res, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, attachProofValidated, res.action)
	})
}

// A validated CHECK under the proof's name lands after the step's first
// read and before its ADD takes the table's lock. Deciding again under the
// lock, the step takes it as the proof and adds nothing.
func TestEnsureAttachProofTakesACheckThatLandedBeforeItsAdd(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)

		res, err := ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil,
			func(op progress.Operation, _ string) {
				if op == progress.OperationBrief {
					f.prove(t, "events_2027_01", p.name(), p.expr)
				}
			})
		require.NoError(t, err)
		assert.Equal(t, attachProofFound, res.action)
		var oid uint32
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT oid FROM pg_catalog.pg_constraint
			WHERE conrelid = $1 AND conname = $2`, p.tableOID, p.name()).Scan(&oid))
		assert.Equal(t, oid, res.constraintOID, "the CHECK the other session added")
	})
}

// The CHECK the step added is dropped, or replaced under the same name,
// before its VALIDATE takes the table's lock. Deciding again under the
// lock, the step finds that the constraint it added is gone and stops
// without validating anything.
func TestEnsureAttachProofRefusesACheckChangedBeforeItsValidate(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		t.Run("dropped", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsWithDefault)
			check, _, err := f.derive(t, attachJanuary)
			require.NoError(t, err)
			p := f.proofOn(t, attachProofBound, "events_2027_01", check)

			_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil,
				func(op progress.Operation, _ string) {
					if op == progress.OperationValidate {
						_, err := f.pool.Exec(t.Context(), p.dropSQL())
						require.NoError(t, err)
					}
				})
			require.ErrorIs(t, err, ErrInvariantViolation)
			_, exists := f.proofValidated(t, p)
			assert.False(t, exists, "nothing is added back")
		})
		t.Run("replaced", func(t *testing.T) {
			f := newFixture(t)
			f.exec(t, eventsWithDefault)
			check, _, err := f.derive(t, attachJanuary)
			require.NoError(t, err)
			p := f.proofOn(t, attachProofBound, "events_2027_01", check)

			_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil,
				func(op progress.Operation, _ string) {
					if op == progress.OperationValidate {
						_, err := f.pool.Exec(t.Context(), p.dropSQL())
						require.NoError(t, err)
						_, err = f.pool.Exec(t.Context(), p.addSQL())
						require.NoError(t, err)
					}
				})
			require.ErrorIs(t, err, ErrInvariantViolation)
			validated, exists := f.proofValidated(t, p)
			assert.True(t, exists && !validated, "the replacement is left NOT VALID")
		})
	})
}

// A reader holds the table for a second, five times the DROP's
// lock_timeout. With a holder wait the drop waits outside the lock queue
// until the reader ends, then takes the CHECK off.
func TestDropAttachProofWaitsOutALongReader(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, eventsWithDefault)
		check, _, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofBound, "events_2027_01", check)
		_, err = f.pool.Exec(t.Context(), p.addSQL())
		require.NoError(t, err)
		pool := f.poolWithLockTimeout(t, 200*time.Millisecond)
		release, _ := f.holdLock(t, "events_2027_01", "ACCESS SHARE")
		var wg sync.WaitGroup
		defer wg.Wait()
		wg.Go(func() {
			// The reader's transaction: held for a second, then ended.
			time.Sleep(time.Second)
			release()
		})

		require.NoError(t, dropAttachProof(t.Context(), pool, p, 10*time.Second, nil))
		_, exists := f.proofValidated(t, p)
		assert.False(t, exists)
	})
}

// A DEFAULT partition that is itself partitioned is a valid schema the step
// does not prove: it is refused typed, not as an engine breach, and nothing
// is added.
func TestEnsureAttachProofRefusesAPartitionedTable(t *testing.T) {
	withAttachProofServer(t, func(t *testing.T, newFixture func(t *testing.T) attachProofFixture) {
		f := newFixture(t)
		f.exec(t, `
			CREATE TABLE %[1]s.events (
				id bigint NOT NULL,
				region text,
				day date
			) PARTITION BY RANGE (day);
			CREATE TABLE %[1]s.events_default PARTITION OF %[1]s.events DEFAULT
				PARTITION BY LIST (region);
			CREATE TABLE %[1]s.events_default_eu PARTITION OF %[1]s.events_default
				FOR VALUES IN ('eu');
			CREATE TABLE %[1]s.events_2027_01 (
				id bigint NOT NULL,
				region text,
				day date
			)`)
		_, exclusion, err := f.derive(t, attachJanuary)
		require.NoError(t, err)
		p := f.proofOn(t, attachProofDefault, "events_default", exclusion)

		_, err = ensureAttachProof(t.Context(), f.pool, p, attachProofTestBudget, 0, nil, nil)
		require.ErrorIs(t, err, errAttachProofNotPlainTable)
		assert.False(t, errors.Is(err, ErrInvariantViolation), "a partitioned table is not an engine breach")
		_, exists := f.proofValidated(t, p)
		assert.False(t, exists, "nothing was added")
	})
}
