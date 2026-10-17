package preflight_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// ColumnDependentViews maps each column to the views and materialized views
// that read it, from any schema. A view that reads the table without naming
// a column, or reads another table, does not count against the column, and
// a column no view reads is absent.
func TestColumnDependentViews(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	other := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text, note text, unread text);
		CREATE TABLE %[1]s.teams (id bigint PRIMARY KEY, email text);
		CREATE VIEW %[1]s.contacts AS SELECT id, email FROM %[1]s.users;
		CREATE MATERIALIZED VIEW %[2]s."Notes" AS SELECT note FROM %[1]s.users WHERE email IS NOT NULL;
		CREATE VIEW %[1]s.user_count AS SELECT pg_catalog.count(*) FROM %[1]s.users;
		CREATE VIEW %[1]s.team_mail AS SELECT email FROM %[1]s.teams`, schema, other))
	require.NoError(t, err)

	views, err := preflight.ColumnDependentViews(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{
		"id":    {fmt.Sprintf(`"%s"."contacts"`, schema)},
		"email": sortedQualified([2]string{schema, "contacts"}, [2]string{other, "Notes"}),
		"note":  {fmt.Sprintf(`"%s"."Notes"`, other)},
	}, views)
}

// sortedQualified renders the views as ColumnDependentViews orders them: by
// schema, then name, bytewise.
func sortedQualified(views ...[2]string) []string {
	if views[0][0] > views[1][0] {
		views[0], views[1] = views[1], views[0]
	}
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = fmt.Sprintf(`"%s"."%s"`, v[0], v[1])
	}
	return out
}
