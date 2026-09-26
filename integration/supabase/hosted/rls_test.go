package hosted_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Policies change real signed-in users' access; removing the last policy denies all.
func TestHostedDeclarativeRLS(t *testing.T) {
	f := newFixture(t)
	name := pgx.Identifier{f.name}.Sanitize()
	f.seed(t)
	desired := fmt.Sprintf(`CREATE TABLE %s (
 id integer PRIMARY KEY,
 owner_id uuid NOT NULL,
 body text NOT NULL
 );
 ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
 CREATE POLICY readers ON %s FOR SELECT TO authenticated
 USING (owner_id = auth.uid());`, name, name, name)
	definitions, err := statement.ParseDesiredWithRowSecurity(desired)
	require.NoError(t, err)
	_, err = executor.ExecuteRowSecurity(t.Context(), f.pool, "public", definitions, executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second})
	require.NoError(t, err)
	f.assertAccess(t)
	status, _, err := f.request(t.Context(), http.MethodPost, "/rest/v1/"+f.name, f.keys.Publishable, f.users[0].Token, map[string]any{"id": 3, "owner_id": f.users[0].ID, "body": "write denied"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, status)
	deny, err := statement.ParseDesiredWithRowSecurity(fmt.Sprintf(`CREATE TABLE %s (
 id integer PRIMARY KEY,
 owner_id uuid NOT NULL,
 body text NOT NULL
 );
 ALTER TABLE %s ENABLE ROW LEVEL SECURITY;`, name, name))
	require.NoError(t, err)
	_, err = executor.ExecuteRowSecurity(t.Context(), f.pool, "public", deny, executor.Budget{LockTimeout: time.Second, StatementTimeout: 5 * time.Second})
	require.NoError(t, err)
	f.assertRows(t, f.users[0].Token)
	f.assertRows(t, f.users[1].Token)
	f.assertRows(t, "")
	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+f.table).Scan(&count))
	assert.Equal(t, 2, count)
}
