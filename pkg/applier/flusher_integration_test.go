package applier_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
)

// flushFixture is a throwaway schema on a superuser pool with the real
// shadow builder and copier in front of the flusher, so every flush below
// writes into a shadow the builder proved and the copier filled. The
// superuser is a SET-usable member of every role, so the copy-and-swap proof
// is minted without provisioning. There is no decoder in front of the
// buffer yet: each test states the source's change as SQL, then hands the
// flusher the batch the buffer would have drained for it.
type flushFixture struct {
	cfg    dbconn.Config
	pool   *pgxpool.Pool
	schema string
}

func newFlushFixture(t *testing.T) flushFixture {
	t.Helper()
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return flushFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
}

// exec runs SQL with %s standing for the fixture schema.
func (f flushFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), strings.ReplaceAll(sql, "%s", f.schema), args...)
	require.NoError(t, err)
}

// createOrders creates an orders table holding keys 1..rows with qty = key
// and a note; the note is the column the schema change drops, so the copy
// column list is narrower than the source.
func (f flushFixture) createOrders(t *testing.T, rows int64) {
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

// createSeats creates the CO-6 table: a unique secondary key whose values
// tests move between rows, and a document column that is always stored out
// of line — EXTERNAL turns compression off, so the value cannot stay inline
// — which an UPDATE leaving it unchanged decodes as a marker. The note is
// the column the schema change drops.
func (f flushFixture) createSeats(t *testing.T, rows int64) {
	t.Helper()
	f.exec(t, `
		CREATE TABLE %s.seats (
			id bigint PRIMARY KEY,
			slot text UNIQUE,
			doc text,
			note text
		)`)
	f.exec(t, `ALTER TABLE %s.seats ALTER COLUMN doc SET STORAGE EXTERNAL`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.seats (id, slot, doc, note)
		SELECT n, chr(64 + n::integer), repeat(encode(sha256(convert_to(n::text, 'UTF8')), 'hex'), 150), 'seat ' || n
		FROM generate_series(1, %d) AS n`, rows))
	var toastBytes int64
	require.NoError(t, f.pool.QueryRow(t.Context(),
		`SELECT coalesce(pg_relation_size(reltoastrelid), 0) FROM pg_class WHERE oid = $1::regclass`,
		pgx.Identifier{f.schema, "seats"}.Sanitize()).Scan(&toastBytes))
	require.Positive(t, toastBytes, "the document column is stored out of line, so an unchanged value decodes as a marker")
}

// prove mints the copy-and-swap proof for table.
func (f flushFixture) prove(t *testing.T, table string) preflight.CopySwapTarget {
	t.Helper()
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, table, preflight.CopySwapEnvironment{FreeDiskBytes: testutil.UnlimitedDisk})
	require.NoError(t, err)
	return target
}

// lock acquires the per-table lock the flusher requires and releases it
// when the test ends.
func (f flushFixture) lock(t *testing.T, table string, options ...dbconn.TableLockOption) *dbconn.TableLockSession {
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
func (f flushFixture) build(t *testing.T, lock *dbconn.TableLockSession, target preflight.CopySwapTarget, alterSQL string) schemachange.BuiltShadow {
	t.Helper()
	alter, err := statement.ParseOne(fmt.Sprintf(alterSQL, pgx.Identifier{f.schema, target.Table()}.Sanitize()))
	require.NoError(t, err)
	shadow, err := schemachange.BuildShadow(t.Context(), f.pool, lock, target, alter, schemachange.Options{})
	require.NoError(t, err)
	return shadow
}

// copy fills the shadow from the source, so every flush below writes into a
// shadow the copier landed.
func (f flushFixture) copy(t *testing.T, target preflight.CopySwapTarget, shadow schemachange.BuiltShadow, lock *dbconn.TableLockSession) {
	t.Helper()
	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{Workers: 1})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), f.pool))
	require.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
}

// prepared is a table proved, locked, shadowed with its note column
// dropped, copied, and fronted by a flusher.
type prepared struct {
	target preflight.CopySwapTarget
	lock   *dbconn.TableLockSession
	shadow schemachange.BuiltShadow
	flush  *applier.Flusher
}

// prepare proves, locks, builds the shadow of, and copies table, whose
// schema change drops the note column, so the copy columns are narrower
// than the source.
func (f flushFixture) prepare(t *testing.T, table string) prepared {
	t.Helper()
	target := f.prove(t, table)
	lock := f.lock(t, table)
	shadow := f.build(t, lock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, lock)
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	return prepared{target: target, lock: lock, shadow: shadow, flush: flush}
}

// assertConverged compares the source with its shadow on the columns the
// shadow kept.
func (f flushFixture) assertConverged(t *testing.T, shadow schemachange.BuiltShadow) {
	t.Helper()
	testutil.AssertConverged(t, f.pool,
		testutil.RelationRef{Schema: f.schema, Table: shadow.SourceTable()},
		testutil.RelationRef{Schema: f.schema, Table: shadow.ShadowTable()},
		testutil.ConvergeOptions{IgnoreColumns: []string{"note"}})
}

// text reads one column of one row of table as text; absent is false when
// the row does not exist.
func (f flushFixture) text(t *testing.T, table, column string, key int64) (value *string, present bool) {
	t.Helper()
	err := f.pool.QueryRow(t.Context(),
		"SELECT "+pgx.Identifier{column}.Sanitize()+"::text FROM "+pgx.Identifier{f.schema, table}.Sanitize()+" WHERE id = $1", key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false
	}
	require.NoError(t, err)
	return value, true
}

func (f flushFixture) count(t *testing.T, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{f.schema, table}.Sanitize()).Scan(&n))
	return n
}

// Batch construction: what the buffer would have drained for the source
// change each test states. Column values are the text pgoutput carries.

func col(name string, value any) decode.Column {
	return decode.Column{Name: name, Value: value, Present: true}
}
func marker(name string) decode.Column { return decode.Column{Name: name, Present: false} }

func image(key int64, cols ...decode.Column) applier.Entry {
	return applier.Entry{Key: key, Kind: applier.Image, Columns: cols, FirstLSN: decode.LSN(key)}
}

func moved(from, to int64, cols ...decode.Column) applier.Entry {
	e := image(to, cols...)
	e.OldKey = &from
	return e
}

func deleted(key int64) applier.Entry {
	return applier.Entry{Key: key, Kind: applier.DeleteMarker, FirstLSN: decode.LSN(key)}
}

func batch(entries ...applier.Entry) applier.Batch { return applier.Batch{Entries: entries} }

// The primary path: one transaction, every entry once, by its kind — an
// updated row's present columns, a deleted row's removal, an inserted row
// whole — and the shadow converges with the source (CO-5).
func TestFlushAppliesABatchColumnWise(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 100)
	p := f.prepare(t, "orders")
	f.exec(t, `UPDATE %s.orders SET qty = 500 WHERE id = 5`)
	f.exec(t, `DELETE FROM %s.orders WHERE id = 7`)
	f.exec(t, `INSERT INTO %s.orders (id, qty, note) VALUES (101, 101, 'order 101')`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(5, col("qty", "500"), col("note", "order 5")),
		deleted(7),
		image(101, col("qty", "101"), col("note", "order 101")),
	))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 2, Deletes: 1}, result)
	f.assertConverged(t, p.shadow)
	assert.Equal(t, int64(100), f.count(t, p.shadow.ShadowTable()))
}

// An empty batch opens no transaction and writes nothing.
func TestFlushOfAnEmptyBatchIsANoOp(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 10)
	p := f.prepare(t, "orders")
	result, err := p.flush.Flush(t.Context(), f.pool, applier.Batch{})
	require.NoError(t, err)
	assert.Equal(t, applier.Result{}, result)
	f.assertConverged(t, p.shadow)
}

// An UPDATE that left the out-of-line document unchanged decodes it as a
// marker; the flush writes the present column and leaves the document in
// place, byte for byte, without reading it (CO-8).
func TestFlushLeavesAnOmittedColumnInPlace(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")
	before, _ := f.text(t, "seats", "doc", 1)
	f.exec(t, `UPDATE %s.seats SET slot = 'Z' WHERE id = 1`)

	result, err := p.flush.Flush(t.Context(), f.pool, batch(image(1, col("slot", "Z"), marker("doc"))))

	require.NoError(t, err)
	assert.Equal(t, applier.Result{Images: 1}, result, "no completion read: the marker column is simply not written")
	after, _ := f.text(t, p.shadow.ShadowTable(), "doc", 1)
	assert.Equal(t, *before, *after, "the document the marker stood for is untouched")
	f.assertConverged(t, p.shadow)
}

// A marker stands for a value in the shadow row under the image's own key;
// an image that did not move and finds no row there is the protocol error
// D13 names, and the whole batch is refused: the drain's deferral rule
// means the row would have been there (CO-8, CO-4).
func TestFlushRefusesAMarkerForARowTheShadowLacks(t *testing.T) {
	f := newFlushFixture(t)
	f.createSeats(t, 3)
	p := f.prepare(t, "seats")

	_, err := p.flush.Flush(t.Context(), f.pool, batch(
		image(2, col("slot", "Y")),
		image(50, col("slot", "Z"), marker("doc")),
	))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(CO-8): flush key 50: image omits [doc]")
	slot, _ := f.text(t, p.shadow.ShadowTable(), "slot", 2)
	assert.Equal(t, "B", *slot, "nothing in the batch was written")
	_, present := f.text(t, p.shadow.ShadowTable(), "slot", 50)
	assert.False(t, present)
}

// The flush runs under the source owner's role, not the connected role
// (SET LOCAL ROLE owner): a shadow column defaulting to current_user
// records the owner on a row the flush inserts, so a shadow the builder made
// owner-correct stays owner-correct through catch-up.
func TestFlushWritesAsTheTableOwner(t *testing.T) {
	cfg := dbconn.Config{URL: testutil.StartPostgres(t)}
	pool, err := dbconn.NewPool(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	owner := testutil.NewRole(t, pool, "NOLOGIN")
	f := flushFixture{cfg: cfg, pool: pool, schema: testutil.NewSchema(t, pool)}
	f.exec(t, "GRANT USAGE, CREATE ON SCHEMA %s TO "+pgx.Identifier{owner}.Sanitize())
	f.createOrders(t, 10)
	f.exec(t, "ALTER TABLE %s.orders OWNER TO "+pgx.Identifier{owner}.Sanitize())
	target := f.prove(t, "orders")
	require.Equal(t, owner, target.OwnerRole())
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `
		ALTER TABLE %s
			ADD COLUMN writer name NOT NULL DEFAULT current_user`)
	f.copy(t, target, shadow, lock)
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)
	f.exec(t, `INSERT INTO %s.orders (id, qty, note) VALUES (11, 11, 'order 11')`)

	_, err = flush.Flush(t.Context(), f.pool, batch(image(11, col("qty", "11"), col("note", "order 11"))))

	require.NoError(t, err)
	writer, _ := f.text(t, shadow.ShadowTable(), "writer", 11)
	assert.Equal(t, owner, *writer, "the flushed row was written as the owner, not as the connected superuser")
}

// Every flush confirms, from its own transaction, that the shadow is still
// the relation the proof was minted for (ST-6): a shadow replaced by a
// same-shaped impostor receives nothing.
func TestFlushRefusesAReplacedShadow(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 10)
	p := f.prepare(t, "orders")
	shadowName := pgx.Identifier{f.schema, p.shadow.ShadowTable()}.Sanitize()
	f.exec(t, "DROP TABLE "+shadowName)
	f.exec(t, "CREATE TABLE "+shadowName+" (id bigint PRIMARY KEY, qty integer NOT NULL)")

	_, err := p.flush.Flush(t.Context(), f.pool, batch(image(1, col("qty", "100"))))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(ST-6): shadow")
	assert.Equal(t, int64(0), f.count(t, p.shadow.ShadowTable()), "the impostor receives nothing")
}

// A flush whose lock session's backend is gone finds that out from its own
// transaction and refuses to write (LK-1): the keepalive has not noticed yet,
// so the in-transaction confirmation is what catches it.
func TestFlushRefusesWhenTheTableLockIsGone(t *testing.T) {
	f := newFlushFixture(t)
	f.createOrders(t, 10)
	target := f.prove(t, "orders")
	buildLock := f.lock(t, "orders")
	shadow := f.build(t, buildLock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, buildLock)
	require.NoError(t, buildLock.Release(t.Context()))
	lock := f.goneLock(t, "orders")
	flush, err := applier.NewFlusher(target, shadow, lock, applier.Options{})
	require.NoError(t, err)

	_, err = flush.Flush(t.Context(), f.pool, batch(image(1, col("qty", "100"))))

	require.ErrorIs(t, err, applier.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(LK-1)")
	qty, _ := f.text(t, shadow.ShadowTable(), "qty", 1)
	assert.Equal(t, "1", *qty, "nothing was written")
}

// goneLock acquires the table lock and then terminates the session's
// backend, so the server no longer grants the lock while the session still
// believes it holds it.
func (f flushFixture) goneLock(t *testing.T, table string) *dbconn.TableLockSession {
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
	var terminated bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, lock.BackendPID()).Scan(&terminated))
	require.True(t, terminated)
	const backendExitDeadline = 10 * time.Second
	require.Eventually(t, func() bool {
		var alive bool
		require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)`, lock.BackendPID()).Scan(&alive))
		return !alive
	}, backendExitDeadline, 50*time.Millisecond, "terminated backend should leave pg_stat_activity")
	require.NoError(t, lock.Err(), "the keepalive has not yet noticed the loss; the in-transaction check must")
	return lock
}
