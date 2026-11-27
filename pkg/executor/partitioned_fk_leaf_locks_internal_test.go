package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/progress"
)

// The locks the leaf foreign key step takes, how long it waits for them,
// and what it does when the leaf changes between its read and its writes.

// openWriter begins a transaction that has inserted a row into the leaf,
// so it holds the leaf's ROW EXCLUSIVE until the test ends it.
func (f fkFixture) openWriter(t *testing.T, leaf string) pgx.Tx {
	t.Helper()
	writer, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := writer.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			assert.NoError(t, err)
		}
	})
	_, err = writer.Exec(t.Context(), fmt.Sprintf(
		`INSERT INTO %s.%s (id, account_id, created_at) VALUES (100000, 1, '2026-01-02')`,
		f.schema, pgx.Identifier{leaf}.Sanitize()))
	require.NoError(t, err)
	return writer
}

// relationLocks lists the modes tx holds on each named table of the
// fixture's schema, sorted.
func (f fkFixture) relationLocks(t *testing.T, tx pgx.Tx, tables ...string) map[string][]string {
	t.Helper()
	rows, err := tx.Query(t.Context(), `
		SELECT c.relname, l.mode
		  FROM pg_locks l
		  JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND l.granted
		   AND c.relnamespace = $1::regnamespace
		   AND c.relname = ANY($2)`, f.schema, tables)
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
		slices.Sort(modes)
	}
	return out
}

// assertHoldsAtMost asserts the modes include strongest and otherwise only
// ACCESS SHARE, which the statement takes to look the table up and which
// blocks no reader or writer.
func assertHoldsAtMost(t *testing.T, modes []string, strongest string) {
	t.Helper()
	assert.Contains(t, modes, strongest)
	for _, mode := range modes {
		assert.Contains(t, []string{"AccessShareLock", strongest}, mode)
	}
}

func isDeadlock(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "40P01"
}

// The modes each of the step's statements holds until its transaction
// ends: the ADD takes SHARE ROW EXCLUSIVE on the leaf and the referenced
// table and nothing stronger; the VALIDATE takes SHARE UPDATE EXCLUSIVE on
// the leaf, which lets writes go on, and ROW SHARE on the referenced table.
func TestLeafForeignKeyStatementsLockModes(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		t.Run("the ADD", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			node := f.leaf(t, "events_2026_01")
			tx, err := f.pool.Begin(t.Context())
			require.NoError(t, err)
			defer func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) }()

			_, err = tx.Exec(t.Context(), leafFKAddSQL(f.accountFK(), node, "jan_account_fk"))
			require.NoError(t, err)

			locks := f.relationLocks(t, tx, "events", "events_2026_01", "accounts")
			assertHoldsAtMost(t, locks["events_2026_01"], "ShareRowExclusiveLock")
			assertHoldsAtMost(t, locks["accounts"], "ShareRowExclusiveLock")
			assert.NotContains(t, locks, "events", "the parent is not locked")
		})

		t.Run("the VALIDATE", func(t *testing.T) {
			f := newFixture(t)
			f.events(t)
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
				    NOT VALID`)
			tx, err := f.pool.Begin(t.Context())
			require.NoError(t, err)
			defer func() { assert.NoError(t, tx.Rollback(context.WithoutCancel(t.Context()))) }()

			_, err = tx.Exec(t.Context(), leafFKValidateSQL(f.leaf(t, "events_2026_01"), "jan_account_fk"))
			require.NoError(t, err)

			locks := f.relationLocks(t, tx, "events", "events_2026_01", "accounts")
			assertHoldsAtMost(t, locks["events_2026_01"], "ShareUpdateExclusiveLock")
			assertHoldsAtMost(t, locks["accounts"], "RowShareLock")
			assert.NotContains(t, locks, "events", "the parent is not locked")
		})
	})
}

// A writer that holds the leaf and then writes to the referenced table is
// neither blocked by the step nor deadlocked with it: the step takes the
// leaf before the referenced table, the order the server's own ADD FOREIGN
// KEY takes them. The writer arrives once the ADD waits for the leaf, at
// once and after the server's deadlock_timeout, so either side would be the
// one aborted were the order reversed.
func TestEnsureLeafForeignKeyTakesTheLeafBeforeTheReferencedTable(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		for name, delay := range map[string]time.Duration{
			"the writer arrives at once":                    0,
			"the writer arrives after the deadlock timeout": 1500 * time.Millisecond,
		} {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				f.events(t)
				fk := f.resolve(t, f.accountFK())
				node := f.leaf(t, "events_2026_01")
				writer := f.openWriter(t, "events_2026_01")

				stepDone := make(chan error, 1)
				go func() {
					_, err := ensureLeafForeignKey(t.Context(), f.pool, fk, node, leafFKBudget, nil)
					stepDone <- err
				}()
				require.Eventually(t, func() bool {
					var waiting int
					err := f.pool.QueryRow(t.Context(), `
						SELECT count(*) FROM pg_stat_activity
						 WHERE datname = current_database() AND wait_event_type = 'Lock'
						   AND query LIKE 'ALTER TABLE%ADD CONSTRAINT%'`).Scan(&waiting)
					return err == nil && waiting > 0
				}, 10*time.Second, 20*time.Millisecond, "the ADD waits for the writer's leaf lock")
				time.Sleep(delay)

				_, writerErr := writer.Exec(t.Context(), fmt.Sprintf(`UPDATE %s.accounts SET id = id WHERE id = 1`, f.schema))
				require.NoError(t, writerErr, "the writer is not aborted")
				require.NoError(t, writer.Commit(t.Context()))
				var stepErr error
				select {
				case stepErr = <-stepDone:
				case <-time.After(20 * time.Second):
					require.FailNow(t, "the step did not finish")
				}
				assert.False(t, isDeadlock(stepErr), "step: %v", stepErr)
				require.NoError(t, stepErr)
			})
		}
	})
}

// A matching foreign key an operator adds after the step's read, before its
// ADD, is found under the ADD's lock and taken as the leaf's; the step adds
// none beside it.
func TestEnsureLeafForeignKeyFindsOneThatLandsAfterTheRead(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)
		started := func(op progress.Operation, _ string) {
			if op != progress.OperationBrief {
				return
			}
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_operator_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
		}

		res, err := ensureLeafForeignKey(t.Context(), f.pool, f.resolve(t, f.accountFK()), f.leaf(t, "events_2026_01"), leafFKBudget, started)

		require.NoError(t, err)
		fks := f.foreignKeys(t, "events_2026_01")
		assert.Equal(t, leafFKResult{action: leafFKFound, constraint: "jan_operator_fk", constraintOID: fks["jan_operator_fk"].oid}, res)
		assert.Len(t, fks, 1)
	})
}

// A matching foreign key that lands between the ADD and the VALIDATE makes
// two; the VALIDATE decides again under its lock and refuses, typed, rather
// than report one of them as the leaf's.
func TestEnsureLeafForeignKeyRefusesASecondThatLandsBeforeTheValidate(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)
		started := func(op progress.Operation, _ string) {
			if op != progress.OperationValidate {
				return
			}
			f.exec(t, `
				ALTER TABLE %[1]s.events_2026_01
				    ADD CONSTRAINT jan_operator_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)`)
		}

		_, err := ensureLeafForeignKey(t.Context(), f.pool, f.resolve(t, f.accountFK()), f.leaf(t, "events_2026_01"), leafFKBudget, started)

		require.ErrorIs(t, err, ErrPartitionedForeignKeyConflict)
		name := derivedPartitionIndexName("events_account_fk", f.leaf(t, "events_2026_01").oid)
		assert.False(t, f.foreignKeys(t, "events_2026_01")[name].validated, "the step's foreign key was not validated")
	})
}

// A writer holding the leaf makes the ADD wait; it gives up at the pool's
// lock budget, typed, and leaves no foreign key behind.
func TestEnsureLeafForeignKeyAddGivesUpAtTheLockBudgetBehindAWriter(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)
		writer := f.openWriter(t, "events_2026_01")

		_, err := f.ensure(t, f.accountFK(), "events_2026_01")

		budgetErr, ok := errors.AsType[*BudgetError](err)
		require.True(t, ok, "a lock budget error: %v", err)
		assert.Equal(t, CauseLock, budgetErr.Cause)
		require.NoError(t, writer.Rollback(t.Context()))
		assert.Empty(t, f.foreignKeys(t, "events_2026_01"))
	})
}

// A lock that conflicts with the VALIDATE's SHARE UPDATE EXCLUSIVE makes it
// wait; it gives up at the budget's LockTimeout, long before its Overall.
func TestEnsureLeafForeignKeyValidateGivesUpAtItsLockTimeout(t *testing.T) {
	withFKServer(t, func(t *testing.T, newFixture func(t *testing.T) fkFixture) {
		f := newFixture(t)
		f.events(t)
		f.exec(t, `
			ALTER TABLE %[1]s.events_2026_01
			    ADD CONSTRAINT jan_account_fk FOREIGN KEY (account_id) REFERENCES %[1]s.accounts (id)
			    NOT VALID`)
		holder, err := f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { assert.NoError(t, holder.Rollback(context.WithoutCancel(t.Context()))) }()
		_, err = holder.Exec(t.Context(), fmt.Sprintf(`LOCK TABLE %s.events_2026_01 IN SHARE UPDATE EXCLUSIVE MODE`, f.schema))
		require.NoError(t, err)

		budget := ValidateBudget{LockTimeout: 300 * time.Millisecond, Overall: 30 * time.Second}
		_, err = ensureLeafForeignKey(t.Context(), f.pool, f.resolve(t, f.accountFK()), f.leaf(t, "events_2026_01"), budget, nil)

		budgetErr, ok := errors.AsType[*BudgetError](err)
		require.True(t, ok, "a lock budget error: %v", err)
		assert.Equal(t, CauseLock, budgetErr.Cause)
		assert.False(t, f.foreignKeys(t, "events_2026_01")["jan_account_fk"].validated)
	})
}
