package executor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

func TestExecuteRowSecurityRefusesMissingPolicyRole(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+pgx.Identifier{schema, "documents"}.Sanitize()+" IN ACCESS SHARE MODE")
	require.NoError(t, err)
	// Invalid desired SQL must be refused without waiting for the target lock.
	report, err := applyRLS(t, pool, schema, fmt.Sprintf(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT TO %s_missing_role USING (true);`, schema))
	assertPermanentRLSDependency(t, err, "42704")
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestExecuteRowSecurityRefusesMissingPolicyHelper(t *testing.T) {
	pool, schema := rlsFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	report, err := applyRLS(t, pool, schema, fmt.Sprintf(`CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (%s.missing_helper(owner_id));`, schema))
	assertPermanentRLSDependency(t, err, "42883")
	assert.Empty(t, report.Statements)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func assertPermanentRLSDependency(t *testing.T, err error, sqlstate string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, sqlstate, pgErr.Code)
	assert.Equal(t, executor.CodeRowSecurityRefused, executor.OutcomeCode(err))
	assert.True(t, executor.OutcomeCode(err).Permanent())
}
