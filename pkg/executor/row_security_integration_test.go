package executor_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

func rlsFixture(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON %s.documents FOR SELECT USING (owner_id = 7);`, schema, schema, schema))
	require.NoError(t, err)
	return pool, schema
}

func applyRLS(t *testing.T, pool *pgxpool.Pool, schema, sql string) (executor.RowSecurityReport, error) {
	t.Helper()
	desired, err := statement.ParseDesiredWithRowSecurity(sql)
	require.NoError(t, err)
	return executor.ExecuteRowSecurity(t.Context(), pool, schema, desired, executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second})
}

func TestExecuteRowSecurityReplacesPoliciesAtomically(t *testing.T) {
	pool, schema := rlsFixture(t)
	sql := `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 ALTER TABLE documents FORCE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);
 CREATE POLICY writers ON documents FOR INSERT WITH CHECK (owner_id = 9);
 COMMENT ON POLICY readers ON documents IS 'Only your documents';`
	report, err := applyRLS(t, pool, schema, sql)
	require.NoError(t, err)
	assert.NotEmpty(t, report.Statements)
	actual, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, actual.RowSecurity.Enabled)
	assert.True(t, actual.RowSecurity.Forced)
	require.Len(t, actual.RowSecurity.Policies, 2)
	assert.Equal(t, "(owner_id = 9)", *actual.RowSecurity.Policies[0].Using)
	assert.Equal(t, "Only your documents", *actual.RowSecurity.Policies[0].Comment)
	assert.Equal(t, "(owner_id = 9)", *actual.RowSecurity.Policies[1].WithCheck)
	again, err := applyRLS(t, pool, schema, sql)
	require.NoError(t, err)
	assert.Empty(t, again.Statements, "a converged retry must do no live DDL")
}

func TestExecuteRowSecurityRemovesLastPolicyAndForce(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents FORCE ROW LEVEL SECURITY;`, schema))
	require.NoError(t, err)
	_, err = applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	actual, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.False(t, actual.RowSecurity.Enabled)
	assert.False(t, actual.RowSecurity.Forced)
	assert.Empty(t, actual.RowSecurity.Policies)
}

func TestExecuteRowSecurityRefusesMixedChanges(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL,
     title text
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, schemadiff.ErrUnsupportedChange)
	assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
	assert.True(t, executor.OutcomeCode(err).Permanent())
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityLockContentionLeavesPoliciesIntact(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{schema, "documents"}.Sanitize()+" IN ACCESS SHARE MODE")
	require.NoError(t, err)
	_, err = applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "55P03", pgErr.Code)
	assert.Equal(t, executor.CodeBudgetLockExceeded, executor.OutcomeCode(err))
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityRollsBackAfterLiveDDLFailure(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	// A real server-side failure after DROP POLICY and CREATE POLICY proves that
	// the transaction restores the original policy, rather than just refusing early.
	trigger := pgx.Identifier{schema + "_fail_policy"}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.fail_policy() RETURNS event_trigger
 LANGUAGE plpgsql AS $$
 BEGIN
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_policy p ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid
         JOIN pg_class c ON c.oid = p.polrelid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND d.command_tag = 'CREATE POLICY'
     ) THEN
         RAISE EXCEPTION 'injected policy failure';
     END IF;
 END;
 $$;
 CREATE EVENT TRIGGER %s ON ddl_command_end EXECUTE FUNCTION %s.fail_policy();`, schema, schema, trigger, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP EVENT TRIGGER "+trigger)
		require.NoError(t, err)
	})
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "P0001", pgErr.Code)
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityEnabledWithoutPoliciesDeniesRows(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO %s.documents VALUES (1, 7), (2, 9);`, schema))
	require.NoError(t, err)
	_, err = applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	// Real non-owner role: catalog convergence alone cannot prove default deny.
	role := pgx.Identifier{schema + "_reader"}.Sanitize()
	_, err = pool.Exec(t.Context(), "CREATE ROLE "+role)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, err := pool.Exec(ctx, "DROP OWNED BY "+role+"; DROP ROLE "+role)
		require.NoError(t, err)
	})
	_, err = pool.Exec(t.Context(), fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s; GRANT SELECT ON %s.documents TO %s", schema, role, schema, role))
	require.NoError(t, err)
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+role)
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{schema, "documents"}.Sanitize()).Scan(&count))
	assert.Zero(t, count)
}

func TestExecuteRowSecurityRefusesMissingTable(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := applyRLS(t, pool, schema, `CREATE TABLE missing (
     id bigint PRIMARY KEY
 );
 ALTER TABLE missing ENABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, executor.ErrTableNotFound)
	assert.Equal(t, executor.CodeTableNotFound, executor.OutcomeCode(err))
	assert.Empty(t, relationKind(t, pool, schema, "missing"))
}

func TestExecuteRowSecurityReadsStateAfterWaitingForLock(t *testing.T) {
	pool, schema := rlsFixture(t)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	target := pgx.Identifier{schema, "documents"}.Sanitize()
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+target+" IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := executor.ExecuteRowSecurity(ctx, pool, schema, desired, executor.Budget{LockTimeout: 3 * time.Second, StatementTimeout: 5 * time.Second})
		done <- err
	}()
	// Observe the actual lock queue, not a timing guess. The earlier policy read
	// would miss this new policy and fail convergence instead of applying cleanly.
	const queueDeadline = 2 * time.Second
	const queuePoll = 10 * time.Millisecond
	require.Eventually(t, func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(), `SELECT EXISTS (
      SELECT 1 FROM pg_locks WHERE relation = $1::regclass
      AND mode = 'AccessExclusiveLock' AND NOT granted
  )`, target).Scan(&waiting)
		return err == nil && waiting
	}, queueDeadline, queuePoll)
	_, err = holder.Exec(t.Context(), "CREATE POLICY concurrent_reader ON "+target+" FOR SELECT USING (owner_id = 11)")
	require.NoError(t, err)
	require.NoError(t, holder.Commit(t.Context()))
	require.NoError(t, <-done)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, after.RowSecurity.Policies, 1)
	assert.Equal(t, "readers", after.RowSecurity.Policies[0].Name)
	assert.Equal(t, "(owner_id = 9)", *after.RowSecurity.Policies[0].Using)
}

func TestExecuteRowSecurityUsesOneConnectionAndQuotedNames(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t), MaxConns: 1})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s."odd""table" (
     id bigint PRIMARY KEY
 );`, schema))
	require.NoError(t, err)
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE "odd""table" (
     id bigint PRIMARY KEY
 );
 ALTER TABLE "odd""table" ENABLE ROW LEVEL SECURITY;
 CREATE POLICY "read""only" ON "odd""table" FOR SELECT USING (id = 7);
 COMMENT ON POLICY "read""only" ON "odd""table" IS 'a quote: '' and semicolon;';`)
	require.NoError(t, err)
	_, err = executor.ExecuteRowSecurity(t.Context(), pool, schema, desired, executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second})
	require.NoError(t, err)
	actual, err := schemadiff.Introspect(t.Context(), pool, schema, `odd"table`)
	require.NoError(t, err)
	require.Len(t, actual.RowSecurity.Policies, 1)
	assert.Equal(t, `read"only`, actual.RowSecurity.Policies[0].Name)
	assert.Equal(t, "a quote: ' and semicolon;", *actual.RowSecurity.Policies[0].Comment)
	var scratch int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_namespace WHERE nspname LIKE 'pgsprite_scratch_%'`).Scan(&scratch))
	assert.Zero(t, scratch, "savepoints must not commit scratch objects with the live change")
}

func TestExecuteRowSecurityDeadlineRollsBackLiveDDL(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	// Stall after live policy creation to exhaust the whole attempt budget.
	// The policy drops and additions must all roll back on cancellation.
	_, err = pool.Exec(t.Context(), "CREATE SEQUENCE "+pgx.Identifier{schema, "fault_reached"}.Sanitize())
	require.NoError(t, err)
	trigger := pgx.Identifier{schema + "_fail_policy"}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.fail_policy() RETURNS event_trigger
 LANGUAGE plpgsql AS $$
 BEGIN
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_policy p ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid
         JOIN pg_class c ON c.oid = p.polrelid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND d.command_tag = 'CREATE POLICY'
     ) THEN
         PERFORM nextval('%s.fault_reached');
         PERFORM pg_sleep(10);
     END IF;
 END;
 $$;
 CREATE EVENT TRIGGER %s ON ddl_command_end EXECUTE FUNCTION %s.fail_policy();`, schema, schema, schema, trigger, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP EVENT TRIGGER "+trigger)
		require.NoError(t, err)
	})
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	require.NoError(t, err)
	report, err := executor.ExecuteRowSecurity(t.Context(), pool, schema, desired, executor.Budget{LockTimeout: 50 * time.Millisecond, StatementTimeout: 2 * time.Second})
	require.Error(t, err)
	assert.Equal(t, executor.CodeBudgetStatementExceeded, executor.OutcomeCode(err))
	assert.Empty(t, report.Statements)
	var reached bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT is_called FROM "+pgx.Identifier{schema, "fault_reached"}.Sanitize()).Scan(&reached))
	assert.True(t, reached, "the deadline must fire after live DDL, not during setup")
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// Qualified application helpers keep their binding; a target-schema function
// with a built-in's name must not change the policy during live execution.
func TestExecuteRowSecurityPreservesHelperResolution(t *testing.T) {
	pool, schema := rlsFixture(t)
	helpers := testutil.NewSchema(t, pool)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
 CREATE FUNCTION %s.allowed_owner() RETURNS bigint
 LANGUAGE sql IMMUTABLE AS 'SELECT 9::bigint';
 CREATE FUNCTION %s.abs(bigint) RETURNS bigint
 LANGUAGE sql IMMUTABLE AS 'SELECT 999::bigint';
 INSERT INTO %s.documents VALUES (1, -9), (2, -7);`, helpers, schema, schema))
	require.NoError(t, err)
	sql := fmt.Sprintf(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT
     USING (abs(owner_id) = %s.allowed_owner());`, helpers)
	report, err := applyRLS(t, pool, schema, sql)
	require.NoError(t, err)
	actual, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	require.Len(t, actual.RowSecurity.Policies, 1)
	assert.Equal(t, "(abs(owner_id) = "+helpers+".allowed_owner())", *actual.RowSecurity.Policies[0].Using)
	// SQL in the committed report comes from PostgreSQL's canonical catalog form.
	assert.Contains(t, report.Statements[3], "USING ((abs(owner_id) = "+helpers+".allowed_owner()))")
	role := pgx.Identifier{schema + "_reader"}.Sanitize()
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE ROLE %s;
 GRANT USAGE ON SCHEMA %s, %s TO %s;
 GRANT SELECT ON %s.documents TO %s;`, role, schema, helpers, role, schema, role))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP OWNED BY "+role+"; DROP ROLE "+role)
		require.NoError(t, err)
	})
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+role)
	require.NoError(t, err)
	var ids []int64
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT array_agg(id ORDER BY id) FROM "+pgx.Identifier{schema, "documents"}.Sanitize()).Scan(&ids))
	assert.Equal(t, []int64{1}, ids)
}
