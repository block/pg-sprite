package checksum_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// verifierFixture is a throwaway schema on a superuser pool with the real
// shadow builder and copier in front of the verifier, so every pass below
// compares a shadow the builder proved and the copier filled. The superuser
// is a SET-usable member of every role, so the copy-and-swap proof is minted
// without provisioning.
type verifierFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
}

func newVerifierFixture(t *testing.T) verifierFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return verifierFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f verifierFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema))
	require.NoError(t, err)
}

// rows is the size of every orders table below: keys 1..rows with qty = key.
const rows = 2500

// chunkRows sizes every pass below to fixed thousand-row chunks, so with
// rows keys the chunks are [MinInt64, 1000], [1001, 2000], and
// [2001, MaxInt64] whatever the chunk timing feedback says.
var chunkRows = copier.ChunkerOptions{InitialRows: 1000, MinRows: 1000, MaxRows: 1000}

// createOrders creates the orders table every pass below compares, holding
// keys 1..rows with qty = key and a note naming the order.
func (f verifierFixture) createOrders(t *testing.T) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.orders (
			id bigint PRIMARY KEY,
			qty integer NOT NULL,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.orders (id, qty, note)
		SELECT n, n, 'order ' || n FROM generate_series(1, %d) AS n`, rows))
}

// prove mints the copy-and-swap proof for table.
func (f verifierFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	role, err := preflight.CheckPrivileges(t.Context(), f.pool, f.schema, table, preflight.Requirement{Tier: preflight.TierCopyAndSwap})
	require.NoError(t, err)
	target, err := preflight.CheckCopySwapShape(t.Context(), f.pool, f.schema, table, role)
	require.NoError(t, err)
	return target
}

// lock acquires the per-table lock the verifier requires and releases it
// when the test ends.
func (f verifierFixture) lock(t *testing.T, table string, options ...dbconn.TableLockOption) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table, options...)
	require.NoError(t, err)
	t.Cleanup(func() {
		// A test that deliberately loses the lock has already seen Release's
		// invariant error through Err; a clean test releases cleanly.
		if lock.Err() == nil {
			assert.NoError(t, lock.Release(context.WithoutCancel(t.Context())))
		}
	})
	return lock
}

// build runs the shadow builder for target under lock with the given
// change, where %s stands for the schema-qualified table.
func (f verifierFixture) build(t *testing.T, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, alterSQL string) schemachange.BuiltShadow {
	t.Helper()
	alter, err := statement.ParseOne(fmt.Sprintf(alterSQL, pgx.Identifier{f.schema, target.Table()}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)
	return shadow
}

// copy fills the shadow from the source, so the pass compares a shadow the
// copier landed.
func (f verifierFixture) copy(t *testing.T, target preflight.CopySwapTarget, shadow schemachange.BuiltShadow, lock *dbconn.TableLockSession) {
	t.Helper()
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 2, Chunker: chunkRows})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))
	require.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}

// prepare creates, proves, locks, builds the shadow of, and copies an orders
// table whose schema change drops the note column, so the copy columns are
// narrower than the source.
func (f verifierFixture) prepare(t *testing.T) (preflight.CopySwapTarget, *dbconn.TableLockSession, schemachange.BuiltShadow) {
	t.Helper()
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, lock)
	return target, lock, shadow
}

// shadowName is the schema-qualified, quoted shadow table.
func (f verifierFixture) shadowName(shadow schemachange.BuiltShadow) string {
	return pgx.Identifier{f.schema, shadow.ShadowTable()}.Sanitize()
}

// verify runs one pass over the whole key space with fixed chunks.
func (f verifierFixture) verify(t *testing.T, pool *pgxpool.Pool, target preflight.CopySwapTarget, shadow schemachange.BuiltShadow, lock *dbconn.TableLockSession, through copier.Watermark) (checksum.Report, error) {
	t.Helper()
	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows})
	require.NoError(t, err)
	return v.Verify(t.Context(), pool, through)
}

func chunk(t *testing.T, lower, upper int64) copier.Chunk {
	t.Helper()
	c, err := copier.NewChunk(lower, upper)
	require.NoError(t, err)
	return c
}

// A pass over a shadow the copier filled completely reports every chunk
// clean, with the row count the source holds, and its report carries the
// watermark it verified through (CO-1). A pass asked to verify up to the
// zero watermark has nothing to compare and refuses rather than reporting
// an empty pass as clean.
func TestVerifierReportsACompleteCopyClean(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean())
	assert.Empty(t, report.Mismatches)
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), report.Through)
	assert.Equal(t, 3, report.Chunks, "thousand-row chunks over 2500 keys")
	assert.Equal(t, int64(rows), report.Rows)

	_, err = f.verify(t, f.pool, target, shadow, lock, copier.Watermark{})
	assert.ErrorIs(t, err, checksum.ErrNothingLanded)
}

// One shadow row whose value differs from the source is reported as
// exactly the chunk that holds its key, with equal row counts and
// differing hashes on the two sides; the chunks around it stay clean.
func TestVerifierLocatesAChangedShadowRow(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 1500")

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.False(t, report.Clean())
	assert.Equal(t, 3, report.Chunks)
	assert.Equal(t, int64(rows), report.Rows)
	require.Len(t, report.Mismatches, 1)
	found := report.Mismatches[0]
	assert.Equal(t, chunk(t, 1001, 2000), found.Chunk)
	assert.Equal(t, int64(1000), found.Source.Rows)
	assert.Equal(t, int64(1000), found.Shadow.Rows)
	assert.NotEqual(t, found.Source.Hash, found.Shadow.Hash)
}

// A row missing from the shadow and a row the shadow holds that the source
// does not are each reported in their own chunk, in key order, and the row
// counts on the two sides say which way each chunk differs.
func TestVerifierCountsMissingAndExtraShadowRows(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "DELETE FROM "+f.shadowName(shadow)+" WHERE id = 5")
	f.exec(t, "INSERT INTO "+f.shadowName(shadow)+" (id, qty) VALUES (2600, 2600)")

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 2)
	missing, extra := report.Mismatches[0], report.Mismatches[1]
	assert.Equal(t, chunk(t, math.MinInt64, 1000), missing.Chunk)
	assert.Equal(t, int64(1000), missing.Source.Rows)
	assert.Equal(t, int64(999), missing.Shadow.Rows)
	assert.Equal(t, chunk(t, 2001, math.MaxInt64), extra.Chunk)
	assert.Equal(t, int64(500), extra.Source.Rows)
	assert.Equal(t, int64(501), extra.Shadow.Rows)
	assert.Equal(t, int64(rows), report.Rows, "the report counts source rows")
}

// A schema change that converts a column's type compares clean: both sides
// hash the value as the shadow's type renders it (D7). The source holds
// integer 5 and the shadow numeric 5.00; without the cast the source would
// hash "5" against the shadow's "5.00" and every chunk would differ.
func TestVerifierComparesThroughTheShadowsTypes(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s ALTER COLUMN qty TYPE numeric(10,2)`)
	f.copy(t, target, shadow, lock)

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean(), "mismatches: %+v", report.Mismatches)
	assert.Equal(t, int64(rows), report.Rows)
}

// A pass compares only the keys at or below the watermark it is given: the
// chunk straddling the watermark is clamped to it, a difference below is
// found, and a difference above — in a chunk the copier has not finished —
// is not a finding (CO-1).
func TestVerifierStopsAtTheWatermark(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id IN (1400, 1600)")

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(1500))
	require.NoError(t, err)
	assert.Equal(t, copier.NewWatermark(1500), report.Through)
	assert.Equal(t, 2, report.Chunks)
	assert.Equal(t, int64(1500), report.Rows)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, chunk(t, 1001, 1500), report.Mismatches[0].Chunk, "the straddling chunk is clamped to the watermark")
	assert.Equal(t, int64(500), report.Mismatches[0].Source.Rows)
}

// The pass resolves every catalog object it names through pg_catalog, so a
// session whose search_path puts a schema of impostors first (CO-9) neither
// misreads the shadow's types nor hashes with someone else's md5: an
// impostor format_type that would make every numeric compare as text
// produces no false mismatch, and an impostor md5 that answers the same for
// every row hides no real one.
func TestVerifierIgnoresTheSessionSearchPath(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s ALTER COLUMN qty TYPE numeric(10,2)`)
	f.copy(t, target, shadow, lock)
	f.exec(t, `CREATE FUNCTION %s.md5(text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT ''impostor''::text'`)
	f.exec(t, `CREATE FUNCTION %s.format_type(oid, integer) RETURNS text LANGUAGE sql STABLE AS 'SELECT ''text''::text'`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	report, err := f.verify(t, shadowing, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean(), "mismatches: %+v", report.Mismatches)

	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 42")
	report, err = f.verify(t, shadowing, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), report.Mismatches[0].Chunk)
}

// A pass refuses to read when the source, or the shadow, has been replaced
// by another relation of the same name since the proofs were minted, or is
// gone (ST-6): a same-shaped impostor is never vouched for.
func TestVerifierRefusesReplacedRelations(t *testing.T) {
	t.Run("source replaced", func(t *testing.T) {
		f := newVerifierFixture(t)
		target, lock, shadow := f.prepare(t)
		f.exec(t, "DROP TABLE %s.orders")
		f.exec(t, "CREATE TABLE %s.orders (id bigint PRIMARY KEY, qty integer NOT NULL, note text)")

		_, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
		require.ErrorIs(t, err, checksum.ErrInvariantViolation)
		assert.Contains(t, err.Error(), "(ST-6): source")
	})
	t.Run("shadow replaced", func(t *testing.T) {
		f := newVerifierFixture(t)
		target, lock, shadow := f.prepare(t)
		f.exec(t, "DROP TABLE "+f.shadowName(shadow))
		f.exec(t, "CREATE TABLE "+f.shadowName(shadow)+" (id bigint PRIMARY KEY, qty integer NOT NULL)")

		_, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
		require.ErrorIs(t, err, checksum.ErrInvariantViolation)
		assert.Contains(t, err.Error(), "(ST-6): shadow")
	})
	t.Run("shadow dropped", func(t *testing.T) {
		f := newVerifierFixture(t)
		target, lock, shadow := f.prepare(t)
		f.exec(t, "DROP TABLE "+f.shadowName(shadow))

		_, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
		require.ErrorIs(t, err, checksum.ErrInvariantViolation)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the server's refusal to lock a missing relation is the cause")
		assert.Equal(t, "42P01", pgErr.Code, "undefined_table")
		assert.Contains(t, err.Error(), "(ST-6)")
	})
}

// The verifier trusts the lock session only as far as the server confirms
// it from the reading connection (LK-1): a session whose backend is gone
// compares nothing. A session for a different table, or one that already
// reported loss, is refused before any connection is opened.
func TestVerifierRefusesAnUnconfirmedLock(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, buildLock)
	require.NoError(t, buildLock.Release(t.Context()))

	f.exec(t, `CREATE TABLE %s.other (id bigint PRIMARY KEY)`)
	_, err := checksum.NewVerifier(target, shadow, f.lock(t, "other"), checksum.Options{})
	require.ErrorIs(t, err, checksum.ErrInvariantViolation, "a lock for another table")
	assert.Contains(t, err.Error(), "(LK-1)")

	gone := f.goneLock(t, "orders")
	_, err = f.verify(t, f.pool, target, shadow, gone, copier.NewWatermark(math.MaxInt64))
	require.ErrorIs(t, err, checksum.ErrInvariantViolation, "a gone lock session")
	assert.ErrorIs(t, err, dbconn.ErrTableLockNotHeld)

	lost := f.lostLock(t, "orders")
	_, err = checksum.NewVerifier(target, shadow, lost, checksum.Options{})
	require.ErrorIs(t, err, checksum.ErrInvariantViolation, "a session that already reported loss")
	assert.ErrorIs(t, err, lost.Err(), "the refusal names what the session saw")
	assert.Contains(t, err.Error(), "(LK-1)")
}

// Losing the table lock mid-pass cancels the read in flight and Verify
// reports the loss as an invariant violation naming what the session saw
// (LK-1), not the cancelled statement. The loss is injected from the clock
// reading the verifier takes just before its first chunk's transaction.
func TestVerifierAbortsWhenTheLockIsLostMidPass(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, buildLock)
	require.NoError(t, buildLock.Release(t.Context()))
	lock := f.lock(t, "orders", dbconn.WithTableLockKeepalive(100*time.Millisecond))
	clock := &hookedClock{hook: func() {
		f.terminateBackend(t, lock.BackendPID())
		const lockLossDeadline = 15 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
	}}

	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{Chunker: chunkRows, Clock: clock})
	require.NoError(t, err)
	_, err = v.Verify(t.Context(), f.pool, copier.NewWatermark(math.MaxInt64))
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.ErrorIs(t, err, lock.Err(), "the loss the session reported is the cause")
	assert.Contains(t, err.Error(), "(LK-1)")
}

// goneLock acquires the table lock and then terminates the session's
// backend, so the server no longer grants the lock while the session still
// believes it holds it: the default keepalive is long enough that only an
// in-transaction confirmation can catch the loss.
func (f verifierFixture) goneLock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table)
	require.NoError(t, err)
	t.Cleanup(func() {
		const lockLossDeadline = 30 * time.Second
		select {
		case <-lock.Done():
		case <-time.After(lockLossDeadline):
			t.Errorf("lock session did not report loss within %s", lockLossDeadline)
		}
		assert.Error(t, lock.Release(context.WithoutCancel(t.Context())))
	})
	f.terminateBackend(t, lock.BackendPID())
	require.NoError(t, lock.Err(), "the keepalive has not yet noticed the loss; the in-transaction check must")
	return lock
}

// lostLock acquires the table lock with a short keepalive, terminates the
// session's backend, and waits until the session itself has reported the
// loss, so Err is set before the verifier ever sees the session.
func (f verifierFixture) lostLock(t *testing.T, table string) *dbconn.TableLockSession {
	t.Helper()
	lock, err := dbconn.AcquireTableLock(t.Context(), f.cfg, f.schema, table, dbconn.WithTableLockKeepalive(200*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { assert.Error(t, lock.Release(context.WithoutCancel(t.Context()))) })
	f.terminateBackend(t, lock.BackendPID())
	const lockLossDeadline = 30 * time.Second
	select {
	case <-lock.Done():
	case <-time.After(lockLossDeadline):
		t.Fatalf("lock session did not report loss within %s", lockLossDeadline)
	}
	require.Error(t, lock.Err())
	return lock
}

func (f verifierFixture) terminateBackend(t *testing.T, pid uint32) {
	t.Helper()
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated))
	require.True(t, terminated)
	const backendExitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive))
		return !alive
	}, backendExitDeadline, 50*time.Millisecond, "terminated backend should leave pg_stat_activity")
}

// hookedClock is a Clock whose first reading runs a hook. The verifier
// reads the clock once before each chunk's transaction begins, so the hook
// lands after the column-type read has committed and before the first
// chunk's digest transaction opens.
type hookedClock struct {
	once sync.Once
	hook func()
}

func (c *hookedClock) Now() time.Time {
	c.once.Do(c.hook)
	return time.Now()
}
