package applier_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/applier"
	"github.com/block/pg-sprite/pkg/decode"
)

// releaseTestCeiling is the ceiling the release-cadence test sets, sized
// against its write load of about 1 MiB of WAL a second, the catch-up's
// own flushes into the shadow included. The server releases the slot one
// running-transactions record behind (see the test), so the slot retains
// between one and two release intervals of the load: about 16 MiB just
// after a release and about 31 MiB just before the next. The ceiling sits
// between the two, so the retained WAL crosses it some nine seconds after
// every record and drops back within it at the next release, while the WAL
// the stream has yet to confirm stays at a few hundred KiB.
const releaseTestCeiling = int64(24 << 20)

// slotReading is one reading of the catch-up's slot, as the test logs it
// and judges it.
type slotReading struct {
	at          time.Time
	restart     decode.LSN
	confirmed   decode.LSN
	write       decode.LSN
	retained    int64
	unconfirmed int64
}

func (r slotReading) over() bool { return r.retained > releaseTestCeiling }

// readSlot reads the slot the way the catch-up's own check does. The write
// position is the one both measures ran to: retained WAL is measured back
// to the restart position from it.
func readSlot(t *testing.T, run *catchupRun) slotReading {
	t.Helper()
	status, found, err := decode.InspectSlot(t.Context(), run.f.pool, run.slot)
	require.NoError(t, err)
	require.True(t, found, "the catch-up's slot is in place")
	require.True(t, status.Retained.Known && status.Unconfirmed.Known, "a primary measures both")
	return slotReading{
		at:          time.Now(),
		restart:     status.RestartLSN,
		confirmed:   status.ConfirmedFlushLSN,
		write:       status.RestartLSN + decode.LSN(status.Retained.Bytes),
		retained:    status.Retained.Bytes,
		unconfirmed: status.Unconfirmed.Bytes,
	}
}

// startReleaseLoad commits a batch of rows to the source every tick, and
// on two more sessions keeps a short write transaction open at every
// instant, until the returned stop is called; stop reports the first write
// that failed. Each batch is about 50 KiB of rows, and the catch-up
// flushes every one into the shadow, which writes as much WAL again. The
// open transactions each write one row and commit two seconds later, the
// second session a second behind the first, so whenever the server logs a
// running-transactions record a transaction is in progress that began
// after the record before it: the shape of a busy source.
func startReleaseLoad(t *testing.T, f catchupFixture) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		loadErr error
	)
	fail := func(err error) {
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if loadErr == nil {
			loadErr = err
		}
	}
	wg.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for batch := 0; ; batch++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			// Negative uniq values, a fresh range per batch, stay clear of
			// the seeded rows' positive ones.
			_, err := f.pool.Exec(ctx, `
				INSERT INTO `+f.table.Qualified()+` (uniq, amount, label, blob)
				SELECT -($1::integer * 1000 + g), 0, 'load', repeat('w', 1024)
				FROM generate_series(1, 40) AS g`, batch)
			if err != nil {
				fail(err)
				return
			}
		}
	})
	const openFor = 2 * time.Second
	for session := range 2 {
		wg.Go(func() {
			if !waitOrDone(ctx, time.Duration(session)*openFor/2) {
				return
			}
			// Uniq values below the batches' range, a fresh one per
			// transaction.
			for n := 1; ; n++ {
				if err := holdOpenWrite(ctx, f, -(1<<30)-n*2-session, openFor); err != nil {
					fail(err)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		})
	}
	var once sync.Once
	stop = func() error {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
		errMu.Lock()
		defer errMu.Unlock()
		return loadErr
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Logf("release-cadence load: %v", err)
		}
	})
	return stop
}

// holdOpenWrite writes one row in a transaction it commits after openFor,
// or rolls back when ctx ends first.
func holdOpenWrite(ctx context.Context, f catchupFixture, uniq int, openFor time.Duration) error {
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO `+f.table.Qualified()+` (uniq, amount, label, blob)
		VALUES ($1, 0, 'open', 'o')`, uniq)
	if err != nil {
		return errors.Join(err, tx.Rollback(context.WithoutCancel(ctx)))
	}
	if !waitOrDone(ctx, openFor) {
		return tx.Rollback(context.WithoutCancel(ctx))
	}
	return tx.Commit(ctx)
}

// waitOrDone paces the load: it waits d and reports true, or false as soon
// as ctx ends.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// A logical slot's restart position moves only at the running-transactions
// records the server logs about every RestartReleaseInterval, once the
// stream has confirmed past one, and while a transaction is in progress at
// a record it moves only to the record before, where the oldest such
// transaction can be decoded from again. A busy source has one in progress
// at nearly every record, so the slot retains between one and two release
// intervals of writes even while the stream keeps up. With the ceiling
// between the two, the retained WAL crosses it before each of the server's
// records and drops back within it at the release that follows, while the
// WAL the stream has yet to confirm stays within it throughout. The
// catch-up lets the retained WAL stand over the ceiling across two such
// releases and keeps running through them, on the real clock and at the
// server's own cadence: nothing here asks the server for a
// running-transactions record. Every reading is logged with its positions,
// so a failure tells the server's timing apart from a stream that fell
// behind.
//
// The test spans several of the server's release intervals on the real
// clock, so it runs only when SLOT_RELEASE_PROOF=1 is set: `make
// slot-release-proof` sets it, with PG_VERSION choosing the major.
func TestCatchupKeepsRunningWhileTheServerReleasesTheSlotAtItsOwnCadence(t *testing.T) {
	if os.Getenv("SLOT_RELEASE_PROOF") != "1" {
		t.Skip("set SLOT_RELEASE_PROOF=1 to run the slot release proof; `make slot-release-proof` sets it")
	}
	f := newCatchupFixture(t, 20)
	run := startCatchupWith(t, f, catchupSetup{
		positions: &switchPosition{pos: allLanded()},
		opts:      applier.CatchupOptions{SlotLagCeiling: releaseTestCeiling},
	})
	run.awaitStatus(t, "complete a cycle under the ceiling", func(s applier.Status) bool {
		return s.WALEnd > 0
	})
	stopLoad := startReleaseLoad(t, f)

	// The readings end once the retained WAL has gone over the ceiling and
	// come back within it at two of the server's releases: the second
	// comes a whole release interval after the first, and by then a
	// catch-up that judged the retained WAL at once, or that waited out a
	// shorter grace than the time it stays over, has ended.
	const (
		wantReleases    = 2
		readingDeadline = 90 * time.Second
		readingInterval = 200 * time.Millisecond
	)
	start := time.Now()
	deadline := time.NewTimer(readingDeadline)
	defer deadline.Stop()
	poll := time.NewTicker(readingInterval)
	defer poll.Stop()
	var (
		prev         *slotReading
		overSince    time.Time
		releases     []slotReading
		longestOver  time.Duration
		confirmsOver int
	)
	for len(releases) < wantReleases {
		select {
		case <-deadline.C:
			t.Fatalf("the slot was not seen over the ceiling and released back within it %d times within %s; %d were",
				wantReleases, readingDeadline, len(releases))
		case <-poll.C:
		}
		run.status(t) // the catch-up is still running
		r := readSlot(t, run)
		t.Logf("reading at %s: restart_lsn %s, confirmed %s, write %s, retained %d, unconfirmed %d",
			r.at.Format("15:04:05.000"), r.restart, r.confirmed, r.write, r.retained, r.unconfirmed)
		require.LessOrEqual(t, r.unconfirmed, releaseTestCeiling,
			"the stream keeps up with the load, so the WAL it has yet to confirm stays within the ceiling")
		switch {
		case prev == nil:
		case r.over() && overSince.IsZero():
			overSince = r.at
		case r.over() && r.restart == prev.restart && r.confirmed > prev.confirmed:
			confirmsOver++
		case !r.over() && prev.over() && r.restart > prev.restart:
			over := r.at.Sub(overSince)
			longestOver = max(longestOver, over)
			t.Logf("released at %s after %s over the ceiling: restart_lsn %s -> %s, retained %d -> %d",
				r.at.Format("15:04:05.000"), over, prev.restart, r.restart, prev.retained, r.retained)
			releases = append(releases, r)
			overSince = time.Time{}
		}
		prev = &r
	}
	t.Logf("load wrote %.2f MiB of WAL a second; longest over the ceiling %s",
		float64(prev.write-releases[0].write)/(1<<20)/prev.at.Sub(releases[0].at).Seconds(), longestOver)
	assert.Positive(t, confirmsOver,
		"while the retained WAL stood over the ceiling the stream kept confirming and the restart position stood still")
	assert.Greater(t, releases[1].at.Sub(releases[0].at), decode.RestartReleaseInterval/2,
		"the releases are the server's own, one release interval apart, not two readings of one release")
	t.Logf("readings spanned %s", time.Since(start))

	// The last reading came after the second release; a cycle that ends
	// after a write made now has judged the slot later still.
	run.awaitCycleAfter(t, "run a cycle after the second release")
	run.status(t)
	require.NoError(t, stopLoad())
	run.stop(t)
}
