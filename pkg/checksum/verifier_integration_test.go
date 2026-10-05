package checksum_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
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
	// The volume is not measured in tests; an unbounded free-disk figure
	// admits the environment check so the proof under test is the shape.
	target, err := preflight.CheckCopySwap(t.Context(), f.pool, f.schema, table, preflight.CopySwapEnvironment{FreeDiskBytes: math.MaxInt64})
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

// A chunk the shadow holds no rows of is a mismatch, not a failed pass: the
// empty side digests as zero rows and the SHA-256 of the empty input, so
// the report names the chunk and the policy decides what happens next.
func TestVerifierReportsAChunkTheShadowIsMissingEntirely(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "DELETE FROM "+f.shadowName(shadow)+" WHERE id > 2000")

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	empty := report.Mismatches[0]
	assert.Equal(t, chunk(t, 2001, math.MaxInt64), empty.Chunk)
	assert.Equal(t, int64(500), empty.Source.Rows)
	assert.Equal(t, int64(0), empty.Shadow.Rows)
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", empty.Shadow.Hash,
		"the SHA-256 of the empty input")
}

// Rows missing from the shadow and a row the shadow holds that the source
// does not are each reported in their own chunk, in key order, and the row
// counts on the two sides say which way each chunk differs. Two rows go
// missing and one is added, so the shadow holds fewer rows than the source
// and the report's row count can only be the source's.
func TestVerifierCountsMissingAndExtraShadowRows(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "DELETE FROM "+f.shadowName(shadow)+" WHERE id IN (5, 6)")
	f.exec(t, "INSERT INTO "+f.shadowName(shadow)+" (id, qty) VALUES (2600, 2600)")

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 2)
	missing, extra := report.Mismatches[0], report.Mismatches[1]
	assert.Equal(t, chunk(t, math.MinInt64, 1000), missing.Chunk)
	assert.Equal(t, int64(1000), missing.Source.Rows)
	assert.Equal(t, int64(998), missing.Shadow.Rows)
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

// A float that differs from its source only beyond the fifteenth
// significant digit is a mismatch whatever extra_float_digits the database
// configures: the digest hashes each row's text rendering, and at
// extra_float_digits <= 0 that rendering rounds to fifteen digits, so 0.3
// and 0.1 + 0.2 would spell, and hash, the same. The pass pins the setting
// itself, so a pool whose every session starts at 0 still finds the row.
// The setting is a connection parameter of the pool the pass reads on, not
// a database setting, so nothing outlives the test or reaches another one
// on the same server.
func TestVerifierHashesFloatsAtFullPrecision(t *testing.T) {
	f := newVerifierFixture(t)
	f.exec(t, `
		CREATE TABLE %s.readings (
			id bigint PRIMARY KEY,
			v double precision NOT NULL,
			note text
		)`)
	f.exec(t, fmt.Sprintf(`
		INSERT INTO %%s.readings (id, v, note)
		SELECT n, 0.3, 'reading ' || n FROM generate_series(1, %d) AS n`, rows))
	target := f.prove(t, "readings")
	lock := f.lock(t, "readings")
	shadow := f.build(t, lock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, lock)
	f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET v = 0.1::float8 + 0.2::float8 WHERE id = 7")
	rounding := f.poolWithRuntimeParam(t, "extra_float_digits", "0")
	var digits string
	require.NoError(t, rounding.QueryRow(t.Context(), "SHOW extra_float_digits").Scan(&digits))
	require.Equal(t, "0", digits, "the connection parameter must reach the session the pass reads on")

	report, err := f.verify(t, rounding, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1)
	assert.Equal(t, chunk(t, math.MinInt64, 1000), report.Mismatches[0].Chunk)
	assert.Equal(t, report.Mismatches[0].Source.Rows, report.Mismatches[0].Shadow.Rows, "the rows differ in value, not in number")
}

// A chunk's two digests describe one snapshot (CO-1): a shadow row another
// session changes after the chunk's source digest has been read, and before
// its shadow digest is, is not seen by that chunk, so the pass compares
// clean. Under a snapshot per statement the shadow digest would see the
// change and report the chunk. The change is made from a query hook on the
// pool the pass reads on, which fires once, when the source digest of the
// chunk holding the key completes. The next pass takes new snapshots and
// finds the row.
func TestVerifierDigestsBothSidesInOneSnapshot(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	var once sync.Once
	hooked := f.hookedPool(t, func(_ context.Context, _ *pgx.Conn, sql string) {
		if !f.isSourceDigest(sql) {
			return
		}
		once.Do(func() { f.exec(t, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 500") })
	})

	report, err := f.verify(t, hooked, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean(), "mismatches: %+v", report.Mismatches)
	assert.Equal(t, 3, report.Chunks)

	report, err = f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	require.Len(t, report.Mismatches, 1, "the change landed; a pass with new snapshots sees it")
	assert.Equal(t, chunk(t, math.MinInt64, 1000), report.Mismatches[0].Chunk)
}

// The transaction a chunk's digests run in is read-only (CO-9): a write
// issued on that transaction's own connection, between its two digests, is
// refused by the server with read_only_sql_transaction rather than landing
// on the shadow, and the pass fails because the transaction is aborted. The
// write is attempted from a query hook on the pool the pass reads on, on
// the connection the hook is handed, so it runs inside the verify
// transaction and not beside it.
func TestVerifierTransactionRefusesWrites(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	var attempted atomic.Bool
	var writeErr error
	hooked := f.hookedPool(t, func(ctx context.Context, conn *pgx.Conn, sql string) {
		if !f.isSourceDigest(sql) || !attempted.CompareAndSwap(false, true) {
			return
		}
		_, writeErr = conn.Exec(ctx, "UPDATE "+f.shadowName(shadow)+" SET qty = 0 WHERE id = 500")
	})

	_, err := f.verify(t, hooked, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.Error(t, err, "a transaction with a refused statement in it cannot finish the pass")
	require.True(t, attempted.Load(), "the write must have been attempted inside the verify transaction")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, writeErr, &pgErr)
	assert.Equal(t, "25006", pgErr.Code, "read_only_sql_transaction")

	var qty int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT qty FROM "+f.shadowName(shadow)+" WHERE id = 500").Scan(&qty))
	assert.NotEqual(t, 0, qty, "the refused write left the shadow row as the copier wrote it")
}

// isSourceDigest reports whether sql is the statement that digests a chunk
// of the fixture's orders table, the first of a chunk's two reads.
func (f verifierFixture) isSourceDigest(sql string) bool {
	sourceDigest := " FROM " + pgx.Identifier{f.schema, "orders"}.Sanitize() + " WHERE "
	return strings.HasPrefix(sql, "SELECT pg_catalog.count(*)") && strings.Contains(sql, sourceDigest)
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
// misreads the shadow's types nor hashes with someone else's functions nor
// compares keys with someone else's operator: an impostor format_type that
// would make every numeric compare as text produces no false mismatch; an
// impostor sha256, convert_to, or encode that answers the same for every
// input hides no real difference; an impostor getdatabaseencoding that
// names no encoding would fail every conversion and so every pass; and an
// impostor bigint <= that is never
// true, which would empty the key range on both sides and compare nothing
// clean, is not the <= that BETWEEN resolves to. Functions are qualified in
// the statement itself; the operator can only be pinned by the
// transaction's own search_path.
func TestVerifierIgnoresTheSessionSearchPath(t *testing.T) {
	f := newVerifierFixture(t)
	f.createOrders(t)
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s ALTER COLUMN qty TYPE numeric(10,2)`)
	f.copy(t, target, shadow, lock)
	f.exec(t, `CREATE FUNCTION %s.sha256(bytea) RETURNS bytea LANGUAGE sql IMMUTABLE AS 'SELECT ''\x00''::bytea'`)
	f.exec(t, `CREATE FUNCTION %s.convert_to(text, name) RETURNS bytea LANGUAGE sql IMMUTABLE AS 'SELECT ''\x00''::bytea'`)
	f.exec(t, `CREATE FUNCTION %s.getdatabaseencoding() RETURNS name LANGUAGE sql STABLE AS 'SELECT ''impostor''::name'`)
	f.exec(t, `CREATE FUNCTION %s.encode(bytea, text) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT ''impostor''::text'`)
	f.exec(t, `CREATE FUNCTION %s.format_type(oid, integer) RETURNS text LANGUAGE sql STABLE AS 'SELECT ''text''::text'`)
	f.exec(t, `CREATE FUNCTION %s.never_le(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT false'`)
	f.exec(t, `CREATE OPERATOR %s.<= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.never_le)`)
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

// A shadow that has lost a column the proof lists for copy is not the shadow
// the proof describes, even though it is still the same relation (ST-6):
// the pass refuses before any digest, naming the column, rather than
// comparing the columns that remain.
func TestVerifierRefusesAShadowMissingACopyColumn(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, "ALTER TABLE "+f.shadowName(shadow)+" DROP COLUMN qty")

	_, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.ErrorIs(t, err, checksum.ErrInvariantViolation)
	assert.Contains(t, err.Error(), "(ST-6): shadow")
	assert.Contains(t, err.Error(), "no column qty")
}

// Every read runs with the source owner's privileges, not the connected
// role's (SET LOCAL ROLE owner): a source the owner may no longer read is
// not read with the superuser's power behind the pool, and the server's
// refusal is the error. The owner is a throwaway role whose own SELECT on
// the table is revoked after the shadow is built and copied; the superuser
// pool would read it regardless.
func TestVerifierReadsAsTheTableOwner(t *testing.T) {
	f := newVerifierFixture(t)
	owner := testutil.NewRole(t, f.pool, "NOLOGIN")
	f.exec(t, "GRANT USAGE, CREATE ON SCHEMA %s TO "+pgx.Identifier{owner}.Sanitize())
	f.createOrders(t)
	f.exec(t, "ALTER TABLE %s.orders OWNER TO "+pgx.Identifier{owner}.Sanitize())
	target := f.prove(t, "orders")
	require.Equal(t, owner, target.OwnerRole(), "the proof names the owner the pass reads as")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, lock)

	report, err := f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean(), "mismatches: %+v", report.Mismatches)

	f.exec(t, "REVOKE SELECT ON %s.orders FROM "+pgx.Identifier{owner}.Sanitize())
	_, err = f.verify(t, f.pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "42501", pgErr.Code, "insufficient_privilege: the owner, not the superuser, was refused")
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

// poolWithRuntimeParam is a pool on the fixture's server whose every session
// starts with the named setting at value, as a connection parameter: the
// setting reaches no other pool and nothing is left behind on the server.
func (f verifierFixture) poolWithRuntimeParam(t *testing.T, name, value string) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	pc.ConnConfig.RuntimeParams[name] = value
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// hookedPool is a pool on the fixture's server whose every query, once its
// result has been read, runs hook with the query's SQL and the connection
// it ran on, on the calling goroutine, so a test can act between two
// statements of one transaction, on that transaction's own connection or
// on another.
func (f verifierFixture) hookedPool(t *testing.T, hook func(ctx context.Context, conn *pgx.Conn, sql string)) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(f.cfg.URL)
	require.NoError(t, err)
	pc.ConnConfig.Tracer = queryHook(hook)
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// queryHook is a pgx query tracer that runs after each query completes,
// carrying the SQL from the query's start to its end through the context.
type queryHook func(ctx context.Context, conn *pgx.Conn, sql string)

type hookedSQLKey struct{}

func (queryHook) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, hookedSQLKey{}, data.SQL)
}

func (h queryHook) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	if sql, ok := ctx.Value(hookedSQLKey{}).(string); ok {
		h(ctx, conn, sql)
	}
}
