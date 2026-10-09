package testutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TriggerEvent is the event an event trigger fires on: a point in a DDL
// command, or a session's login.
type TriggerEvent string

const (
	// DDLCommandStart fires before the command runs: the objects it will
	// create do not exist yet, so a name can still be taken from under it.
	DDLCommandStart TriggerEvent = "ddl_command_start"
	// DDLCommandEnd fires after the command has run but inside its
	// transaction: the objects it created exist and can be altered before
	// the caller sees the commit.
	DDLCommandEnd TriggerEvent = "ddl_command_end"
	// Login fires once a session has authenticated and its startup
	// parameters have been applied, so what it does to the session
	// outranks them. Login triggers exist from PostgreSQL 17.
	Login TriggerEvent = "login"
)

// eventTriggerDrainTimeout bounds how long a trigger's drop waits for the
// transactions that may still hold the trigger in their event-trigger
// cache. Those are other tests' transactions, bounded by the statements
// they run; a wait this long means a transaction was left open.
const eventTriggerDrainTimeout = 30 * time.Second

// eventTriggerDrainPoll is the interval between checks for open transactions.
const eventTriggerDrainPoll = 20 * time.Millisecond

// errEventTriggerDrainTimedOut reports that transactions older than the
// trigger's drop were still open when the drain gave up, so the function
// was not dropped.
var errEventTriggerDrainTimedOut = errors.New("event trigger drain timed out")

// InstallEventTrigger creates the plpgsql event-trigger function
// schema.name() with the given body and an event trigger on event that
// calls it, and registers their drop for the end of the test. Event
// triggers are database-global, so every test that installs one goes
// through this: the drop keeps the function alive until every transaction
// that may still hold the trigger in its event-trigger cache has ended
// (see dropEventTrigger), which an inline DROP EVENT TRIGGER followed by
// the function's drop — or the schema's cascade — does not.
func InstallEventTrigger(t *testing.T, pool *pgxpool.Pool, event TriggerEvent, schema, name, body string) {
	t.Helper()
	functionName := pgx.Identifier{schema, name}.Sanitize()
	triggerName := pgx.Identifier{schema + "_" + name}.Sanitize()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS event_trigger LANGUAGE plpgsql AS $body$
		BEGIN
			%s
		END
		$body$;
		CREATE EVENT TRIGGER %s ON %s EXECUTE FUNCTION %s()`,
		functionName, body, triggerName, string(event), functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		err := dropEventTrigger(ctx, pool, triggerName, functionName, eventTriggerDrainTimeout)
		if errors.Is(err, errEventTriggerDrainTimedOut) {
			// Failing this test would not protect the transaction that is
			// still open; the function goes with the schema drop.
			t.Logf("%v", err)
			return
		}
		assert.NoError(t, err)
	})
}

// RunDuringDDL installs an event trigger that executes sql from inside the
// next DDL command with the given tag whose text names schema.table, at
// the given event. It is the fault-injection seam for the windows a
// single-statement DDL leaves open — between a probe and the server's
// choice of names, or between a commit and the read that verifies it —
// made deterministic. The trigger is server-wide (event triggers are), so
// the schema qualifier scopes it to the test's own objects; the trigger
// and its function are dropped when the test ends.
func RunDuringDDL(t *testing.T, pool *pgxpool.Pool, event TriggerEvent, tag, schema, table, sql string) {
	t.Helper()
	// The command text carries the qualified name followed by a space
	// (the column list or the next clause); the pattern escapes the
	// name's own LIKE metacharacters so an underscore in a schema or table
	// name matches only itself.
	pattern := "%" + escapeLikePattern(schema+"."+table) + " %"
	InstallEventTrigger(t, pool, event, schema, "run_during_ddl", fmt.Sprintf(`
			IF TG_TAG = %s AND current_query() LIKE %s THEN
				EXECUTE %s;
			END IF;`,
		quoteLiteral(tag), quoteLiteral(pattern), quoteLiteral(sql)))
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
// function runs, its predicate does not match, and it returns.
// Transactions that begin after the drop see no trigger at all.
func dropEventTrigger(ctx context.Context, pool *pgxpool.Pool, triggerName, functionName string, drainTimeout time.Duration) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("drop event trigger %s: acquire connection: %w", triggerName, err)
	}
	defer conn.Release()
	dropped, err := dropTrigger(ctx, conn, triggerName)
	if err != nil {
		return err
	}
	return dropFunctionAfterDrain(ctx, conn, triggerName, functionName, dropped, drainTimeout)
}

// dropTrigger drops the trigger and returns the server instant after the
// drop committed: any transaction that begins at or after it accepted the
// invalidation at its start and never sees the trigger.
func dropTrigger(ctx context.Context, conn *pgxpool.Conn, triggerName string) (dropped time.Time, err error) {
	if _, err := conn.Exec(ctx, "DROP EVENT TRIGGER IF EXISTS "+triggerName); err != nil {
		return time.Time{}, fmt.Errorf("drop event trigger %s: %w", triggerName, err)
	}
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.clock_timestamp()").Scan(&dropped); err != nil {
		return time.Time{}, fmt.Errorf("drop event trigger %s: read the drop instant: %w", triggerName, err)
	}
	return dropped, nil
}

// dropFunctionAfterDrain waits until no other transaction that began before
// dropped is open, then drops the function. Past drainTimeout it returns
// errEventTriggerDrainTimedOut and leaves the function in place.
func dropFunctionAfterDrain(ctx context.Context, conn *pgxpool.Conn, triggerName, functionName string, dropped time.Time, drainTimeout time.Duration) error {
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
			return fmt.Errorf("drop event trigger %s: giving up after %s with %d transactions that began before the drop still open; the function %s stays until its schema is dropped: %w",
				triggerName, drainTimeout, open, functionName, errEventTriggerDrainTimedOut)
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
