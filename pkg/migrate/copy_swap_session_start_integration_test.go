package migrate

import (
	"context"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// A host pool whose sessions log in as the superuser and switch to an
// ordinary role (the role connection parameter here, a login hook's
// SET ROLE on a host) cannot read when its own sessions started:
// pg_stat_activity shows a session's start only to a role with the
// privileges of its login user or of pg_read_all_stats. The swap runs on
// the pool and follows its own backend by PID and start, so Run refuses
// the pool before the preflight: a failed verdict with the
// table-lock-unconfirmed code, an error matching
// dbconn.ErrSessionStartHidden, and no shadow or checkpoint row left
// behind. The route's connection logs in as the same role directly, so its
// lock session reads its own start and the refusal can only be the pool's.
// With no disk headroom the preflight would refuse the statement with its
// own cause, so an empty cause shows the pool was checked first.
func TestCopyAndSwapRefusesAPoolThatCannotReadItsOwnStart(t *testing.T) {
	f := newCopySwapFixture(t)
	f.createOrders(t)
	role := testutil.NewRole(t, f.pool, "LOGIN PASSWORD 'switched'")
	t.Cleanup(func() {
		// The grant below must go before the role's drop; cleanups run in
		// reverse, so this runs first.
		_, err := f.pool.Exec(context.WithoutCancel(t.Context()),
			"DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
		assert.NoError(t, err)
	})
	// The role reads the table's catalog facts as the statement is routed,
	// ahead of the route's own checks.
	f.exec(t, `GRANT USAGE ON SCHEMA {schema} TO `+pgx.Identifier{role}.Sanitize())
	switched, err := dbconn.NewPool(t.Context(), dbconn.Config{
		URL:           f.switchedRoleURL(t, role),
		MaxConnsFloor: CopySwapMinConns(copier.Options{}),
	})
	require.NoError(t, err)
	t.Cleanup(switched.Close)
	st, err := statement.ParseOne(f.inSchema(`ALTER TABLE {schema}.orders ALTER COLUMN qty TYPE bigint`))
	require.NoError(t, err)
	opts := f.options()
	opts.CopySwap.Connection = dbconn.Config{URL: f.loginURL(t, role, "switched")}
	opts.CopySwap.FreeDiskBytes = 1

	v, err := Run(t.Context(), switched, st, opts)
	require.ErrorIs(t, err, dbconn.ErrSessionStartHidden)
	assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	assert.Equal(t, string(executor.CodeTableLockUnconfirmed), v.Code)
	assert.Empty(t, v.Cause, "the preflight's disk refusal was never reached")
	assert.False(t, v.WritesBlocked, "no fence went on")

	assert.Equal(t, "integer", f.columnType(t, "orders", "qty"), "the source is untouched")
	assert.Equal(t, []string{"orders", "orders_id_seq", "orders_pkey", "orders_qty_idx"}, f.relations(t),
		"no shadow table was built")
	f.assertNoCheckpointRow(t, "orders")
}

// switchedRoleURL is the fixture's URL with the role connection parameter
// set, so every session logs in as the URL's user and then runs as role.
// pgx's own parse confirms the parameter reaches the startup packet.
func (f copySwapFixture) switchedRoleURL(t *testing.T, role string) string {
	t.Helper()
	u, err := url.Parse(f.cfg.URL)
	require.NoError(t, err)
	query := u.Query()
	query.Set("role", role)
	u.RawQuery = query.Encode()
	parsed, err := pgx.ParseConfig(u.String())
	require.NoError(t, err)
	require.Equal(t, role, parsed.RuntimeParams["role"], "the role parameter rides the startup packet")
	return u.String()
}

// loginURL is the fixture's URL logging in as user with password.
func (f copySwapFixture) loginURL(t *testing.T, user, password string) string {
	t.Helper()
	u, err := url.Parse(f.cfg.URL)
	require.NoError(t, err)
	u.User = url.UserPassword(user, password)
	return u.String()
}
