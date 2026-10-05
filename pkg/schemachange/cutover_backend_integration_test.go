package schemachange_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/schemachange"
)

// unreachableConn models a client that stops hearing from the server: once
// lose is set, a CancelRequest never reaches the server and, with
// eofOnCommit, the COMMIT it forwards is answered with EOF on the client
// side. The backend that received the COMMIT keeps running, as it does
// behind a dropped link.
type unreachableConn struct {
	net.Conn
	lose        *atomic.Bool
	eofOnCommit bool
	sent        atomic.Bool
}

// cancelRequestCode is the protocol code of a CancelRequest message, which
// travels on a connection of its own.
const cancelRequestCode = 80877102

func (c *unreachableConn) Write(b []byte) (int, error) {
	if c.lose.Load() && len(b) == 16 && binary.BigEndian.Uint32(b[4:8]) == cancelRequestCode {
		if err := c.Close(); err != nil {
			return 0, err
		}
		c.sent.Store(true)
		return len(b), nil
	}
	n, err := c.Conn.Write(b)
	if c.lose.Load() && c.eofOnCommit && isSimpleQuery(b, "commit") {
		c.sent.Store(true)
	}
	return n, err
}

func (c *unreachableConn) Read(b []byte) (int, error) {
	if c.sent.Load() {
		return 0, io.EOF
	}
	return c.Conn.Read(b)
}

// isSimpleQuery reports whether the bytes are one simple-protocol Query
// message carrying the given SQL.
func isSimpleQuery(b []byte, sql string) bool {
	if len(b) < 6 || b[0] != 'Q' {
		return false
	}
	return strings.EqualFold(strings.TrimRight(string(b[5:]), "\x00"), sql)
}

// unreachablePool is a pool on the fixture's server whose connections stop
// hearing from it once lose is set.
func (f shadowFixture) unreachablePool(t *testing.T, lose *atomic.Bool, eofOnCommit bool) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	dialer := &net.Dialer{}
	pc.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &unreachableConn{Conn: c, lose: lose, eofOnCommit: eofOnCommit}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// slowCommit makes the drain's write hold COMMIT open through a deferred
// constraint trigger that sleeps, so the swap's backend is still deciding
// after the client has stopped hearing from it.
func (f shadowFixture) slowCommit(t *testing.T, hold time.Duration) schemachange.DrainFunc {
	t.Helper()
	f.exec(t, `CREATE TABLE %s.commit_probe (x int)`)
	f.exec(t, `
		CREATE FUNCTION %s.slow_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(`+strconv.FormatFloat(hold.Seconds(), 'f', -1, 64)+`);
			RETURN NULL;
		END $$`)
	f.exec(t, `
		CREATE CONSTRAINT TRIGGER slow_commit AFTER INSERT ON %s.commit_probe
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s.slow_commit()`)
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+pgx.Identifier{f.schema, "commit_probe"}.Sanitize()+` VALUES (1)`)
		return err
	}
}

// waitForCommitToFinish waits until no backend is still running a COMMIT.
func (f shadowFixture) waitForCommitToFinish(t *testing.T) {
	t.Helper()
	const commitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var running int
		require.NoError(t, f.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE state = 'active' AND query ILIKE 'commit%' AND pid <> pg_backend_pid()`).Scan(&running))
		return running == 0
	}, commitDeadline, 50*time.Millisecond)
}

// The swap's answer must match what the server did with the COMMIT, even
// when the client stopped hearing from the server while it was still
// committing (LK-4). The link is cut as COMMIT goes out, so the server
// finishes the commit on its own while the client sees EOF; the swap
// follows the backend to its exit, reads the catalog, and reports the
// swap as the success it was — with the identities the live table carries.
func TestCutoverReportsACommitTheClientNeverHeardOf(t *testing.T) {
	f := newShadowFixture(t)
	f.exec(t, `
		CREATE TABLE %s.tickets (
			id  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			qty integer NOT NULL
		)`)
	f.exec(t, `INSERT INTO %s.tickets (qty) SELECT g FROM generate_series(1, 100) g`)
	write := f.slowCommit(t, 2*time.Second)
	s := f.stage(t, "tickets", `ALTER TABLE %s.tickets ALTER COLUMN qty TYPE bigint`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	var lose atomic.Bool
	pool := f.unreachablePool(t, &lose, true)

	drain := func(ctx context.Context, tx pgx.Tx) error {
		lose.Store(true)
		return write(ctx, tx)
	}
	swapped, err := schemachange.Cutover(t.Context(), pool, s.lock, ready, drain, schemachange.CutoverOptions{})
	f.waitForCommitToFinish(t)

	require.NoError(t, err, "the server committed, so the swap is reported as a success")
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "tickets"))
	assert.Equal(t, 1, swapped.Attempts())
	assert.Equal(t, s.built.IdentityColumns(), swapped.IdentityColumns(), "the identities were read back from the live table")
	assert.Equal(t, f.schema+".tickets_id_seq", f.serialSequence(t, "tickets", "id"))
}

// A context that ends while COMMIT is in flight is no more an answer than a
// lost link: pgx's cancel request may never reach the server, which
// finishes the commit on its own. The swap must not report the plain
// context error as a rollback; it follows the backend and reads the
// outcome, and whichever way the server decided, the answer matches the
// catalog — a rollback carries ErrCutoverRolledBack, a commit a
// SwappedTable (LK-4).
func TestCutoverReportsTheOutcomeWhenTheContextEndsDuringCommit(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	write := f.slowCommit(t, 2*time.Second)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	var lose atomic.Bool
	pool := f.unreachablePool(t, &lose, false)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	drain := func(ctx context.Context, tx pgx.Tx) error {
		lose.Store(true)
		time.AfterFunc(time.Second, cancel)
		return write(ctx, tx)
	}
	swapped, err := schemachange.Cutover(ctx, pool, s.lock, ready, drain, schemachange.CutoverOptions{})
	f.waitForCommitToFinish(t)

	if err != nil {
		require.ErrorIs(t, err, schemachange.ErrCutoverRolledBack, "a failure that leaves the source live says so")
		assert.Equal(t, s.built.SourceOID(), f.relationOID(t, "orders"), "reported rolled back, so the source must still be live")
		return
	}
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "reported swapped, so the shadow must be live")
	assert.Equal(t, 1, swapped.Attempts())
}

// An attempt whose backend is still running when the wait for it runs out
// is refused as ambiguous rather than read: a catalog read while the
// backend is deciding could report either answer (LK-4). The wait is
// bounded by the attempt's own lock_timeout and statement_timeout, here
// shorter than the COMMIT the trigger holds open; the injected Sleep
// counts the polls without waiting on the wall clock.
func TestCutoverRefusesToReadWhileTheLostAttemptIsStillRunning(t *testing.T) {
	f := newShadowFixture(t)
	f.createOrders(t)
	write := f.slowCommit(t, 3*time.Second)
	s := f.stage(t, "orders", `ALTER TABLE %s.orders DROP COLUMN note`)
	ready, err := f.gate(t, s)
	require.NoError(t, err)
	var lose atomic.Bool
	pool := f.unreachablePool(t, &lose, true)

	var polls int
	opts := schemachange.CutoverOptions{
		Options: schemachange.Options{LockTimeout: 100 * time.Millisecond, StatementTimeout: 10 * time.Second},
		Sleep: func(context.Context, time.Duration) error {
			polls++
			return nil
		},
	}
	drain := func(ctx context.Context, tx pgx.Tx) error {
		lose.Store(true)
		return write(ctx, tx)
	}
	_, err = schemachange.Cutover(t.Context(), pool, s.lock, ready, drain, opts)

	assert.Equal(t, schemachange.CauseOutcomeAmbiguous, schemachange.RefusalCauseOf(err))
	assert.NotErrorIs(t, err, schemachange.ErrCutoverRolledBack, "an ambiguous outcome never claims the source is live")
	assert.Positive(t, polls, "the backend was polled before the refusal")
	f.waitForCommitToFinish(t)
	assert.Equal(t, s.built.ShadowOID(), f.relationOID(t, "orders"), "the server did commit; the swap simply could not know yet")
}
