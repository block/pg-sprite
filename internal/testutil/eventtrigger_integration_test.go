package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// privatePool connects to a database of the test's own: the drain counts
// sessions in pg_stat_activity, which throwaway schemas do not isolate, and
// the trigger it drops is database-global, so no other package's backend
// may see it.
func privatePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), NewDatabase(t, StartPostgres(t)))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// installedEventTrigger is a trigger and function created the way
// InstallEventTrigger creates them, without its cleanup, so a test can
// drive the drop itself.
type installedEventTrigger struct {
	triggerName  string
	functionName string
	schema       string
}

func installEventTrigger(t *testing.T, pool *pgxpool.Pool) installedEventTrigger {
	t.Helper()
	schema := NewSchema(t, pool)
	installed := installedEventTrigger{
		triggerName:  pgx.Identifier{schema + "_run_during_ddl"}.Sanitize(),
		functionName: pgx.Identifier{schema, "run_during_ddl"}.Sanitize(),
		schema:       schema,
	}
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS event_trigger LANGUAGE plpgsql AS $body$
		BEGIN
		END
		$body$;
		CREATE EVENT TRIGGER %s ON ddl_command_start EXECUTE FUNCTION %s()`,
		installed.functionName, installed.triggerName, installed.functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := pool.Exec(ctx, "DROP EVENT TRIGGER IF EXISTS "+installed.triggerName)
		assert.NoError(t, err)
	})
	return installed
}

// functionExists reports whether the trigger's function is still in the
// catalog: the drain's observable effect is that it outlives the trigger
// while an older transaction is open.
func (i installedEventTrigger) functionExists(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_catalog.pg_proc p
			  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = $1 AND p.proname = 'run_during_ddl')`, i.schema).Scan(&exists))
	return exists
}

// beginTransaction opens a transaction on its own connection and returns
// the function that commits it. The transaction has run a statement, so the
// server records its xact_start.
func beginTransaction(t *testing.T, pool *pgxpool.Pool) (commit func()) {
	t.Helper()
	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	tx, err := conn.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "SELECT 1")
	require.NoError(t, err)
	var once sync.Once
	commit = func() {
		once.Do(func() {
			assert.NoError(t, tx.Commit(context.WithoutCancel(t.Context())))
			conn.Release()
		})
	}
	t.Cleanup(commit)
	return commit
}

// The function must survive the trigger for as long as a transaction that
// began before the drop is open: that transaction's backend may still hold
// the trigger in its event-trigger cache and call the function from its next
// DDL statement. Once the older transaction ends, the function goes.
func TestDropEventTriggerKeepsTheFunctionWhileAnOlderTransactionIsOpen(t *testing.T) {
	pool := privatePool(t)
	installed := installEventTrigger(t, pool)
	commit := beginTransaction(t, pool)

	dropReturned := make(chan error, 1)
	go func() {
		dropReturned <- dropEventTrigger(t.Context(), pool, installed.triggerName, installed.functionName, eventTriggerDrainTimeout)
	}()

	dropMustStillWait := time.After(300 * time.Millisecond)
	select {
	case err := <-dropReturned:
		t.Fatalf("dropEventTrigger returned (%v) while a transaction older than the drop was open", err)
	case <-dropMustStillWait:
	}
	assert.True(t, installed.functionExists(t, pool), "the function must outlive the trigger while the older transaction is open")

	commit()
	dropDeadline := time.After(10 * time.Second)
	select {
	case err := <-dropReturned:
		require.NoError(t, err)
	case <-dropDeadline:
		t.Fatal("dropEventTrigger did not return after the older transaction committed")
	}
	assert.False(t, installed.functionExists(t, pool), "the function is dropped once no older transaction remains")
}

// A transaction that never ends must not hang the test's cleanup: past the
// drain timeout the drop reports the leak with a typed error and leaves the
// function for the schema drop to remove.
func TestDropEventTriggerReportsAnOlderTransactionThatOutlivesTheDrain(t *testing.T) {
	pool := privatePool(t)
	installed := installEventTrigger(t, pool)
	beginTransaction(t, pool)

	err := dropEventTrigger(t.Context(), pool, installed.triggerName, installed.functionName, 200*time.Millisecond)
	require.ErrorIs(t, err, errEventTriggerDrainTimedOut)
	assert.True(t, installed.functionExists(t, pool), "a drop that gave up must not remove the function a cached trigger may still call")
}

// A transaction that begins after the drop never saw the trigger, so it
// does not hold the drop back: only transactions older than the drop do.
// The newer transaction is opened after the drop instant has been read, so
// the test does not depend on when the drain samples the clock.
func TestDropEventTriggerIgnoresTransactionsThatBeganAfterTheDrop(t *testing.T) {
	pool := privatePool(t)
	installed := installEventTrigger(t, pool)
	commitOlder := beginTransaction(t, pool)

	conn, err := pool.Acquire(t.Context())
	require.NoError(t, err)
	// The drain owns conn until it returns; release only after that, or a
	// failing deadline below would release a connection still in use.
	var drain sync.WaitGroup
	t.Cleanup(func() {
		drain.Wait()
		conn.Release()
	})
	dropped, err := dropTrigger(t.Context(), conn, installed.triggerName)
	require.NoError(t, err)
	beginTransaction(t, pool)

	drainReturned := make(chan error, 1)
	drain.Go(func() {
		drainReturned <- dropFunctionAfterDrain(t.Context(), conn, installed.triggerName, installed.functionName, dropped, eventTriggerDrainTimeout)
	})

	commitOlder()
	drainDeadline := time.After(10 * time.Second)
	select {
	case err := <-drainReturned:
		require.NoError(t, err)
	case <-drainDeadline:
		t.Fatal("the drain waited for a transaction that began after the drop")
	}
	assert.False(t, installed.functionExists(t, pool))
}
