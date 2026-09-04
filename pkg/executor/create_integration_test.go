package executor_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
)

// createFixture is one schema on a real server with an absence proof and
// a creation-privilege proof minted for the named table — the inputs
// ExecuteCreate requires.
type createFixture struct {
	pool   *pgxpool.Pool
	schema string
	at     preflight.AbsentTarget
	cr     preflight.CreationRole
}

func newCreateFixture(t *testing.T, table string) createFixture {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)

	at, err := preflight.CheckTableAbsent(t.Context(), pool, schema, table)
	require.NoError(t, err)
	cr, err := preflight.CheckCreatePrivileges(t.Context(), pool, schema)
	require.NoError(t, err)
	return createFixture{pool: pool, schema: schema, at: at, cr: cr}
}

func desired(t *testing.T, sql string) statement.DesiredSchema {
	t.Helper()
	ds, err := statement.ParseDesired(sql)
	require.NoError(t, err)
	return ds
}

// relationKind returns the pg_class relkind of schema.name, or "" when no
// relation owns the name — the catalog oracle for what a create run left.
func relationKind(t *testing.T, pool *pgxpool.Pool, schema, name string) string {
	t.Helper()
	var relkind *string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT c.relkind::text
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = $1 AND c.relname = $2`,
		schema, name).Scan(&relkind))
	if relkind == nil {
		return ""
	}
	return *relkind
}

// relationExists reports whether any relation owns schema.name, for
// assertions where only presence matters.
func relationExists(t *testing.T, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT EXISTS (
		   SELECT FROM pg_class c
		     JOIN pg_namespace n ON n.oid = c.relnamespace
		    WHERE n.nspname = $1 AND c.relname = $2)`,
		schema, name).Scan(&exists))
	return exists
}

func TestExecuteCreateRunsTableAndIndexes(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE TABLE t (id int PRIMARY KEY, name text);
		CREATE INDEX t_name_idx ON t (name);
		CREATE UNIQUE INDEX t_id_name_idx ON t (id, name);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.NoError(t, err)

	assert.Equal(t, "r", relationKind(t, f.pool, f.schema, "t"))
	assert.Equal(t, "i", relationKind(t, f.pool, f.schema, "t_name_idx"))
	assert.Equal(t, "i", relationKind(t, f.pool, f.schema, "t_id_name_idx"))

	require.Len(t, rep.Steps, 3)
	assert.Contains(t, rep.Steps[0].SQL, "CREATE TABLE")
	for _, step := range rep.Steps {
		assert.Equal(t, executor.StepBrief, step.Kind)
		assert.GreaterOrEqual(t, step.Duration, time.Duration(0))
	}
}

// A desired file may state its index before its table — declarative input
// carries no ordering contract — but an index cannot be built before its
// table exists, so the executor orders the CREATE TABLE first.
func TestExecuteCreateOrdersTableBeforeIndexes(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE INDEX t_name_idx ON t (name);
		CREATE TABLE t (id int, name text);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.NoError(t, err)

	require.Len(t, rep.Steps, 2)
	assert.Contains(t, rep.Steps[0].SQL, "CREATE TABLE")
	assert.Equal(t, "i", relationKind(t, f.pool, f.schema, "t_name_idx"))
}

func TestExecuteCreateWithProgressReportsQualifiedStepStatementsInOrder(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE INDEX t_name_idx ON t (name);
		CREATE TABLE t (id int, name text);
	`)
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	functionName := pgx.Identifier{f.schema, "delay_create_progress"}.Sanitize()
	triggerName := pgx.Identifier{f.schema + "_delay_create_progress"}.Sanitize()
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS event_trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF current_query() LIKE '%%%s%%' THEN
				PERFORM pg_sleep(0.25);
			END IF;
		END
		$$;
		CREATE EVENT TRIGGER %s ON ddl_command_start EXECUTE FUNCTION %s()`,
		functionName, f.schema, triggerName, functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, cleanupErr := f.pool.Exec(ctx, fmt.Sprintf("DROP EVENT TRIGGER IF EXISTS %s", triggerName))
		assert.NoError(t, cleanupErr)
		_, cleanupErr = f.pool.Exec(ctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
		assert.NoError(t, cleanupErr)
	})

	type result struct {
		rep executor.SequenceReport
		err error
	}
	results := make(chan result, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		rep, executeErr := executor.ExecuteCreateWithProgress(t.Context(), f.pool, f.at, f.cr, ds,
			createBudget, executor.DefaultRetryPolicy(), tracker)
		results <- result{rep: rep, err: executeErr}
	})
	t.Cleanup(workers.Wait)

	var observed []string
	require.Eventually(t, func() bool {
		snapshot, progressErr := tracker.Progress(t.Context())
		if progressErr != nil || snapshot.Detail.Statement == "" {
			return false
		}
		if len(observed) == 0 || observed[len(observed)-1] != snapshot.Detail.Statement {
			observed = append(observed, snapshot.Detail.Statement)
		}
		return len(observed) == 2
	}, 5*time.Second, 10*time.Millisecond, "both active create steps must publish their statements in order")

	execution := <-results
	workers.Wait()
	require.NoError(t, execution.err)
	require.Len(t, execution.rep.Steps, 2)
	assert.Equal(t, []string{
		fmt.Sprintf("CREATE TABLE %s.t (id int, name text)", f.schema),
		fmt.Sprintf("CREATE INDEX t_name_idx ON %s.t USING btree (name)", f.schema),
	}, observed)
}

// The absence proof is time-of-check: a create that takes the name after
// the check surfaces as the typed collision, and the caller re-diffs
// rather than assuming what the occupant looks like.
func TestExecuteCreateReportsCollisionAsTyped(t *testing.T) {
	f := newCreateFixture(t, "t")
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (other int)", f.schema))
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t (id int)")
	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.Error(t, err)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 1, stepErr.Step)
	assert.ErrorIs(t, err, executor.ErrCreateCollision)
	assert.Equal(t, executor.CodeCreateCollision, executor.OutcomeCode(err))
	assert.Empty(t, rep.Steps)
}

// The first-choice name of an index-backed constraint is part of the
// desired set's contract. An unrelated catalog occupant must refuse the
// whole set rather than make PostgreSQL suffix the constraint name.
func TestExecuteCreateRefusesOccupiedImplicitIndexNameBeforeExecution(t *testing.T) {
	f := newCreateFixture(t, "t")
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.other (v int)", f.schema))
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf("CREATE INDEX t_pkey ON %s.other (v)", f.schema))
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t (id int PRIMARY KEY, v text)")
	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrCreateCollision)
	assert.Empty(t, rep.Steps)
	assert.False(t, relationExists(t, f.pool, f.schema, "t"), "the catalog preflight runs before every step")
}

// A column-owned sequence's first-choice name is part of the desired set's
// contract. An occupant must refuse the whole set rather than make
// PostgreSQL silently suffix the sequence name.
func TestExecuteCreateRefusesOccupiedImplicitSequenceNameBeforeExecution(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "serial", sql: "CREATE TABLE t (id serial PRIMARY KEY)"},
		{name: "identity", sql: "CREATE TABLE t (id bigint GENERATED BY DEFAULT AS IDENTITY)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCreateFixture(t, "t")
			_, err := f.pool.Exec(t.Context(), fmt.Sprintf("CREATE SEQUENCE %s.t_id_seq", f.schema))
			require.NoError(t, err)

			rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, desired(t, tt.sql), createBudget, executor.DefaultRetryPolicy())
			require.ErrorIs(t, err, executor.ErrCreateCollision)
			assert.Empty(t, rep.Steps)
			assert.False(t, relationExists(t, f.pool, f.schema, "t"), "the catalog preflight runs before every step")
		})
	}
}

// A claimed-name probe that cannot complete says nothing about whether the
// names are free: it is the caller's operational failure, never a
// collision, so the caller retries rather than being told a free name is
// taken.
func TestExecuteCreateProbeFailureIsNotACollision(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, "CREATE TABLE t (id int PRIMARY KEY, v text)")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rep, err := executor.ExecuteCreate(ctx, f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, executor.ErrCreateCollision)
	var stepErr *executor.SequenceStepError
	assert.False(t, errors.As(err, &stepErr), "nothing started, so there is no step to blame")
	assert.Empty(t, rep.Steps)
	assert.False(t, relationExists(t, f.pool, f.schema, "t"))
}

// A failed step ends the run; the steps before it committed and remain,
// and the report covers exactly that prefix so the caller can disclose
// what already happened. An index on a column the table does not have
// passes admission — admission checks shape and target, not column
// existence — and fails only when the server executes it.
func TestExecuteCreateFailedStepKeepsCommittedPrefix(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE TABLE t (id int, name text);
		CREATE INDEX t_id_idx ON t (id);
		CREATE INDEX t_missing_idx ON t (missing);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.Error(t, err)

	var stepErr *executor.SequenceStepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, 3, stepErr.Step)
	assert.Equal(t, 3, stepErr.Total)
	// The server error is not a collision; it passes through untyped.
	assert.NotErrorIs(t, err, executor.ErrCreateCollision)
	assert.Equal(t, executor.CodeExecutionFailed, executor.OutcomeCode(err))

	assert.True(t, relationExists(t, f.pool, f.schema, "t"))
	assert.True(t, relationExists(t, f.pool, f.schema, "t_id_idx"))
	require.Len(t, rep.Steps, 2)

	// The committed prefix is the rerun contract: the absence check now
	// refuses, which is the declarative front door's signal to re-diff.
	_, err = preflight.CheckTableAbsent(t.Context(), f.pool, f.schema, "t")
	assert.ErrorIs(t, err, preflight.ErrRelationExists)
}

// A name claimed twice within the desired set is decidable at admission,
// so the whole set refuses before the first step runs — never a mid-run
// failure with a committed prefix.
func TestExecuteCreateRefusesDuplicateNamesAtAdmission(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{
			name: "two indexes under one name",
			sql: `CREATE TABLE t (id int, name text);
				CREATE INDEX dup_idx ON t (id);
				CREATE INDEX dup_idx ON t (name)`,
		},
		{
			name: "index named after the table",
			sql: `CREATE TABLE t (id int);
				CREATE INDEX t ON t (id)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCreateFixture(t, "t")
			ds := desired(t, tt.sql)

			_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
			require.ErrorIs(t, err, executor.ErrDuplicateCreateName)
			assert.Equal(t, executor.CodeDuplicateCreateName, executor.OutcomeCode(err))
			assert.False(t, relationExists(t, f.pool, f.schema, "t"),
				"admission covers the whole set before the first step executes")
		})
	}
}

// A standalone type occupying the table's name raises a different SQLSTATE
// than a relation would — every table also mints a composite type — and
// still surfaces as the typed collision.
func TestExecuteCreateReportsTypeCollisionAsTyped(t *testing.T) {
	f := newCreateFixture(t, "t")
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf("CREATE TYPE %s.t AS ENUM ('a')", f.schema))
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t (id int)")
	_, err = executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrCreateCollision)
	assert.Equal(t, executor.CodeCreateCollision, executor.OutcomeCode(err))
}

func TestExecuteCreateAdmissionRefusals(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr error
	}{
		{
			name:    "if not exists on the table",
			sql:     "CREATE TABLE IF NOT EXISTS t (id int)",
			wantErr: executor.ErrIfNotExistsUnsupported,
		},
		{
			name:    "if not exists on an index",
			sql:     "CREATE TABLE t (id int); CREATE INDEX IF NOT EXISTS t_idx ON t (id)",
			wantErr: executor.ErrIfNotExistsUnsupported,
		},
		// INHERITS, LIKE, and OF bind to a secondary relation or type the
		// qualification never touches: the name resolves via search_path
		// to an existing object the absence proof says nothing about.
		{
			name:    "inherits from an existing parent",
			sql:     "CREATE TABLE t (id int) INHERITS (parent)",
			wantErr: executor.ErrUnsupportedCreateStep,
		},
		{
			name:    "like an existing source table",
			sql:     "CREATE TABLE t (LIKE src INCLUDING ALL)",
			wantErr: executor.ErrUnsupportedCreateStep,
		},
		{
			name:    "of an existing composite type",
			sql:     "CREATE TABLE t OF ty",
			wantErr: executor.ErrUnsupportedCreateStep,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCreateFixture(t, "t")
			ds := desired(t, tt.sql)

			_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
			require.ErrorIs(t, err, tt.wantErr)
			assert.False(t, relationExists(t, f.pool, f.schema, "t"),
				"admission covers the whole set before the first step executes")
		})
	}
}

func TestExecuteCreateRefusesPartitionOf(t *testing.T) {
	f := newCreateFixture(t, "t_part")
	_, err := f.pool.Exec(t.Context(),
		fmt.Sprintf("CREATE TABLE %s.parent (id int) PARTITION BY RANGE (id)", f.schema))
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t_part PARTITION OF parent FOR VALUES FROM (1) TO (10)")
	_, err = executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrPartitionOfUnsupported)
	assert.Equal(t, executor.CodePartitionOfUnsupported, executor.OutcomeCode(err))
}

// A desired schema for one table can never run against a proof minted for
// another: the mismatch is an invariant breach, not a refusal.
func TestExecuteCreateRefusesProofTargetMismatch(t *testing.T) {
	f := newCreateFixture(t, "other")
	ds := desired(t, "CREATE TABLE t (id int)")

	_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.False(t, relationExists(t, f.pool, f.schema, "t"))
}

// Create steps run with search_path pinned to the proof's schema then
// public — the same policy the introspection read path sets — so a
// desired file's unqualified type reference resolves in the target
// schema, and resolves there even when public holds a type of the same
// name. Without the pin the steps would run under the session default and
// the target schema's type would be invisible (SQLSTATE 42704).
func TestExecuteCreateResolvesTypesInTargetSchema(t *testing.T) {
	f := newCreateFixture(t, "t")

	// The type lives in the target schema and, under a unique name, in
	// public too — resolution must pick the target schema's copy.
	typeName := f.schema + "_mood"
	_, err := f.pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TYPE %s.%s AS ENUM ('happy', 'sad')", f.schema, typeName))
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), fmt.Sprintf(
		"CREATE TYPE public.%s AS ENUM ('decoy')", typeName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()),
			fmt.Sprintf("DROP TYPE IF EXISTS public.%s", typeName))
		assert.NoError(t, err)
	})

	ds := desired(t, fmt.Sprintf("CREATE TABLE t (id int, m %s)", typeName))
	_, err = executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.NoError(t, err)

	var udtSchema string
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT udt_schema FROM information_schema.columns
		  WHERE table_schema = $1 AND table_name = 't' AND column_name = 'm'`,
		f.schema).Scan(&udtSchema))
	assert.Equal(t, f.schema, udtSchema,
		"the column's type must resolve in the proof's schema, not public")
}

// An explicit CREATE INDEX whose name is the first choice of an implicit
// constraint index is a decidable conflict: admission refuses the whole
// set before anything runs, rather than letting the server suffix its way
// around one name or fail mid-run after the table committed.
func TestExecuteCreateRefusesImplicitIndexNameCollision(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{
			name: "explicit index named after the primary key's index",
			sql: `CREATE TABLE t (id int PRIMARY KEY);
			      CREATE INDEX t_pkey ON t (id);`,
		},
		{
			name: "explicit index named after a unique constraint's index",
			sql: `CREATE TABLE t (a int, b int, UNIQUE (a, b));
			      CREATE INDEX t_a_b_key ON t (a);`,
		},
		{
			name: "explicit index named after a named constraint",
			sql: `CREATE TABLE t (id int, CONSTRAINT my_uni UNIQUE (id));
			      CREATE INDEX my_uni ON t (id);`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCreateFixture(t, "t")
			ds := desired(t, tt.sql)

			_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
			require.ErrorIs(t, err, executor.ErrDuplicateCreateName)
			assert.Equal(t, executor.CodeDuplicateCreateName, executor.OutcomeCode(err))
			assert.False(t, relationExists(t, f.pool, f.schema, "t"),
				"admission covers the whole set before the first step executes")
		})
	}
}

// A zero CreationRole is forgeable by any package: only
// CheckCreatePrivileges mints one with a schema, so the executor refuses
// it as an invariant breach before anything runs.
func TestExecuteCreateRefusesZeroCreationRole(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, "CREATE TABLE t (id int)")

	_, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, preflight.CreationRole{}, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.False(t, relationExists(t, f.pool, f.schema, "t"))
}

// A creation-privilege proof minted for one schema can never authorize a
// run whose absence proof names another: the mismatch is an invariant
// breach, not a refusal.
func TestExecuteCreateRefusesCreationRoleSchemaMismatch(t *testing.T) {
	f := newCreateFixture(t, "t")
	otherSchema := testutil.NewSchema(t, f.pool)
	otherCR, err := preflight.CheckCreatePrivileges(t.Context(), f.pool, otherSchema)
	require.NoError(t, err)

	ds := desired(t, "CREATE TABLE t (id int)")
	_, err = executor.ExecuteCreate(t.Context(), f.pool, f.at, otherCR, ds, createBudget, executor.DefaultRetryPolicy())
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.False(t, relationExists(t, f.pool, f.schema, "t"))
}

// Unnamed CREATE INDEX steps claim no name — the server invents one,
// suffixing around occupants — so two of them in one desired set are not
// a duplicate-name conflict.
func TestExecuteCreateAllowsMultipleUnnamedIndexes(t *testing.T) {
	f := newCreateFixture(t, "t")
	ds := desired(t, `
		CREATE TABLE t (a int, b int);
		CREATE INDEX ON t (a);
		CREATE INDEX ON t (b);
	`)

	rep, err := executor.ExecuteCreate(t.Context(), f.pool, f.at, f.cr, ds, createBudget, executor.DefaultRetryPolicy())
	require.NoError(t, err)
	require.Len(t, rep.Steps, 3)

	var indexes int
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND tablename = 't'`,
		f.schema).Scan(&indexes))
	assert.Equal(t, 2, indexes, "both server-named indexes exist")
}
