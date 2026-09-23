package executor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// Fire only for live CREATE POLICY, never for the disposable scratch declaration.
func onLiveRLSPolicy(t *testing.T, pool *pgxpool.Pool, schema, action string) {
	t.Helper()
	trigger := pgx.Identifier{schema + "_policy_fault"}.Sanitize()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.policy_fault() RETURNS event_trigger
 LANGUAGE plpgsql AS $$
 BEGIN
     IF EXISTS (
         SELECT 1 FROM pg_event_trigger_ddl_commands() d
         JOIN pg_policy p ON d.classid = 'pg_policy'::regclass AND d.objid = p.oid
         JOIN pg_class c ON c.oid = p.polrelid
         JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = '%s' AND d.command_tag = 'CREATE POLICY'
     ) THEN
         %s
     END IF;
 END;
 $$;
 CREATE EVENT TRIGGER %s ON ddl_command_end EXECUTE FUNCTION %s.policy_fault();`, schema, schema, action, trigger, schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP EVENT TRIGGER "+trigger)
		require.NoError(t, err)
	})
}

func TestExecuteRowSecurityRollsBackPolicyDivergence(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	onLiveRLSPolicy(t, pool, schema, fmt.Sprintf(`ALTER POLICY readers ON %s.documents USING (false);`, schema))
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Equal(t, executor.CodeInvariantViolation, executor.OutcomeCode(err))
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityRollsBackTableDivergence(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	onLiveRLSPolicy(t, pool, schema, fmt.Sprintf(`CREATE TABLE %s.unexpected_reference (
     document_id bigint REFERENCES %s.documents(id)
 );`, schema, schema))
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityWrapsCommitFailure(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	// Defer a real server error until COMMIT, after all convergence checks pass.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.commit_probe (id integer);
 CREATE FUNCTION %s.fail_commit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
     RAISE EXCEPTION 'injected deferred commit failure';
 END;
 $$;
 CREATE CONSTRAINT TRIGGER fail_commit AFTER INSERT ON %s.commit_probe
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s.fail_commit();`, schema, schema, schema, schema))
	require.NoError(t, err)
	onLiveRLSPolicy(t, pool, schema, fmt.Sprintf(`INSERT INTO %s.commit_probe VALUES (1);`, schema))
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 9);`)
	var unknown *executor.RowSecurityOutcomeUnknownError
	require.ErrorAs(t, err, &unknown)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "P0001", pgErr.Code)
	assert.Equal(t, executor.CodeRowSecurityOutcomeUnknown, executor.OutcomeCode(err))
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
