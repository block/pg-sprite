package checksum_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
)

// prepareUnique is prepare with a unique index on qty, so the shadow
// carries the source's unique index and a recopy whose rows hold a value
// another shadow row still holds is refused by the server.
func (f verifierFixture) prepareUnique(t *testing.T) (preflight.CopySwapTarget, *dbconn.TableLockSession, schemachange.BuiltShadow) {
	t.Helper()
	f.createOrders(t)
	f.exec(t, "CREATE UNIQUE INDEX orders_qty ON %s.orders (qty)")
	target := f.prove(t, "orders")
	lock := f.lock(t, "orders")
	shadow := f.build(t, lock, target, `ALTER TABLE %s DROP COLUMN note`)
	f.copy(t, target, shadow, lock)
	return target, lock, shadow
}

// moveUniqueValue moves qty value from key from to key to on the source,
// giving from the negative of its own key so no row holds the value twice,
// the way a source transaction moves a unique value between two rows.
func (f verifierFixture) moveUniqueValue(t *testing.T, from, to int64) {
	t.Helper()
	_, err := f.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{f.schema, "orders"}.Sanitize()+" SET qty = -id WHERE id = $1", from)
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{f.schema, "orders"}.Sanitize()+" SET qty = $1 WHERE id = $2", from, to)
	require.NoError(t, err)
}

// checkWith is check with the verifier's options stated.
func (f verifierFixture) checkWith(t *testing.T, pool *pgxpool.Pool, target preflight.CopySwapTarget, shadow schemachange.BuiltShadow, lock *dbconn.TableLockSession, through copier.Watermark, policy checksum.DivergencePolicy, opts checksum.Options) (checksum.Outcome, error) {
	t.Helper()
	v, err := checksum.NewVerifier(target, shadow, lock, opts)
	require.NoError(t, err)
	return v.Check(t.Context(), pool, through, policy)
}

// Under DivergenceRecopy the shadow converges when the source moves a
// unique value from a row in a chunk the comparison read equal to a row in
// the differing chunk after the comparison and before the recopy — the
// race a source written through a reconcile can run. The recopy of the
// middle chunk puts back key 1600 holding the value 100 while the shadow's
// key 100, in the first chunk and untouched, still holds it, so the server
// refuses the insert; the pass compares again, finds both chunks
// differing, and recopies both in one transaction, which lands. The
// collision is counted and the pass mints no proof.
func TestCheckRecopyRetriesAUniqueValueMovedDuringTheRecopy(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepareUnique(t)
	f.exec(t, "UPDATE %s.orders SET qty = -1500 WHERE id = 1500")
	pool := f.poolHookedBeforeStatement(t, "DELETE FROM", func() { f.moveUniqueValue(t, 100, 1600) })

	outcome, err := f.check(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRecopy)
	require.NoError(t, err)
	assert.Equal(t, 1, outcome.Collisions, "the first recopy collided; the second landed")
	require.Len(t, outcome.Repairs, 2, "the recopy that landed covered the chunk the value left and the chunk it moved to")
	assert.Equal(t, chunk(t, math.MinInt64, 1000), outcome.Repairs[0].Mismatch.Chunk)
	assert.Equal(t, chunk(t, 1001, 2000), outcome.Repairs[1].Mismatch.Chunk)
	assert.Len(t, outcome.Report.Mismatches, 2, "the report is the comparison the landed recopy was cut from")
	qty, _ := f.shadowRow(t, shadow, 100)
	assert.Equal(t, int64(-100), qty, "key 100 gave the value up")
	qty, _ = f.shadowRow(t, shadow, 1600)
	assert.Equal(t, int64(100), qty, "key 1600 holds the value that moved to it")
	qty, _ = f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(-1500), qty, "the difference the pass set out to recopy is gone too")
	assertNoProofs(t, outcome)
}

// A source that moves a unique value into the differing chunk before every
// recopy exhausts the pass's attempts: each recopy collides, each is
// counted, and the last one's error ends the pass as the server raised it,
// with no recopy committed. The moves come from two different clean
// chunks, so the second comparison's wider recopy still meets a row the
// shadow holds.
func TestCheckRecopyStopsWhenEveryAttemptCollides(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepareUnique(t)
	f.exec(t, "UPDATE %s.orders SET qty = -1500 WHERE id = 1500")
	moves := []func(){
		func() { f.moveUniqueValue(t, 100, 1600) },
		func() { f.moveUniqueValue(t, 2100, 1700) },
	}
	var mu sync.Mutex
	var armed bool
	pool := f.hookedPool(t, func(_ context.Context, _ *pgx.Conn, sql string) {
		mu.Lock()
		defer mu.Unlock()
		if sql == "begin read write" {
			armed = true
			return
		}
		if armed && strings.HasPrefix(sql, "DELETE FROM") {
			armed = false
			require.NotEmpty(t, moves, "a third recopy ran")
			moves[0]()
			moves = moves[1:]
		}
	})

	outcome, err := f.checkWith(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRecopy, checksum.Options{Chunker: chunkRows, RecopyAttempts: 2})
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code, "the last attempt's collision is the error")
	assert.Equal(t, 2, outcome.Collisions)
	assert.Empty(t, outcome.Repairs, "no recopy committed")
	assert.Len(t, outcome.Report.Mismatches, 2, "the report is the comparison the last recopy was cut from")
	mu.Lock()
	assert.Empty(t, moves, "both attempts ran a recopy")
	mu.Unlock()
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(1500), qty, "every recopy rolled back with its collision")
	qty, _ = f.shadowRow(t, shadow, 100)
	assert.Equal(t, int64(100), qty)
	assertNoProofs(t, outcome)
}

// Under DivergenceRepair the same move is not retried: a repair pass
// assumes the source rows it recopies hold still, so the collision is the
// pass's error as the server raised it and nothing is counted or recopied.
func TestCheckRepairDoesNotRetryACollision(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepareUnique(t)
	f.exec(t, "UPDATE %s.orders SET qty = -1500 WHERE id = 1500")
	pool := f.poolHookedBeforeStatement(t, "DELETE FROM", func() { f.moveUniqueValue(t, 100, 1600) })

	outcome, err := f.check(t, pool, target, shadow, lock, copier.NewWatermark(math.MaxInt64), checksum.DivergenceRepair)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	assert.Equal(t, "23505", pgErr.Code)
	assert.Zero(t, outcome.Collisions)
	assert.Empty(t, outcome.Repairs)
	qty, _ := f.shadowRow(t, shadow, 1500)
	assert.Equal(t, int64(1500), qty, "the repair rolled back")
}
