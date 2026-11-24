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

// ReadTableDropDependents names every object outside the table that makes
// a DROP TABLE without CASCADE fail, except other tables' foreign keys.
// The table's own trigger, rule, policy, index, owned sequence, and
// foreign key to itself go with the drop and are not named: once the named
// objects and the referencing table are gone, the plain drop succeeds.
func TestReadTableDropDependents(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	schema := testutil.NewSchema(t, pool)
	other := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.users (id bigserial PRIMARY KEY, manager_id bigint REFERENCES %[1]s.users, email text);
		CREATE INDEX ON %[1]s.users (lower(email));
		CREATE FUNCTION %[1]s.noop() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
		CREATE TRIGGER users_noop BEFORE INSERT ON %[1]s.users FOR EACH ROW EXECUTE FUNCTION %[1]s.noop();
		CREATE RULE users_no_delete AS ON DELETE TO %[1]s.users DO INSTEAD NOTHING;
		ALTER TABLE %[1]s.users ENABLE ROW LEVEL SECURITY;
		CREATE POLICY users_self ON %[1]s.users USING (manager_id IN (SELECT id FROM %[1]s.users));
		CREATE TABLE %[1]s.orders (id bigint PRIMARY KEY, user_id bigint REFERENCES %[1]s.users);
		CREATE TABLE %[1]s.teams (id bigint PRIMARY KEY);
		CREATE VIEW %[1]s.contacts AS SELECT id, email FROM %[1]s.users;
		CREATE MATERIALIZED VIEW %[2]s.user_count AS SELECT pg_catalog.count(*) FROM %[1]s.users;
		CREATE FUNCTION %[1]s.first_user() RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT min(id) FROM %[1]s.users; END;
		CREATE FUNCTION %[1]s.user_email(u %[1]s.users) RETURNS text LANGUAGE sql AS 'SELECT u.email';
		CREATE TABLE %[1]s.snapshots (id bigint PRIMARY KEY, rows %[1]s.users[]);
		ALTER TABLE %[1]s.teams ENABLE ROW LEVEL SECURITY;
		CREATE POLICY teams_with_users ON %[1]s.teams USING (EXISTS (SELECT FROM %[1]s.users));`, schema, other))
	require.NoError(t, err)

	dependents, err := preflight.ReadTableDropDependents(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Equal(t, []string{
		fmt.Sprintf("column rows of table %s.snapshots", schema),
		fmt.Sprintf("function %s.first_user()", schema),
		fmt.Sprintf("function %[1]s.user_email(%[1]s.users)", schema),
		fmt.Sprintf("materialized view %s.user_count", other),
		fmt.Sprintf("policy teams_with_users on table %s.teams", schema),
		fmt.Sprintf("view %s.contacts", schema),
	}, dependents)

	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		DROP TABLE %[1]s.snapshots;
		DROP FUNCTION %[1]s.first_user();
		DROP FUNCTION %[1]s.user_email(%[1]s.users);
		DROP MATERIALIZED VIEW %[2]s.user_count;
		DROP POLICY teams_with_users ON %[1]s.teams;
		DROP VIEW %[1]s.contacts;
		DROP TABLE %[1]s.orders;`, schema, other))
	require.NoError(t, err)
	dependents, err = preflight.ReadTableDropDependents(t.Context(), pool, schema, "users")
	require.NoError(t, err)
	assert.Empty(t, dependents)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("DROP TABLE %s.users", schema))
	require.NoError(t, err, "nothing named is left, so the plain drop succeeds")
}

// A table that does not exist has nothing that keeps it from being dropped.
func TestReadTableDropDependentsOfAMissingTable(t *testing.T) {
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: testutil.StartPostgres(t)})
	require.NoError(t, err)
	defer pool.Close()

	dependents, err := preflight.ReadTableDropDependents(t.Context(), pool, testutil.NewSchema(t, pool), "absent")
	require.NoError(t, err)
	assert.Empty(t, dependents)
}
