package cli

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
	"github.com/jackc/pgx/v5"
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
	assert.Equal(t, "55P03", v.SQLState)
	assert.Equal(t, "canceling statement due to lock timeout", v.ServerError)
	assert.Empty(t, v.ExecutedSQL)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// A row-security declaration whose CREATE TABLE declares one constraint
// twice is refused by design, as the same file without row security is:
// the scratch build shows the server kept only one of them, and nothing
// runs against the live table.
func TestMigrateDesiredRLSWithAConstraintDeclaredTwiceIsRefused(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL,
 CONSTRAINT documents_owner_a UNIQUE (owner_id),
 CONSTRAINT documents_owner_b UNIQUE (owner_id)
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	var out strings.Builder
	require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, v.Reason)
	assert.Equal(t, verdict.ClassByDesign, v.Class)
	assert.Contains(t, v.Detail, `constraint "documents_owner_b"`)
	assert.Empty(t, v.Code)
	assert.Empty(t, v.ExecutedSQL)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// An absent table is created together with its row security. The dry run
// lists the statements without creating anything, the run commits exactly
// those statements, and a rerun finds nothing to do.
func TestMigrateDesiredRLSCreatesMissingTable(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), "DROP TABLE "+schema+".documents")
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY,
 owner_id int NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT USING (owner_id = 7);`)

	cmd.DryRun = true
	var out strings.Builder
	require.NoError(t, cmd.run(t.Context(), &out))
	var preview executor.RowSecurityPlan
	require.NoError(t, json.Unmarshal([]byte(out.String()), &preview))
	require.Len(t, preview.Statements, 4)
	_, err = schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.ErrorIs(t, err, schemadiff.ErrTableNotFound, "the dry run must not create the table")

	cmd.DryRun = false
	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Equal(t, "table created with its row security: all changes committed in one transaction", v.Detail)
	assert.Equal(t, preview.Statements, v.ExecutedSQL)
	assert.Contains(t, v.ExecutedSQL[0], "CREATE TABLE "+schema+".documents")
	assert.Contains(t, v.ExecutedSQL[3], `CREATE POLICY "readers"`)
	after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
	require.NoError(t, err)
	assert.True(t, after.RowSecurity.Enabled)
	require.Len(t, after.RowSecurity.Policies, 1)
	assert.Equal(t, "(owner_id = 7)", *after.RowSecurity.Policies[0].Using)

	out.Reset()
	require.NoError(t, cmd.run(t.Context(), &out))
	v = verdict.Verdict{}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
	assert.Equal(t, verdict.OutcomeExecuted, v.Outcome)
	assert.Empty(t, v.ExecutedSQL)
	assert.Equal(t, "already converged: row security matches; nothing to run", v.Detail)
}

// A standalone type holding the table's name refuses as a create collision,
// on the dry run and the run alike, and nothing is created.
func TestMigrateDesiredRLSRefusesTypeAtTheTableName(t *testing.T) {
	url, schema, pool := desiredFixture(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("DROP TABLE %[1]s.documents; CREATE TYPE %[1]s.documents AS ENUM ('a')", schema))
	require.NoError(t, err)
	cmd := desiredCommand(t, url, schema, `CREATE TABLE documents (
 id int PRIMARY KEY
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
	for _, dryRun := range []bool{true, false} {
		cmd.DryRun = dryRun
		var out strings.Builder
		require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
		var v verdict.Verdict
		require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ClassEnvironmental, v.Class)
		assert.Equal(t, verdict.ReasonCreateCollision, v.Reason)
		assert.Empty(t, v.ExecutedSQL)
	}
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

// Both privilege failures must tell callers to fix the environment, not wait
// for a new engine capability. Neither may change the live table.
func TestMigrateDesiredRLSPrivilegeRefusals(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "non-owner"
		if owner {
			name = "owner-without-database-create"
		}
		t.Run(name, func(t *testing.T) {
			url, schema, pool := desiredFixture(t)
			role := pgx.Identifier{schema + "_role"}.Sanitize()
			_, err := pool.Exec(t.Context(), "CREATE ROLE "+role+" LOGIN")
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx := context.WithoutCancel(t.Context())
				_, err := pool.Exec(ctx, "DROP OWNED BY "+role)
				require.NoError(t, err)
				_, err = pool.Exec(ctx, "DROP ROLE "+role)
				require.NoError(t, err)
			})
			_, err = pool.Exec(t.Context(), "GRANT USAGE, CREATE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+role)
			require.NoError(t, err)
			if owner {
				_, err = pool.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{schema, "documents"}.Sanitize()+" OWNER TO "+role)
				require.NoError(t, err)
			}
			if !owner {
				var database string
				require.NoError(t, pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&database))
				_, err = pool.Exec(t.Context(), "GRANT CREATE ON DATABASE "+pgx.Identifier{database}.Sanitize()+" TO "+role)
				require.NoError(t, err)
			}
			before, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			config, err := neturl.Parse(url)
			require.NoError(t, err)
			query := config.Query()
			query.Set("role", schema+"_role")
			config.RawQuery = query.Encode()
			cmd := desiredCommand(t, config.String(), schema, `CREATE TABLE documents (
    id int PRIMARY KEY,
    owner_id int NOT NULL
   );
   ALTER TABLE documents ENABLE ROW LEVEL SECURITY;`)
			var out strings.Builder
			runErr := cmd.run(t.Context(), &out)
			require.ErrorIs(t, runErr, verdict.ErrRefused)
			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
			assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
			assert.Equal(t, verdict.ClassEnvironmental, v.Class)
			if owner {
				assert.ErrorIs(t, runErr, executor.ErrRowSecurityDatabaseCreateRequired)
			} else {
				assert.ErrorIs(t, runErr, executor.ErrRowSecurityOwnerRequired)
			}
			assert.Empty(t, v.ExecutedSQL)
			after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
