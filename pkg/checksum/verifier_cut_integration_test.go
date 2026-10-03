package checksum_test

import (
	"context"
	"math"
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
)

// A pass cuts each chunk inside the chunk's own guarded transaction, so the
// cut runs under the verifier's lock timeout whatever pool the caller built —
// a pool built outside pkg/dbconn has no session defaults to fall back on.
// The pass is parked at its second clock reading — its first chunk compared
// and committed, its next chunk not yet begun — while a reader holds ACCESS
// SHARE on the source and an application queues an ACCESS EXCLUSIVE request
// behind it. The next chunk's transaction queues behind that request and
// must give up at the verifier's lock timeout rather than wait out the
// application.
func TestVerifierCutIsBoundedOnACallerBuiltPool(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	unbounded, err := pgxpool.New(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(unbounded.Close)
	parked, resume := make(chan struct{}), make(chan struct{})
	park := &nthReadingClock{at: 2, hook: func() {
		close(parked)
		<-resume
	}}

	v, err := checksum.NewVerifier(target, shadow, lock, checksum.Options{
		LockTimeout: 500 * time.Millisecond,
		Chunker:     chunkRows,
		Clock:       park,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := v.Verify(t.Context(), unbounded, copier.NewWatermark(math.MaxInt64))
		results <- err
	})
	t.Cleanup(wg.Wait)
	const parkDeadline = 15 * time.Second
	select {
	case <-parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the pass did not compare its first chunk within %s", parkDeadline)
	}

	source := pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()
	reader, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = reader.Exec(t.Context(), "LOCK TABLE "+source+" IN ACCESS SHARE MODE")
	require.NoError(t, err)

	// The application's request waits behind the reader for as long as the
	// test lets it.
	blockerCtx, giveUp := context.WithCancel(t.Context())
	t.Cleanup(giveUp)
	blocked := make(chan error, 1)
	wg.Go(func() {
		blocker, err := f.pool.Begin(blockerCtx)
		if err != nil {
			blocked <- err
			return
		}
		defer func() { _ = blocker.Rollback(context.WithoutCancel(blockerCtx)) }()
		if _, err := blocker.Exec(blockerCtx, "SET LOCAL lock_timeout = 0"); err != nil {
			blocked <- err
			return
		}
		_, err = blocker.Exec(blockerCtx, "LOCK TABLE "+source+" IN ACCESS EXCLUSIVE MODE")
		blocked <- err
	})
	const queuedDeadline = 15 * time.Second
	require.Eventually(t, func() bool {
		return f.waitsForLock(t, shadow.SourceOID(), "AccessExclusiveLock")
	}, queuedDeadline, 20*time.Millisecond, "the application's ACCESS EXCLUSIVE should queue behind the reader")

	close(resume)
	const cutDeadline = 15 * time.Second
	select {
	case err := <-results:
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr, "the pass ends with the server's lock timeout, not the application's patience")
		assert.Equal(t, "55P03", pgErr.Code)
	case <-time.After(cutDeadline):
		t.Fatalf("the pass did not end within %s: its cut waited behind the application's lock", cutDeadline)
	}

	giveUp()
	require.Error(t, <-blocked, "the application's request was cancelled, never granted")
	require.NoError(t, reader.Rollback(context.WithoutCancel(t.Context())))
}

// The cut runs in the guarded transaction, whose search_path is the catalog
// alone (CO-9), so the boundary query resolves its key comparison to the
// catalog's operator whatever the caller's pool puts ahead of pg_catalog.
// The schema first on this pool's path offers a bigint >= that is always
// true; a cut under the session's own path would return the same thousandth
// key every time, and the second cut would close below its own lower bound
// — a chunk the chunker refuses as a coverage violation. The digests are
// not at stake: their BETWEEN is pinned by the same transaction already.
func TestVerifierCutIgnoresTheSessionSearchPath(t *testing.T) {
	f := newVerifierFixture(t)
	target, lock, shadow := f.prepare(t)
	f.exec(t, `CREATE FUNCTION %s.always_ge(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'`)
	f.exec(t, `CREATE OPERATOR %s.>= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.always_ge)`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	report, err := f.verify(t, shadowing, target, shadow, lock, copier.NewWatermark(math.MaxInt64))
	require.NoError(t, err)
	assert.True(t, report.Clean(), "mismatches: %+v", report.Mismatches)
	assert.Equal(t, 3, report.Chunks, "the pass cut its three thousand-row chunks with the catalog's operator")
	assert.Equal(t, int64(rows), report.Rows)
}

// waitsForLock reports whether some backend is waiting for mode on the
// relation with the given OID.
func (f verifierFixture) waitsForLock(t *testing.T, oid uint32, mode string) bool {
	t.Helper()
	var waiting bool
	require.NoError(t, f.pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'relation' AND relation = $1 AND mode = $2 AND NOT granted
		)`, oid, mode).Scan(&waiting))
	return waiting
}

// nthReadingClock runs hook on its n-th reading, once, and otherwise tells
// the wall time.
type nthReadingClock struct {
	mu       sync.Mutex
	readings int
	at       int
	hook     func()
}

func (c *nthReadingClock) Now() time.Time {
	c.mu.Lock()
	c.readings++
	fire := c.readings == c.at
	c.mu.Unlock()
	if fire {
		c.hook()
	}
	return time.Now()
}
