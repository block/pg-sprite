package executor_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// recursiveDocumentsPolicy is a documents policy that reads team_members,
// formatted for the schema both tables live in.
const recursiveDocumentsPolicy = `CREATE TABLE documents (
     id bigint PRIMARY KEY,
     owner_id bigint NOT NULL
 );
 ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON documents FOR SELECT
     USING (EXISTS (SELECT 1 FROM %s.team_members m WHERE m.owner_id = documents.owner_id));`

// A documents policy that reads team_members is refused when team_members'
// own policies read documents back, directly or through a view. PostgreSQL
// would accept the policy and then fail every query on documents that row
// security applies to, so both the preview and the execution refuse it
// before any policy changes, and the live policies stay as they were.
func TestExecuteRowSecurityRefusesPolicyRecursion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		// owners runs, in the rolled-back transaction that proves the
		// recursion, after the role row security applies to exists.
		owners string
	}{
		{
			name: "the other table's policy reads this table",
			setup: `CREATE TABLE %[1]s.team_members (owner_id bigint NOT NULL);
 ALTER TABLE %[1]s.team_members ENABLE ROW LEVEL SECURITY;
 CREATE POLICY owners ON %[1]s.team_members FOR SELECT
     USING (EXISTS (SELECT 1 FROM %[1]s.documents d WHERE d.owner_id = team_members.owner_id));`,
		},
		{
			name: "the other table's policy reads a view over this table",
			setup: `CREATE TABLE %[1]s.team_members (owner_id bigint NOT NULL);
 CREATE VIEW %[1]s.document_owners AS SELECT owner_id FROM %[1]s.documents;
 ALTER TABLE %[1]s.team_members ENABLE ROW LEVEL SECURITY;
 CREATE POLICY owners ON %[1]s.team_members FOR SELECT
     USING (owner_id IN (SELECT owner_id FROM %[1]s.document_owners));`,
			// A view reads its tables as its owner, so the view recurses
			// only when row security applies to that owner too.
			owners: `ALTER VIEW %[2]s.document_owners OWNER TO %[1]s;`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := rlsFixture(t)
			_, err := pool.Exec(t.Context(), fmt.Sprintf(tc.setup, schema))
			require.NoError(t, err)
			desired, err := statement.ParseDesiredWithRowSecurity(fmt.Sprintf(recursiveDocumentsPolicy, schema))
			require.NoError(t, err)
			requireRecursionWhenApplied(t, pool, schema, tc.owners)

			budget := executor.Budget{LockTimeout: 100 * time.Millisecond, StatementTimeout: 5 * time.Second}
			_, err = executor.PreviewRowSecurity(t.Context(), pool, schema, desired, budget)
			require.ErrorIs(t, err, executor.ErrPolicyRecursion)
			_, err = executor.ExecuteRowSecurity(t.Context(), pool, schema, desired, budget)
			require.ErrorIs(t, err, executor.ErrPolicyRecursion)
			require.ErrorIs(t, err, executor.ErrRowSecurityRefused)
			assert.ErrorContains(t, err, schema+".team_members")

			live, err := schemadiff.Introspect(t.Context(), pool, schema, "documents")
			require.NoError(t, err)
			require.Len(t, live.RowSecurity.Policies, 1)
			assert.Equal(t, "(owner_id = 7)", *live.RowSecurity.Policies[0].Using, "the live policy is unchanged")
		})
	}
}

// requireRecursionWhenApplied creates the documents policy by hand in a
// rolled-back transaction and confirms that a role row security applies to
// then cannot read documents at all. owners is formatted with that role and
// the schema and runs before the policy is created.
func requireRecursionWhenApplied(t *testing.T, pool *pgxpool.Pool, schema, owners string) {
	t.Helper()
	role := pgx.Identifier{schema + "_reader"}.Sanitize()
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
	documents := pgx.Identifier{schema, "documents"}.Sanitize()
	_, err = tx.Exec(t.Context(), fmt.Sprintf(`CREATE ROLE %[1]s;
 GRANT USAGE ON SCHEMA %[2]s TO %[1]s;
 GRANT SELECT ON ALL TABLES IN SCHEMA %[2]s TO %[1]s;
 `+owners+`
 DROP POLICY readers ON %[3]s;
 CREATE POLICY readers ON %[3]s FOR SELECT
     USING (EXISTS (SELECT 1 FROM %[2]s.team_members m WHERE m.owner_id = documents.owner_id));
 SET LOCAL ROLE %[1]s;`, role, pgx.Identifier{schema}.Sanitize(), documents))
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "SELECT count(*) FROM "+documents)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "querying documents must fail, got %v", err)
	assert.Equal(t, "42P17", pgErr.Code, "PostgreSQL reports the policies as infinite recursion: %s", pgErr.Message)
}
