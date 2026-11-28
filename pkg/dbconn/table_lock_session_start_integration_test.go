package dbconn_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// switchedRoleURL is serverURL with the role connection parameter set, so
// every session logs in as the URL's user and then runs as role. The
// parameter is set through the parsed URL, and pgx's own parse confirms it
// reaches the startup packet.
func switchedRoleURL(t *testing.T, serverURL, role string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	require.NoError(t, err, "parse server URL")
	query := u.Query()
	query.Set("role", role)
	u.RawQuery = query.Encode()
	switched := u.String()
	parsed, err := pgx.ParseConfig(switched)
	require.NoError(t, err, "parse the switched-role URL")
	require.Equal(t, role, parsed.RuntimeParams["role"], "the role parameter rides the startup packet")
	return switched
}

// tableLockHeld reports, from a connection of its own, whether any session
// holds the table lock for schema.table.
func tableLockHeld(t *testing.T, observer dbconn.AdvisoryLockHolder, schema, table string) bool {
	t.Helper()
	var held bool
	require.NoError(t, observer.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1
		  FROM pg_catalog.pg_locks
		 WHERE locktype = 'advisory'
		   AND classid::bigint = $1
		   AND objid::bigint = hashtext(quote_ident($2) || '.' || quote_ident($3))::oid::bigint
		   AND objsubid = 2)`,
		dbconn.TableLockClassID, schema, table).Scan(&held))
	return held
}

// A session that logs in as a superuser and runs as a role without
// pg_read_all_stats cannot read its own backend_start: pg_stat_activity
// shows a session's details only to a current role with the privileges of
// the session's login user or of pg_read_all_stats. The lock session needs
// that start time to identify itself from other connections, so
// acquisition refuses with ErrSessionStartHidden before it takes the lock,
// and no session holds the table lock afterwards.
func TestTableLockRefusesASessionThatCannotReadItsOwnStart(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	role := testutil.NewRole(t, admin, "NOLOGIN")

	session, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: switchedRoleURL(t, serverURL, role)}, "app", "orders")
	if session != nil {
		t.Cleanup(func() { assert.NoError(t, session.Release(context.WithoutCancel(t.Context()))) })
	}
	require.ErrorIs(t, err, dbconn.ErrSessionStartHidden)
	assert.Nil(t, session)
	assert.False(t, tableLockHeld(t, admin, "app", "orders"), "the refusal comes before the lock is taken")
}

// The same switched role with pg_read_all_stats reads its own session's
// start, so the lock is taken and records that start.
func TestTableLockAcquiresForASwitchedRoleWithReadAllStats(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	admin, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: serverURL})
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	role := testutil.NewRole(t, admin, "NOLOGIN IN ROLE pg_read_all_stats")

	session, err := dbconn.AcquireTableLock(t.Context(), dbconn.Config{URL: switchedRoleURL(t, serverURL, role)}, "app", "orders")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, session.Release(context.WithoutCancel(t.Context()))) })
	assert.False(t, session.BackendStart().IsZero())
	assert.True(t, tableLockHeld(t, admin, "app", "orders"))

	holder, found, err := dbconn.LookupTableLockHolder(t.Context(), admin, "app", "orders")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, session.BackendPID(), holder.PID)
	assert.True(t, holder.BackendStart.Equal(session.BackendStart()), "the session's own read of its start matches the catalog's")
}
