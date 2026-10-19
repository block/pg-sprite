package preflight_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
)

// ColumnDependentViews maps each column to the views and materialized views
// that read it, from any schema. A view that reads the table without naming
// a column, or reads another table, does not count against the column, and
// a column no view reads is absent. A view on an inheritance child counts
// for the tree, not for the table alone.
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
		CREATE VIEW %[1]s.team_mail AS SELECT email FROM %[1]s.teams;
		CREATE TABLE %[1]s.admins (level int) INHERITS (%[1]s.users);
		CREATE VIEW %[1]s.admin_notes AS SELECT note, level FROM %[1]s.admins`, schema, other))
	require.NoError(t, err)

	views, err := preflight.ColumnDependentViews(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{
		"id":    {fmt.Sprintf(`"%s"."contacts"`, schema)},
		"email": sortedQualified([2]string{schema, "contacts"}, [2]string{other, "Notes"}),
		"note":  {fmt.Sprintf(`"%s"."Notes"`, other)},
	}, views.Own)
	assert.Equal(t, map[string][]string{
		"id":    {fmt.Sprintf(`"%s"."contacts"`, schema)},
		"email": sortedQualified([2]string{schema, "contacts"}, [2]string{other, "Notes"}),
		"note":  sortedQualified([2]string{schema, "admin_notes"}, [2]string{other, "Notes"}),
	}, views.Tree)
	assert.Equal(t, views.Tree, views.Drop, "admins inherits every column from users alone, so a drop reaches it")
}

// A recursive DROP COLUMN drops a child's copy only when the child
// inherits the column from this one parent and does not declare it, and
// descends only through the copies it drops; a type change reaches every
// copy. The server is the oracle: with the views Drop names removed the
// drop succeeds, so no other view blocks it, and each view Drop names
// blocks it on its own.
func TestColumnDependentViewsForADrop(t *testing.T) {
	url := testutil.StartPostgres(t)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: url})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id bigint PRIMARY KEY, email text);
		CREATE TABLE %[1]s.inherited () INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.below_inherited () INHERITS (%[1]s.inherited);
		CREATE TABLE %[1]s.declared (email text) INHERITS (%[1]s.users);
		CREATE TABLE %[1]s.below_declared () INHERITS (%[1]s.declared);
		CREATE TABLE %[1]s.other_parent (email text);
		CREATE TABLE %[1]s.two_parents () INHERITS (%[1]s.users, %[1]s.other_parent);
		CREATE TABLE %[1]s.attached (id bigint NOT NULL, email text);
		ALTER TABLE %[1]s.attached INHERIT %[1]s.users;
		CREATE VIEW %[1]s.v_users AS SELECT email FROM ONLY %[1]s.users;
		CREATE VIEW %[1]s.v_inherited AS SELECT email FROM ONLY %[1]s.inherited;
		CREATE VIEW %[1]s.v_below_inherited AS SELECT email FROM %[1]s.below_inherited;
		CREATE VIEW %[1]s.v_declared AS SELECT email FROM ONLY %[1]s.declared;
		CREATE VIEW %[1]s.v_below_declared AS SELECT email FROM %[1]s.below_declared;
		CREATE VIEW %[1]s.v_two_parents AS SELECT email FROM %[1]s.two_parents;
		CREATE VIEW %[1]s.v_attached AS SELECT email FROM %[1]s.attached;
		CREATE TABLE %[1]s.events (id bigint, email text) PARTITION BY RANGE (id);
		CREATE TABLE %[1]s.events_1 PARTITION OF %[1]s.events FOR VALUES FROM (0) TO (10);
		CREATE VIEW %[1]s.v_events_1 AS SELECT email FROM %[1]s.events_1`, schema))
	require.NoError(t, err)
	qualified := func(names ...string) []string {
		out := make([]string, len(names))
		for i, name := range names {
			out[i] = fmt.Sprintf(`"%s"."%s"`, schema, name)
		}
		return out
	}

	views, err := preflight.ColumnDependentViews(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, qualified("v_attached", "v_below_declared", "v_below_inherited", "v_declared",
		"v_inherited", "v_two_parents", "v_users"), views.Tree["email"])
	assert.Equal(t, qualified("v_below_inherited", "v_inherited", "v_users"), views.Drop["email"])
	assert.Equal(t, qualified("v_users"), views.Own["email"])

	dropFails := func(t *testing.T, keep string) error {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
		for _, view := range views.Drop["email"] {
			if view != keep {
				_, err := tx.Exec(t.Context(), "DROP VIEW "+view)
				require.NoError(t, err)
			}
		}
		_, err = tx.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.users DROP COLUMN email", schema))
		return err
	}
	require.NoError(t, dropFails(t, ""), "no view outside Drop blocks the drop")
	for _, view := range views.Drop["email"] {
		var pgErr *pgconn.PgError
		require.ErrorAs(t, dropFails(t, view), &pgErr, view)
		assert.Equal(t, "2BP01", pgErr.Code, view)
	}

	partitioned, err := preflight.ColumnDependentViews(t.Context(), pool, schema, "events")
	require.NoError(t, err)
	assert.Equal(t, qualified("v_events_1"), partitioned.Drop["email"], "a partition never declares a column")
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
