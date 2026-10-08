package dbconn_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
)

// A replication connection is a WAL sender on the configured database: it
// answers IDENTIFY_SYSTEM with that database's name and shows up in
// pg_stat_replication under the engine's application_name, so an operator
// can tell the engine's sender from any other.
func TestConnectReplicationOpensAWALSenderOnTheConfiguredDatabase(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	databaseURL := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var database string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database))

	conn, err := dbconn.ConnectReplication(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close(context.WithoutCancel(t.Context()))) })

	identity, err := pglogrepl.IdentifySystem(t.Context(), conn)
	require.NoError(t, err)
	assert.Equal(t, database, identity.DBName)

	const senderVisible = 10 * time.Second
	assert.EventuallyWithT(t, func(collect *assert.CollectT) {
		var applicationName string
		err := pool.QueryRow(t.Context(),
			`SELECT application_name FROM pg_stat_replication WHERE pid = $1`, conn.PID()).Scan(&applicationName)
		if !assert.NoError(collect, err, "the replication connection must appear in pg_stat_replication") {
			return
		}
		assert.Equal(collect, "pg-sprite", applicationName)
	}, senderVisible, 50*time.Millisecond)
}

// The BeforeConnect hook runs on the replication dial exactly as on a pooled
// one, so a credential provider wired into Config reaches the walsender too.
func TestConnectReplicationAppliesBeforeConnect(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	databaseURL := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := dbconn.Config{URL: databaseURL, BeforeConnect: func(_ context.Context, cc *pgx.ConnConfig) error {
		cc.RuntimeParams["application_name"] = "pg-sprite-probe"
		return nil
	}}
	conn, err := dbconn.ConnectReplication(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close(context.WithoutCancel(t.Context()))) })

	const senderVisible = 10 * time.Second
	assert.EventuallyWithT(t, func(collect *assert.CollectT) {
		var applicationName string
		err := pool.QueryRow(t.Context(),
			`SELECT application_name FROM pg_stat_replication WHERE pid = $1`, conn.PID()).Scan(&applicationName)
		if !assert.NoError(collect, err, "the replication connection must appear in pg_stat_replication") {
			return
		}
		assert.Equal(collect, "pg-sprite-probe", applicationName)
	}, senderVisible, 50*time.Millisecond)
}

// The replication connection asks for warnings itself: a database set to
// send its sessions only errors cannot keep the walsender's warning — the
// one word that it will withhold changes — from the connection.
func TestConnectReplicationAsksForWarningsOverTheDatabaseSetting(t *testing.T) {
	serverURL := testutil.StartPostgres(t)
	databaseURL := testutil.NewDatabase(t, serverURL)
	pool, err := dbconn.NewPool(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var database string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&database))
	_, err = pool.Exec(t.Context(), `ALTER DATABASE `+pgx.Identifier{database}.Sanitize()+` SET client_min_messages = error`)
	require.NoError(t, err)

	conn, err := dbconn.ConnectReplication(t.Context(), dbconn.Config{URL: databaseURL})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close(context.WithoutCancel(t.Context()))) })

	results, err := conn.Exec(t.Context(), `SHOW client_min_messages`).ReadAll()
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Rows, 1)
	assert.Equal(t, "warning", string(results[0].Rows[0][0]))
}
