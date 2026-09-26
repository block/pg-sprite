package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateDesiredRLSLockTimeoutPreservesState(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	holder, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, holder.Rollback(context.WithoutCancel(t.Context()))) }()
	_, err = holder.Exec(t.Context(), "LOCK TABLE "+schema+".documents IN ACCESS SHARE MODE")
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`, "--lock-timeout", "50ms", "--statement-timeout", "5s")
	var out strings.Builder
	err = cmd.run(t.Context(), &out)
	require.Error(t, err)
	require.NotErrorIs(t, err, verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeBudgetLockExceeded), v.Code)
	assert.Empty(t, v.ExecutedSQL)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestMigrateDesiredRLSMissingTableIsRefused(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), "DROP TABLE "+schema+".documents")
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Empty(t, v.Code)
	_, err = schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound)
}

func TestMigrateDesiredRLSRemovesLastPolicy(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`ALTER TABLE %s.documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON %s.documents FOR SELECT USING (true);`, schema, schema))
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, after.RowSecurity.Enabled)
	assert.Empty(t, after.RowSecurity.Policies, "enabled without policies is default deny")
}
