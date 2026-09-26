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
			require.ErrorIs(t, cmd.run(t.Context(), &out), verdict.ErrRefused)
			var v verdict.Verdict
			require.NoError(t, json.Unmarshal([]byte(out.String()), &v))
			assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
			assert.Equal(t, verdict.ClassEnvironmental, v.Class)
			if owner {
				assert.Contains(t, v.Detail, "requires CREATE on the database")
			} else {
				assert.Contains(t, v.Detail, "requires owner privileges")
			}
			assert.Empty(t, v.ExecutedSQL)
			after, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
