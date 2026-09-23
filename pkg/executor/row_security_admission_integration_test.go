package executor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

func requireRLSShapeRefusal(t *testing.T, pool *pgxpool.Pool, schema string, cause error) {
	t.Helper()
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	report, err := applyRLS(t, pool, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
	assert.True(t, executor.OutcomeCode(err).Permanent())
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityRefusesReferencedTable(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.references_documents (
     document_id bigint REFERENCES %s.documents(id)
 );`, schema, schema))
	require.NoError(t, err)
	requireRLSShapeRefusal(t, pool, schema, schemadiff.ErrUnrenderableForeignKey)
}

func TestExecuteRowSecurityRefusesInheritanceParent(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.child_documents () INHERITS (%s.documents);`, schema, schema))
	require.NoError(t, err)
	requireRLSShapeRefusal(t, pool, schema, schemadiff.ErrUnrenderableInheritance)
}

func TestExecuteRowSecurityRefusesInheritanceChild(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE TABLE %s.parent_documents (
     id bigint NOT NULL,
     owner_id bigint NOT NULL
 );
 ALTER TABLE %s.documents INHERIT %s.parent_documents;`, schema, schema, schema))
	require.NoError(t, err)
	requireRLSShapeRefusal(t, pool, schema, schemadiff.ErrUnrenderableInheritance)
}

func TestExecuteRowSecurityRefusesUnloggedTable(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents SET UNLOGGED;`, schema))
	require.NoError(t, err)
	requireRLSShapeRefusal(t, pool, schema, schemadiff.ErrUnrenderableUnlogged)
}

func TestExecuteRowSecurityRefusesPartitionedParent(t *testing.T) {
	pool, schema := rlsFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`DROP TABLE %s.documents;
 CREATE TABLE %s.documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 ) PARTITION BY RANGE (id);
 ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;`, schema, schema, schema))
	require.NoError(t, err)
	requireRLSShapeRefusal(t, pool, schema, schemadiff.ErrUnrenderablePartition)
}

func TestExecuteRowSecurityRefusesNonOwnerBeforeLock(t *testing.T) {
	pool, schema := rlsFixture(t)
	role := pgx.Identifier{schema + "_writer"}.Sanitize()
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE ROLE %s;
 GRANT USAGE ON SCHEMA %s TO %s;
 GRANT UPDATE ON %s.documents TO %s;`, role, schema, role, schema, role))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP OWNED BY "+role+"; DROP ROLE "+role)
		require.NoError(t, err)
	})
	cfg := pool.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	restricted, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer restricted.Close()
	// A conflicting lock makes the assertion distinguish early owner refusal
	// from an attempt that first waits for ACCESS EXCLUSIVE and times out.
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = tx.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{schema, "documents"}.Sanitize()+" IN ACCESS SHARE MODE")
	require.NoError(t, err)
	_, err = applyRLS(t, restricted, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.ErrorIs(t, err, executor.ErrRowSecurityRefused)
	assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
	assert.True(t, executor.OutcomeCode(err).Permanent())
}

func TestExecuteRowSecurityAcceptsInheritedOwnership(t *testing.T) {
	pool, schema := rlsFixture(t)
	owner := pgx.Identifier{schema + "_owner"}.Sanitize()
	role := pgx.Identifier{schema + "_engine"}.Sanitize()
	var database string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&database))
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE ROLE %s;
 CREATE ROLE %s INHERIT;
 GRANT %s TO %s;
 GRANT USAGE ON SCHEMA %s TO %s;
 GRANT CREATE ON DATABASE %s TO %s;
 ALTER TABLE %s.documents OWNER TO %s;`, owner, role, owner, role, schema, role, pgx.Identifier{database}.Sanitize(), role, schema, owner))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.WithoutCancel(t.Context()), "DROP OWNED BY "+role+"; DROP OWNED BY "+owner+" CASCADE; DROP ROLE "+role+"; DROP ROLE "+owner)
		require.NoError(t, err)
	})
	cfg := pool.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	engine, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer engine.Close()
	_, err = applyRLS(t, engine, schema, `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents DISABLE ROW LEVEL SECURITY;`)
	require.NoError(t, err)
	actual, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.False(t, actual.RowSecurity.Enabled)
	assert.Empty(t, actual.RowSecurity.Policies)
}
