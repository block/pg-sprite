package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DDLEvent is the point in a DDL command at which an event trigger fires.
type DDLEvent string

const (
	// DDLCommandStart fires before the command runs: the objects it will
	// create do not exist yet, so a name can still be taken from under it.
	DDLCommandStart DDLEvent = "ddl_command_start"
	// DDLCommandEnd fires after the command has run but inside its
	// transaction: the objects it created exist and can be altered before
	// the caller sees the commit.
	DDLCommandEnd DDLEvent = "ddl_command_end"
)

// eventTriggerDrainTimeout bounds how long a trigger's drop waits for the
// transactions that may still hold the trigger in their event-trigger
// cache. Those are other tests' transactions, each bounded by its own
// statement_timeout; a wait this long means a transaction leaked.
const eventTriggerDrainTimeout = 30 * time.Second

// eventTriggerDrainPoll is the interval between checks for open transactions.
const eventTriggerDrainPoll = 20 * time.Millisecond

// RunDuringDDL installs an event trigger that executes sql from inside the
// next DDL command with the given tag whose text names schema.table, at
// the given event. It is the fault-injection seam for the windows a
// single-statement DDL leaves open — between a probe and the server's
// choice of names, or between a commit and the read that verifies it —
// made deterministic. The trigger is server-wide (event triggers are), so
// the schema qualifier scopes it to the test's own objects; the trigger
// and its function are dropped when the test ends.
func RunDuringDDL(t *testing.T, pool *pgxpool.Pool, event DDLEvent, tag, schema, table, sql string) {
	t.Helper()
	functionName := pgx.Identifier{schema, "run_during_ddl"}.Sanitize()
	triggerName := pgx.Identifier{schema + "_run_during_ddl"}.Sanitize()
	// The command text carries the qualified name followed by a space
	// (the column list or the next clause); the pattern escapes the
	// name's own LIKE metacharacters so an underscore in a schema or table
	// name matches only itself.
	pattern := "%" + escapeLikePattern(schema+"."+table) + " %"
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS event_trigger LANGUAGE plpgsql AS $body$
		BEGIN
			IF TG_TAG = %s AND current_query() LIKE %s THEN
				EXECUTE %s;
			END IF;
		END
		$body$;
		CREATE EVENT TRIGGER %s ON %s EXECUTE FUNCTION %s()`,
		functionName, quoteLiteral(tag), quoteLiteral(pattern), quoteLiteral(sql),
		triggerName, string(event), functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		assert.NoError(t, dropEventTrigger(ctx, pool, triggerName, functionName, eventTriggerDrainTimeout))
	})
}

// openTransactionsBeforeSQL counts the other sessions in this database whose
// current transaction began before the given instant.
const openTransactionsBeforeSQL = `SELECT pg_catalog.count(*)
  FROM pg_catalog.pg_stat_activity
 WHERE datname OPERATOR(pg_catalog.=) pg_catalog.current_database()
   AND pid OPERATOR(pg_catalog.<>) pg_catalog.pg_backend_pid()
   AND xact_start OPERATOR(pg_catalog.<) $1`

// dropEventTrigger drops the trigger, waits until no transaction that began
// before the drop is still open, and only then drops the function.
//
// Event triggers are cached per backend, and a backend refreshes that cache
// only when it accepts catalog invalidations — at transaction start and
// when it takes a lock. A transaction already open when the trigger is
// dropped keeps the trigger in its cache until then; ddl_command_start
// fires before any lock, so its next DDL statement still calls the
// trigger's function. Were the function gone by then, that statement —
// another test's, in another package sharing the database — would fail
// with "cache lookup failed for function". Keeping the function alive until
// every such transaction has ended makes the stale entry harmless: the
// function runs, its tag and query predicate do not match, and it returns.
// Transactions that begin after the drop see no trigger at all.
func dropEventTrigger(ctx context.Context, pool *pgxpool.Pool, triggerName, functionName string, drainTimeout time.Duration) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("drop event trigger %s: acquire connection: %w", triggerName, err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "DROP EVENT TRIGGER IF EXISTS "+triggerName); err != nil {
		return fmt.Errorf("drop event trigger %s: %w", triggerName, err)
	}
	// The drop has committed by the time this statement runs, so any
	// transaction that begins at or after this instant accepted the
	// invalidation at its start and never sees the trigger.
	var dropped time.Time
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.clock_timestamp()").Scan(&dropped); err != nil {
		return fmt.Errorf("drop event trigger %s: read the drop instant: %w", triggerName, err)
	}
	deadline := time.Now().Add(drainTimeout)
	for {
		var open int
		if err := conn.QueryRow(ctx, openTransactionsBeforeSQL, dropped).Scan(&open); err != nil {
			return fmt.Errorf("drop event trigger %s: count open transactions: %w", triggerName, err)
		}
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("drop event trigger %s: %d transactions that began before the drop are still open after %s; the function %s is left in place",
				triggerName, open, drainTimeout, functionName)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drop event trigger %s: %w", triggerName, ctx.Err())
		case <-time.After(eventTriggerDrainPoll):
		}
	}
	if _, err := conn.Exec(ctx, "DROP FUNCTION IF EXISTS "+functionName+"()"); err != nil {
		return fmt.Errorf("drop event trigger function %s: %w", functionName, err)
	}
	return nil
}

// quoteLiteral renders s as a standard-conforming SQL string literal:
// backslashes are ordinary characters, so only the quote needs doubling.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// escapeLikePattern escapes the LIKE metacharacters in s with the default
// backslash escape so the result matches s and only s.
func escapeLikePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
