package executor_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

func rlsOwnerPool(t *testing.T, admin *pgxpool.Pool, schema string) (*pgxpool.Pool, string) {
	t.Helper()
	role := pgx.Identifier{schema + "_owner"}.Sanitize()
	_, err := admin.Exec(t.Context(), fmt.Sprintf(`CREATE ROLE %s;
 GRANT USAGE ON SCHEMA %s TO %s;
 ALTER TABLE %s.documents OWNER TO %s;`, role, schema, role, schema, role))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(context.WithoutCancel(t.Context()), "DROP OWNED BY "+role+" CASCADE; DROP ROLE "+role)
		require.NoError(t, err)
	})
	cfg := admin.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, role
}

func TestExecuteRowSecurityRefusesMissingScratchPrivilegeBeforeLock(t *testing.T) {
	admin, schema := rlsFixture(t)
	pool, _ := rlsOwnerPool(t, admin, schema)
	var canCreate bool
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT has_database_privilege(current_user, current_database(), 'CREATE')").Scan(&canCreate))
	require.False(t, canCreate, "the fixture role must lack database CREATE")
	holder, err := admin.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{schema, "documents"}.Sanitize()+" IN ACCESS SHARE MODE")
	require.NoError(t, err)
	// The lock would time out if scratch privileges were checked only after locking.
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, executor.ErrRowSecurityRefused)
	assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
	assert.True(t, executor.OutcomeCode(err).Permanent())
	assert.Empty(t, report.Statements)
}

func TestExecuteRowSecurityRechecksOwnerAfterLockWait(t *testing.T) {
	admin, schema := rlsFixture(t)
	pool, role := rlsOwnerPool(t, admin, schema)
	var database string
	require.NoError(t, admin.QueryRow(t.Context(), "SELECT current_database()").Scan(&database))
	_, err := admin.Exec(t.Context(), "GRANT CREATE ON DATABASE "+pgx.Identifier{database}.Sanitize()+" TO "+role)
	require.NoError(t, err)
	before, err := schemadiff.Introspect(t.Context(), admin, schema, "documents")
	require.NoError(t, err)
	// This already matches. Without the recheck the no-op would succeed, so the
	// test cannot accidentally pass because PostgreSQL rejects a later owner-only DDL.
	desired, err := statement.ParseDesiredWithRowSecurity(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`)
	require.NoError(t, err)
	holder, err := admin.Begin(t.Context())
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
	const queueDeadline = 2 * time.Second
	const queuePoll = 10 * time.Millisecond
	require.Eventually(t, func() bool {
		var waiting bool
		err := admin.QueryRow(t.Context(), `SELECT EXISTS (
      SELECT 1 FROM pg_locks WHERE relation = $1::regclass
      AND mode = 'AccessExclusiveLock' AND NOT granted
  )`, target).Scan(&waiting)
		return err == nil && waiting
	}, queueDeadline, queuePoll)
	// Transfer to the administrator but retain enough privilege for the queued
	// LOCK to succeed. Only the executor's post-lock owner check must refuse.
	_, err = holder.Exec(t.Context(), "ALTER TABLE "+target+" OWNER TO CURRENT_USER; GRANT SELECT, UPDATE ON "+target+" TO "+role)
	require.NoError(t, err)
	require.NoError(t, holder.Commit(t.Context()))
	err = <-done
	require.ErrorIs(t, err, executor.ErrRowSecurityRefused)
	assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
	after, err := schemadiff.Introspect(t.Context(), admin, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
