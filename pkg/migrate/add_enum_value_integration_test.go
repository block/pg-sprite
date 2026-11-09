package migrate_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/verdict"
)

// enumFixture creates an enum type mood with the labels happy and sad, and
// a table people with a column of that type.
func enumFixture(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	serverURL := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TYPE %[1]s.mood AS ENUM ('happy', 'sad');
		CREATE TABLE %[1]s.people (
			id   int PRIMARY KEY,
			mood %[1]s.mood NOT NULL
		);
		INSERT INTO %[1]s.people VALUES (1, 'happy')`, schema))
	require.NoError(t, err)
	return pool, schema, serverURL
}

// enumLabels returns the labels of schema.mood in their sort order.
func enumLabels(t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT enumlabel FROM pg_enum
		WHERE enumtypid = to_regtype($1)
		ORDER BY enumsortorder`, pgx.Identifier{schema, "mood"}.Sanitize())
	require.NoError(t, err)
	labels, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return labels
}

// holdTransaction opens a transaction on pool that runs sql and stays open
// until the test ends, holding every lock sql takes.
func holdTransaction(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) })
	_, err = tx.Exec(t.Context(), sql)
	require.NoError(t, err)
}

// An operator adds a label to an enum while another session holds the
// strongest lock on a table that uses the type. Adding the label locks only
// the type, so it commits without waiting for that session, in the position
// the statement asks for, and the verdict names no table.
func TestAddEnumValueRunsWithoutLockingATableThatUsesTheType(t *testing.T) {
	pool, schema, _ := enumFixture(t)
	holdTransaction(t, pool, fmt.Sprintf("LOCK TABLE %s.people IN ACCESS EXCLUSIVE MODE", schema))
	opts := runOptions()
	opts.Retry = executor.RetryPolicy{MaxAttempts: 1, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}

	sql := fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'calm' AFTER 'happy'", schema)
	v, err := migrate.Run(t.Context(), pool, parseOne(t, sql), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	assert.Empty(t, v.Table, "the statement names a type, not a table")
	assert.Contains(t, v.Detail, schema+".mood carries the value, and no table was locked")
	assert.Equal(t, []string{"happy", "calm", "sad"}, enumLabels(t, pool, schema))
}

// An unqualified type name resolves on the session search_path, as the
// statement does when it runs, and IF NOT EXISTS for a label the type
// already carries runs as a no-op.
func TestAddEnumValueResolvesAnUnqualifiedTypeOnTheSearchPath(t *testing.T) {
	pool, schema, serverURL := enumFixture(t)
	pc, err := pgxpool.ParseConfig(serverURL)
	require.NoError(t, err)
	pc.ConnConfig.RuntimeParams["search_path"] = schema + ", public"
	scoped, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(scoped.Close)

	for _, sql := range []string{
		"ALTER TYPE mood ADD VALUE 'calm'",
		"ALTER TYPE mood ADD VALUE IF NOT EXISTS 'calm'",
	} {
		v, err := migrate.Run(t.Context(), scoped, parseOne(t, sql), runOptions())
		require.NoError(t, err, sql)
		assert.Equal(t, verdict.OutcomeExecuted, v.Outcome, v.Detail)
	}
	assert.Equal(t, []string{"happy", "sad", "calm"}, enumLabels(t, pool, schema))
}

// A name that is not an enum type, and an acknowledgement that names a
// table, stop the run before anything executes.
func TestAddEnumValueStopsBeforeExecutingWhatItCannotAdd(t *testing.T) {
	pool, schema, _ := enumFixture(t)
	for _, tc := range []struct {
		name string
		sql  string
		opts func(*migrate.Options)
		err  error
		msg  string
	}{
		{
			name: "the type does not exist",
			sql:  "ALTER TYPE %s.missing ADD VALUE 'calm'",
			err:  preflight.ErrEnumTypeNotFound,
			msg:  "no type",
		},
		{
			name: "the type is a table's row type",
			sql:  "ALTER TYPE %s.people ADD VALUE 'calm'",
			err:  preflight.ErrEnumTypeNotFound,
			msg:  ".people is not an enum type",
		},
		{
			name: "force names a table",
			sql:  "ALTER TYPE %s.mood ADD VALUE 'calm'",
			opts: func(o *migrate.Options) { o.Force = schema + ".people" },
			err:  migrate.ErrEnumValueAcknowledgement,
			msg:  "force names a table",
		},
		{
			name: "allow-destructive names a table",
			sql:  "ALTER TYPE %s.mood ADD VALUE 'calm'",
			opts: func(o *migrate.Options) { o.AllowDestructive = schema + ".people" },
			err:  migrate.ErrEnumValueAcknowledgement,
			msg:  "discards nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := runOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf(tc.sql, schema)), opts)
			require.ErrorIs(t, err, tc.err)
			assert.ErrorContains(t, err, tc.msg)
			assert.Equal(t, verdict.Verdict{}, v, "the run stopped before a verdict")
			assert.Equal(t, []string{"happy", "sad"}, enumLabels(t, pool, schema), "nothing was added")
		})
	}
}

// enumRole creates a login role that does not own mood and returns a pool
// connected as it, with USAGE on the fixture's schema when usage is set.
func enumRole(t *testing.T, pool *pgxpool.Pool, schema, serverURL string, usage bool) (*pgxpool.Pool, string) {
	t.Helper()
	const password = "enum-value-password"
	role := testutil.NewRole(t, pool, "LOGIN PASSWORD '"+password+"'")
	if usage {
		_, err := pool.Exec(t.Context(), fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", schema, pgx.Identifier{role}.Sanitize()))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := pool.Exec(context.WithoutCancel(t.Context()),
				fmt.Sprintf("REVOKE USAGE ON SCHEMA %s FROM %s", schema, pgx.Identifier{role}.Sanitize()))
			if err != nil {
				t.Logf("revoke schema usage from %s: %v", role, err)
			}
		})
	}
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	engine, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: u.String()})
	require.NoError(t, err)
	t.Cleanup(engine.Close)
	return engine, role
}

// ALTER TYPE needs membership in the type's owning role. A role without it
// is refused before the statement runs, with the grant that fixes it and a
// hint that transfers the type instead.
func TestAddEnumValueRefusesARoleThatDoesNotOwnTheType(t *testing.T) {
	pool, schema, serverURL := enumFixture(t)
	engine, role := enumRole(t, pool, schema, serverURL, true)

	v, err := migrate.Run(t.Context(), engine, parseOne(t, fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'calm'", schema)), runOptions())
	require.NoError(t, err, "a missing grant is a refusal, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
	assert.Contains(t, v.Detail, "pg_has_role("+role+", ")
	assert.Contains(t, v.Detail, fmt.Sprintf("transfer the type instead: ALTER TYPE %q.\"mood\" OWNER TO <role>", schema))
	assert.Equal(t, []string{"happy", "sad"}, enumLabels(t, pool, schema), "nothing was added")
}

// A qualified type name resolves only through a schema the role has USAGE
// on. A role without it is refused before the name is resolved, with the
// schema grant, rather than failing on the server's permission error.
func TestAddEnumValueRefusesARoleWithoutUsageOnTheTypesSchema(t *testing.T) {
	pool, schema, serverURL := enumFixture(t)
	engine, role := enumRole(t, pool, schema, serverURL, false)

	v, err := migrate.Run(t.Context(), engine, parseOne(t, fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'calm'", schema)), runOptions())
	require.NoError(t, err, "a missing grant is a refusal, not an error")
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
	assert.Contains(t, v.Detail, fmt.Sprintf("has_schema_privilege(%s, %s, 'USAGE')", role, schema))
	assert.Contains(t, v.Detail, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", pgx.Identifier{schema}.Sanitize(), pgx.Identifier{role}.Sanitize()))
	assert.Equal(t, []string{"happy", "sad"}, enumLabels(t, pool, schema), "nothing was added")
}

// RENAME VALUE ” parses with the same empty old label ADD VALUE has, and
// the server would rename the type's empty label. It is refused as a
// statement the front door does not take, and the type keeps its labels.
func TestAddEnumValueRefusesARenameOfTheEmptyLabel(t *testing.T) {
	pool, schema, _ := enumFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE ''", schema))
	require.NoError(t, err)

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf("ALTER TYPE %s.mood RENAME VALUE '' TO 'blue'", schema)), runOptions())
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, []string{"happy", "sad", ""}, enumLabels(t, pool, schema), "nothing was renamed")
}

// Adding a label waits only for another session altering the same type.
// That wait is bounded by the lock budget, so the run is refused as
// budget-exceeded once its attempts run out, and adds nothing.
func TestAddEnumValueIsBoundedWhileAnotherSessionAltersTheType(t *testing.T) {
	pool, schema, _ := enumFixture(t)
	holdTransaction(t, pool, fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'held'", schema))
	opts := runOptions()
	opts.Budget.Brief.LockTimeout = 200 * time.Millisecond
	opts.Retry = executor.RetryPolicy{MaxAttempts: 2, InitialBackoff: 50 * time.Millisecond, MaxBackoff: 100 * time.Millisecond}

	v, err := migrate.Run(t.Context(), pool, parseOne(t, fmt.Sprintf("ALTER TYPE %s.mood ADD VALUE 'calm'", schema)), opts)
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonBudgetExceeded, v.Reason)
	assert.Equal(t, verdict.CauseLockBudget, v.Cause)
	assert.Equal(t, 2, v.Attempts)
	assert.Equal(t, []string{"happy", "sad"}, enumLabels(t, pool, schema), "nothing was added")
}
