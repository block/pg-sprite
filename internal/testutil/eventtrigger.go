package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
		_, cleanupErr := pool.Exec(ctx, fmt.Sprintf("DROP EVENT TRIGGER IF EXISTS %s", triggerName))
		assert.NoError(t, cleanupErr)
		_, cleanupErr = pool.Exec(ctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
		assert.NoError(t, cleanupErr)
	})
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
