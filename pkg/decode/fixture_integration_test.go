package decode_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
)

// slotFixture is one database on a cluster that decodes WAL, holding a
// populated table and the copy-and-swap target proof minted for it, so a
// test can create the route's slot exactly as the orchestrator will.
type slotFixture struct {
	serverURL   string
	databaseURL string
	cfg         dbconn.Config
	pool        *pgxpool.Pool
	schema      string
	target      preflight.CopySwapTarget
}

// newSlotFixture starts a dedicated server at wal_level = logical — the
// shared harness does not decode WAL — and mints the target on a database
// of its own, so every slot name in the run belongs to this test.
func newSlotFixture(t *testing.T) slotFixture {
	t.Helper()
	serverURL := testutil.StartPostgresWithSettings(t, "wal_level=logical")
	return newSlotFixtureOn(t, serverURL, testutil.NewDatabase(t, serverURL))
}

func newSlotFixtureOn(t *testing.T, serverURL, databaseURL string) slotFixture {
	t.Helper()
	cfg := dbconn.Config{URL: databaseURL}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema := testutil.NewSchema(t, pool)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %s.ledger (
			id bigint PRIMARY KEY,
			note text
		)`, schema))
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		INSERT INTO %s.ledger (id, note)
		SELECT g, 'row ' || g FROM generate_series(1, 100) AS g`, schema))
	require.NoError(t, err)

	f := slotFixture{serverURL: serverURL, databaseURL: databaseURL, cfg: cfg, pool: pool, schema: schema}
	f.target = f.mintTarget(t, pool, true)
	return f
}

// mintTarget runs the preflight chain for the fixture's table as the pool's
// role — privileges, shape, environment — and returns the target proof; a
// quiesced run's target is minted with logicalDecoding false.
func (f slotFixture) mintTarget(t *testing.T, pool *pgxpool.Pool, logicalDecoding bool) preflight.CopySwapTarget {
	t.Helper()
	req := preflight.Requirement{Tier: preflight.TierCopyAndSwap, LogicalDecoding: logicalDecoding}
	role, err := preflight.CheckPrivileges(t.Context(), pool, f.schema, "ledger", req)
	require.NoError(t, err)
	shape, err := preflight.CheckCopySwapShape(t.Context(), pool, f.schema, "ledger", role)
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapEnvironment(t.Context(), pool, shape,
		preflight.CopySwapEnvironment{LogicalDecoding: logicalDecoding, FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	return target
}

// withCredentials is databaseURL authenticated as role instead.
func withCredentials(t *testing.T, databaseURL, role, password string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	require.NoError(t, err)
	u.User = url.UserPassword(role, password)
	return u.String()
}

// createSlot creates the route's slot and arranges for it to be closed and
// dropped after the test, whatever the test does with it in between.
func (f slotFixture) createSlot(t *testing.T) *decode.Slot {
	t.Helper()
	slot, err := decode.CreateSlot(t.Context(), f.cfg, f.pool, f.target)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		// A test that already closed the slot leaves a closed connection
		// behind, whose second close is a no-op.
		if err := slot.Close(ctx); err != nil {
			t.Logf("close slot: %v", err)
		}
		assert.NoError(t, decode.DropSlot(ctx, f.cfg, f.pool, slot.Name()))
	})
	return slot
}

// ledgerRows counts the table's rows as one transaction sees them.
func (f slotFixture) ledgerRows(t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var n int64
	require.NoError(t, tx.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s.ledger`, f.schema)).Scan(&n))
	return n
}

// importSnapshot opens a REPEATABLE READ transaction on the fixture's
// database reading from the named exported snapshot.
func (f slotFixture) importSnapshot(t *testing.T, snapshot string) (pgx.Tx, error) {
	t.Helper()
	tx, err := f.pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Rolling back a transaction the test already ended is a no-op.
		if err := tx.Rollback(context.WithoutCancel(t.Context())); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Logf("roll back snapshot transaction: %v", err)
		}
	})
	_, err = tx.Exec(t.Context(), `SET TRANSACTION SNAPSHOT `+quoteLiteral(snapshot))
	return tx, err
}

// publishedTables lists what the named publication publishes, as
// schema.table, so a test can check the publication is exactly the target.
func (f slotFixture) publishedTables(t *testing.T, name string) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `
		SELECT schemaname || '.' || tablename
		FROM pg_catalog.pg_publication_tables
		WHERE pubname = $1
		ORDER BY 1`, name)
	require.NoError(t, err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return tables
}

// publicationExists reports whether a publication of the name is in the
// catalog, regardless of what it publishes.
func (f slotFixture) publicationExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_publication WHERE pubname = $1)`, name).Scan(&exists))
	return exists
}

// currentWALLSN is the server's write position.
func (f slotFixture) currentWALLSN(t *testing.T) decode.LSN {
	t.Helper()
	var text string
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_current_wal_lsn()::text`).Scan(&text))
	lsn, err := decode.ParseLSN(text)
	require.NoError(t, err)
	return lsn
}

// holdSlot opens a second replication connection and starts streaming from
// the slot, which is what makes a slot active: the walsender holds it until
// the connection closes. The returned closer releases it.
func (f slotFixture) holdSlot(t *testing.T, slot *decode.Slot) func() {
	t.Helper()
	holder, err := dbconn.ConnectReplication(t.Context(), f.cfg)
	require.NoError(t, err)
	err = pglogrepl.StartReplication(t.Context(), holder, slot.Name(), pglogrepl.LSN(slot.ConsistentPoint()),
		pglogrepl.StartReplicationOptions{
			Mode:       pglogrepl.LogicalReplication,
			PluginArgs: []string{"proto_version '1'", "publication_names " + quoteLiteral(slot.Name())},
		})
	require.NoError(t, err)
	const slotHeld = 10 * time.Second
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, found, err := decode.InspectSlot(t.Context(), f.pool, slot.Name())
		if !assert.NoError(collect, err) || !assert.True(collect, found) {
			return
		}
		assert.True(collect, status.Active, "the streaming walsender must hold the slot")
	}, slotHeld, 50*time.Millisecond)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		assert.NoError(t, holder.Close(context.WithoutCancel(t.Context())))
	}
	t.Cleanup(release)
	return release
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// requireSQLState asserts err is the PostgreSQL error with the SQLSTATE.
func requireSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, code, pgErr.Code)
}
