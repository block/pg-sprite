package executor_test

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

const notesWithRowSecurity = `CREATE TABLE notes (
     id bigint PRIMARY KEY,
     author text NOT NULL,
     body text
 );
 CREATE INDEX notes_author_idx ON notes (author);
 ALTER TABLE notes ENABLE ROW LEVEL SECURITY;
 CREATE POLICY own_notes ON notes FOR SELECT USING (author = current_user);`

var createWithRowSecurityBudget = executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second}

func desiredRowSecurity(t *testing.T, sql string) statement.DesiredWithRowSecurity {
	t.Helper()
	d, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	return d
}

// The table, its index, and its row security are created by one run, and
// the report lists every statement it committed in execution order. A
// second comparison against the desired file finds nothing left to do.
func TestExecuteCreateWithRowSecurityCreatesTheTableAndItsPolicies(t *testing.T) {
	f := newCreateFixture(t, "notes")
	d := desiredRowSecurity(t, notesWithRowSecurity)

	report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr, d, createWithRowSecurityBudget)
	require.NoError(t, err)
	assert.True(t, report.TableCreated)
	require.Len(t, report.Statements, 5)
	assert.Contains(t, report.Statements[0], "CREATE TABLE "+f.schema+".notes")
	assert.Contains(t, report.Statements[1], "CREATE INDEX notes_author_idx ON "+f.schema+".notes")
	assert.Contains(t, report.Statements[2], "ENABLE ROW LEVEL SECURITY")
	assert.Contains(t, report.Statements[3], "NO FORCE ROW LEVEL SECURITY")
	assert.Contains(t, report.Statements[4], `CREATE POLICY "own_notes"`)

	live, err := schemadiff.Introspect(t.Context(), f.pool, f.schema, "notes")
	require.NoError(t, err)
	assert.True(t, live.RowSecurity.Enabled)
	require.Len(t, live.RowSecurity.Policies, 1)
	assert.Equal(t, "own_notes", live.RowSecurity.Policies[0].Name)
	wanted, err := schemadiff.IntrospectDesiredWithRowSecurity(t.Context(), f.pool, f.schema, d)
	require.NoError(t, err)
	_, err = schemadiff.DiffWithRowSecurity(f.schema, live, wanted)
	assert.NoError(t, err, "the created table must already match the desired file")
}

// A role whose default privileges grant every new table in the schema to a
// client — Supabase's anon role, for one — never sees the table without its
// row security. The run is held inside its transaction after the CREATE
// TABLE has run, and a session as the client finds no table. Once the run
// commits, the client holds the default grants and the row security
// decides what it can do: its insert is refused and its read sees no rows.
func TestExecuteCreateWithRowSecurityNeverExposesTheTableWithoutItsPolicies(t *testing.T) {
	f := newCreateFixture(t, "notes")
	client := testutil.NewRole(t, f.pool, "NOLOGIN")
	// The client holds default privileges in the schema, which block its
	// drop, so the schema goes first.
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()),
			"DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	schemaID, clientID := pgx.Identifier{f.schema}.Sanitize(), pgx.Identifier{client}.Sanitize()
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(`
		GRANT USAGE ON SCHEMA %[1]s TO %[2]s;
		ALTER DEFAULT PRIVILEGES IN SCHEMA %[1]s GRANT ALL ON TABLES TO %[2]s`, schemaID, clientID))
	require.NoError(t, err)

	// The run waits on an advisory lock the test holds, from inside its
	// CREATE POLICY: after the CREATE TABLE and before the commit.
	key := advisoryKey(f.schema)
	holder, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	defer holder.Release()
	_, err = holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", key)
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			_, err := holder.Exec(context.WithoutCancel(t.Context()), "SELECT pg_advisory_unlock($1)", key)
			assert.NoError(t, err)
		}
	}()
	testutil.InstallEventTrigger(t, f.pool, testutil.DDLCommandEnd, f.schema, "hold_policy", fmt.Sprintf(`
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_policy p ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid
         JOIN pg_class c ON c.oid = p.polrelid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND d.command_tag = 'CREATE POLICY'
     ) THEN
         PERFORM pg_advisory_xact_lock(%d);
     END IF;`, f.schema, key))

	budget := executor.Budget{LockTimeout: 20 * time.Second, StatementTimeout: 25 * time.Second}
	type result struct {
		report executor.RowSecurityReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
			desiredRowSecurity(t, notesWithRowSecurity), budget)
		done <- result{report, err}
	}()
	waitForAdvisoryWaiter(t, f.pool, key)

	asClient(t, f.pool, client, func(tx pgx.Tx) {
		var visible bool
		require.NoError(t, tx.QueryRow(t.Context(),
			"SELECT to_regclass($1) IS NOT NULL", f.schema+".notes").Scan(&visible))
		assert.False(t, visible, "the table must not be visible before its row security commits")
		_, err := tx.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.notes (id, author) VALUES (1, current_user)", schemaID))
		assert.Equal(t, "42P01", sqlstate(err), "the client cannot write to a table it cannot see")
	})

	_, err = holder.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", key)
	require.NoError(t, err)
	released = true
	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the create did not finish after the advisory lock was released")
	}
	require.NoError(t, r.err)
	require.Len(t, r.report.Statements, 5)

	_, err = f.pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.notes (id, author) VALUES (1, 'someone else')", schemaID))
	require.NoError(t, err)
	asClient(t, f.pool, client, func(tx pgx.Tx) {
		var granted bool
		require.NoError(t, tx.QueryRow(t.Context(),
			"SELECT has_table_privilege($1, 'INSERT')", f.schema+".notes").Scan(&granted))
		assert.True(t, granted, "the default privileges grant the client the new table")
		var rows int
		require.NoError(t, tx.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s.notes", schemaID)).Scan(&rows))
		assert.Zero(t, rows, "the policy hides rows the client did not write")
		_, err := tx.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.notes (id, author) VALUES (2, current_user)", schemaID))
		assert.Equal(t, "42501", sqlstate(err), "with no insert policy the client's insert is refused")
	})
}

// A failure after the CREATE TABLE has run — here a policy the server
// refuses — rolls the table back with everything else: no table, index, or
// policy is left behind, so a rerun starts from an absent table again.
func TestExecuteCreateWithRowSecurityRollsBackTheTableWhenAPolicyFails(t *testing.T) {
	f := newCreateFixture(t, "notes")
	testutil.InstallEventTrigger(t, f.pool, testutil.DDLCommandEnd, f.schema, "fail_policy", fmt.Sprintf(`
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_policy p ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid
         JOIN pg_class c ON c.oid = p.polrelid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND d.command_tag = 'CREATE POLICY'
     ) THEN
         RAISE EXCEPTION 'injected policy failure';
     END IF;`, f.schema))

	report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
		desiredRowSecurity(t, notesWithRowSecurity), createWithRowSecurityBudget)
	assert.Equal(t, "P0001", sqlstate(err))
	assert.Empty(t, report.Statements)
	for _, name := range []string{"notes", "notes_pkey", "notes_author_idx"} {
		assert.False(t, relationExists(t, f.pool, f.schema, name), name)
	}
	_, err = preflight.CheckTableAbsent(t.Context(), f.pool, f.schema, "notes")
	assert.NoError(t, err)
}

// A constraint index name an occupant takes after the probe makes the
// server choose another name. The run reads the names before committing,
// so the collision rolls back the whole create instead of leaving a table
// that owns the wrong name.
func TestExecuteCreateWithRowSecurityRollsBackANameTakenAfterTheProbe(t *testing.T) {
	f := newCreateFixture(t, "notes")
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other (v int)", f.schema))
	require.NoError(t, err)
	testutil.RunDuringDDL(t, f.pool, testutil.DDLCommandStart, "CREATE TABLE", f.schema, "notes",
		fmt.Sprintf("CREATE INDEX notes_pkey ON %s.other (v)", f.schema))

	report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
		desiredRowSecurity(t, notesWithRowSecurity), createWithRowSecurityBudget)
	require.ErrorIs(t, err, executor.ErrCreateCollision)
	assert.ErrorContains(t, err, `"notes_pkey1" instead of "notes_pkey"`)
	assert.Empty(t, report.Statements)
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))
	assert.False(t, relationExists(t, f.pool, f.schema, "notes_pkey1"))
}

// The preview runs the same transaction, readback included, and rolls it
// back. It lists exactly the statements the run then commits.
func TestPreviewCreateWithRowSecurityMatchesTheRunAndLeavesNothing(t *testing.T) {
	f := newCreateFixture(t, "notes")
	d := desiredRowSecurity(t, notesWithRowSecurity)

	preview, err := executor.PreviewCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr, d, createWithRowSecurityBudget)
	require.NoError(t, err)
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))

	report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr, d, createWithRowSecurityBudget)
	require.NoError(t, err)
	assert.Equal(t, preview.Statements, report.Statements)
}

// A preview of a missing table runs the create transaction on the target
// and rolls it back. No catalog change survives, but what a rollback cannot
// undo does: here an event trigger on the CREATE TABLE advances a sequence,
// and the sequence keeps the value it reached while the table stays absent.
func TestPreviewCreateWithRowSecurityRunsTheTargetDDL(t *testing.T) {
	f := newCreateFixture(t, "notes")
	audit := pgx.Identifier{f.schema, "ddl_audit"}.Sanitize()
	_, err := f.pool.Exec(t.Context(), "CREATE SEQUENCE "+audit)
	require.NoError(t, err)
	testutil.InstallEventTrigger(t, f.pool, testutil.DDLCommandEnd, f.schema, "audit_create", fmt.Sprintf(`
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_class c ON d.classid = 'pg_class'::regclass AND d.objid = c.oid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND c.relname = 'notes' AND d.command_tag = 'CREATE TABLE'
     ) THEN
         PERFORM nextval(format('%%I.ddl_audit', '%s'));
     END IF;`, f.schema, f.schema))

	_, err = executor.PreviewCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
		desiredRowSecurity(t, notesWithRowSecurity), createWithRowSecurityBudget)
	require.NoError(t, err)
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))
	var last int64
	var called bool
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT last_value, is_called FROM "+audit).Scan(&last, &called))
	assert.True(t, called, "the trigger ran on the preview's CREATE TABLE")
	assert.Equal(t, int64(1), last, "the sequence keeps the value the rolled-back preview advanced it to")
}

// With a create owner in the proof, the table, its relations, and its
// policies are all created as that role.
func TestExecuteCreateWithRowSecurityCreatesAsTheOwner(t *testing.T) {
	f, owner := newCreateOwnerFixture(t, "notes")

	_, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
		desiredRowSecurity(t, notesWithRowSecurity), createWithRowSecurityBudget)
	require.NoError(t, err)
	for _, relation := range []string{"notes", "notes_pkey", "notes_author_idx"} {
		assert.Equal(t, owner, relationOwner(t, f.pool, f.schema, relation), relation)
	}
	live, err := schemadiff.Introspect(t.Context(), f.pool, f.schema, "notes")
	require.NoError(t, err)
	assert.True(t, live.RowSecurity.Enabled)
	assert.Len(t, live.RowSecurity.Policies, 1)
}

// The proofs are checked at the point of use, as for ExecuteCreate.
func TestExecuteCreateWithRowSecurityRefusesProofsForAnotherTable(t *testing.T) {
	f := newCreateFixture(t, "other")
	_, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
		desiredRowSecurity(t, notesWithRowSecurity), createWithRowSecurityBudget)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.ErrorContains(t, err, "ST-7")
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))
}

func advisoryKey(schema string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(schema))
	return int64(h.Sum32() & 0x7fffffff)
}

// waitForAdvisoryWaiter polls until a session is waiting for the advisory
// lock on key.
func waitForAdvisoryWaiter(t *testing.T, pool *pgxpool.Pool, key int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting bool
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT EXISTS (
			  SELECT FROM pg_locks
			   WHERE locktype = 'advisory' AND classid = 0 AND objid = $1 AND NOT granted)`, key).Scan(&waiting))
		if waiting {
			return
		}
		require.True(t, time.Now().Before(deadline), "no session waited for advisory lock %d", key)
		time.Sleep(20 * time.Millisecond)
	}
}

// asClient runs fn in a transaction under role, and rolls it back.
func asClient(t *testing.T, pool *pgxpool.Pool, role string, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	fn(tx)
}

func sqlstate(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// A table shape the row security path does not support is refused before
// any target DDL, by the run and the preview alike, and nothing is created.
func TestCreateWithRowSecurityRefusesAnUnsupportedShapeBeforeCreating(t *testing.T) {
	f := newCreateFixture(t, "notes")
	d := desiredRowSecurity(t, `CREATE UNLOGGED TABLE notes (id bigint PRIMARY KEY);
 ALTER TABLE notes ENABLE ROW LEVEL SECURITY;`)

	_, err := executor.PreviewCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr, d, createWithRowSecurityBudget)
	require.ErrorIs(t, err, executor.ErrRowSecurityRefused)
	assert.ErrorContains(t, err, "admit the desired table")
	assert.NotErrorIs(t, err, executor.ErrInvariantViolation)
	report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr, d, createWithRowSecurityBudget)
	require.ErrorIs(t, err, executor.ErrRowSecurityRefused)
	assert.NotErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Empty(t, report.Statements)
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))
}

// An event trigger that hands the table to another role while its policy is
// created cannot make the run commit a table the creation role does not
// own: the owner is read after every statement, and the run rolls back.
func TestExecuteCreateWithRowSecurityRollsBackAnOwnerChangedByAPolicyTrigger(t *testing.T) {
	f := newCreateFixture(t, "notes")
	other := testutil.NewRole(t, f.pool, "NOLOGIN")
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()),
			"DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	testutil.InstallEventTrigger(t, f.pool, testutil.DDLCommandEnd, f.schema, "give_away", fmt.Sprintf(`
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_policy p ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid
         JOIN pg_class c ON c.oid = p.polrelid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND d.command_tag = 'CREATE POLICY'
     ) THEN
         EXECUTE format('ALTER TABLE %%I.notes OWNER TO %%I', '%s', '%s');
     END IF;`, f.schema, f.schema, other))

	report, err := executor.ExecuteCreateWithRowSecurity(t.Context(), f.pool, f.at, f.cr,
		desiredRowSecurity(t, notesWithRowSecurity), createWithRowSecurityBudget)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.ErrorContains(t, err, "ST-9")
	assert.Empty(t, report.Statements)
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))
}

// The probes before the transaction share the attempt's deadline. With
// every pool connection held elsewhere, a run that cannot even probe its
// names stops at the statement budget instead of waiting for the caller.
func TestCreateWithRowSecurityProbesShareTheAttemptDeadline(t *testing.T) {
	f := newCreateFixture(t, "notes")
	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(small.Close)
	held, err := small.Acquire(t.Context())
	require.NoError(t, err)
	defer held.Release()
	budget := executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 300 * time.Millisecond}
	d := desiredRowSecurity(t, notesWithRowSecurity)

	runs := map[string]func() error{
		"execute-create": func() error {
			_, err := executor.ExecuteCreateWithRowSecurity(t.Context(), small, f.at, f.cr, d, budget)
			return err
		},
		"execute-desired": func() error {
			_, err := executor.ExecuteDesiredRowSecurity(t.Context(), small, f.schema, "", d, budget)
			return err
		},
		"preview-desired": func() error {
			_, err := executor.PreviewDesiredCreateWithRowSecurity(t.Context(), small, f.schema, "", d, budget)
			return err
		},
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			err := run()
			var budgetErr *executor.BudgetError
			require.ErrorAs(t, err, &budgetErr)
			assert.Equal(t, executor.CauseStatement, budgetErr.Cause)
		})
	}
	assert.False(t, relationExists(t, f.pool, f.schema, "notes"))
}
