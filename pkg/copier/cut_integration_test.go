package copier_test

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
	"github.com/block/pg-sprite/pkg/copier"
)

// A cut bounds itself whatever pool the caller built: the boundary query
// runs under the copier's own lock timeout, not the pool's session defaults,
// which a pool built outside pkg/dbconn does not have. The one worker is
// parked at its second clock reading — its first chunk committed and landed,
// its next cut not yet begun — while a reader holds ACCESS SHARE on the
// source and an application queues an ACCESS EXCLUSIVE request behind it.
// The cut's own ACCESS SHARE queues behind that request and must give up at
// the copier's lock timeout rather than wait out the application. The
// position afterwards shows it was the cut that gave up: the first chunk
// landed, nothing above it was ever claimed, and nothing is in flight — a
// chunk whose copy had failed would still be in flight.
func TestCopierCutIsBoundedOnACallerBuiltPool(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	unbounded, err := pgxpool.New(t.Context(), f.cfg.URL)
	require.NoError(t, err)
	t.Cleanup(unbounded.Close)
	parked, resume := make(chan struct{}), make(chan struct{})
	park := &nthReadingClock{at: 2, hook: func() {
		close(parked)
		<-resume
	}}

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers:     1,
		LockTimeout: 500 * time.Millisecond,
		Chunker:     copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
		Clock:       park,
	})
	require.NoError(t, err)
	results := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { results <- c.Run(t.Context(), unbounded) })
	t.Cleanup(wg.Wait)
	const parkDeadline = 15 * time.Second
	select {
	case <-parked:
	case <-time.After(parkDeadline):
		t.Fatalf("the copier did not land its first chunk within %s", parkDeadline)
	}

	source := pgx.Identifier{f.schema, shadow.SourceTable()}.Sanitize()
	reader, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	// Redundant safety closer: the test rolls the reader back on its last
	// line, after which Rollback returns the guaranteed ErrTxClosed. A
	// failure before that line must still release the connection, or the
	// fixture's pool close waits on it for the package deadline.
	t.Cleanup(func() { _ = reader.Rollback(context.WithoutCancel(t.Context())) })
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
		require.ErrorAs(t, err, &pgErr, "the cut ends with the server's lock timeout, not the application's patience")
		assert.Equal(t, "55P03", pgErr.Code)
	case <-time.After(cutDeadline):
		t.Fatalf("the copy did not end within %s: its cut waited behind the application's lock", cutDeadline)
	}
	position := c.Position()
	assert.Equal(t, copier.NewWatermark(100), position.Watermark, "the first chunk landed before the application arrived")
	assert.Equal(t, copier.NewWatermark(100), position.Cut, "the second cut never completed, so nothing above the first chunk was claimed")
	assert.Empty(t, position.InFlight, "a failed cut claims nothing; only a failed copy would leave a chunk in flight")

	giveUp()
	require.Error(t, <-blocked, "the application's request was cancelled, never granted")
	require.NoError(t, reader.Rollback(context.WithoutCancel(t.Context())))
}

// Every cut pins the catalog alone on its search_path (CO-9), so the
// boundary query resolves its key comparison to the catalog's operator
// whatever the caller's pool puts ahead of pg_catalog. The schema first on
// this pool's path offers a bigint >= that is always true; under the
// session's own path every cut would return the same hundredth key, and the
// second cut would close below its own lower bound — a chunk the copier
// refuses as a coverage violation. The copy statement is not at stake: its
// BETWEEN is pinned by the chunk transaction already.
func TestCopierCutsIgnoreTheSessionSearchPath(t *testing.T) {
	f := newCopierFixture(t)
	const rows = 300
	target, lock, shadow := f.prepare(t, rows)
	f.exec(t, `CREATE FUNCTION %s.always_ge(bigint, bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'`)
	f.exec(t, `CREATE OPERATOR %s.>= (LEFTARG = bigint, RIGHTARG = bigint, FUNCTION = %s.always_ge)`)
	shadowing := testutil.NewCatalogShadowingPool(t, f.cfg.URL, f.schema)

	c, err := copier.NewCopier(target, shadow, lock, copier.Watermark{}, copier.Options{
		Workers: 2,
		Chunker: copier.ChunkerOptions{InitialRows: 100, MaxRows: 100},
	})
	require.NoError(t, err)
	require.NoError(t, c.Run(t.Context(), shadowing))

	f.assertConverged(t, shadow)
	assert.Equal(t, int64(rows), c.Position().RowsInserted, "every row landed through chunks cut with the catalog's operator")
	assert.Equal(t, copier.NewWatermark(math.MaxInt64), c.Position().Watermark)
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
