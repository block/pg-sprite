package loadproof_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/verdict"
)

// defaultLibraryResults is where the library proof writes its record,
// beside the binary proof's, so one run does not overwrite the other's.
const defaultLibraryResults = "results-library.json"

// slotIdleDeadline bounds the host's wait for the server to let go of the
// slot a cancelled call held: the walsender serving the call's stream
// leaves once it notices the connection closed, which on a healthy server
// is at once. A resume that started before it left would be refused as
// slot-held.
const slotIdleDeadline = 30 * time.Second

// waitForSlotIdle waits, for at most deadline, until no backend holds the
// slot the row names, as a host resuming a stopped run does, and returns
// how long it waited. A slot that is gone is an error: the stopped run
// left it to resume from.
func waitForSlotIdle(ctx context.Context, pool *pgxpool.Pool, store *checkpoint.Store, table testutil.WorkloadTable, deadline time.Duration) (time.Duration, error) {
	cp, err := store.Inspect(ctx, table.Schema, table.Table)
	if err != nil {
		return 0, fmt.Errorf("inspect the checkpoint row before the resume: %w", err)
	}
	if cp.SlotName == "" {
		return 0, fmt.Errorf("the stopped live run's row in %s names no slot", cp.Phase)
	}
	began := time.Now()
	giveUp := time.NewTimer(deadline)
	defer giveUp.Stop()
	poll := time.NewTicker(commitPollInterval)
	defer poll.Stop()
	for {
		holder, found, err := slotHolder(ctx, pool, cp.SlotName)
		if err != nil {
			return 0, fmt.Errorf("read replication slot %s: %w", cp.SlotName, err)
		}
		if !found {
			return 0, fmt.Errorf("the stopped run's slot %s is gone, so there is no stream to resume", cp.SlotName)
		}
		if holder == 0 {
			return time.Since(began), nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for slot %s to be let go: %w", cp.SlotName, ctx.Err())
		case <-giveUp.C:
			return 0, fmt.Errorf("%w: backend %d still holds %s after %s", errSlotStillHeld, holder, cp.SlotName, deadline)
		case <-poll.C:
		}
	}
}

// errNotStopped reports an attempt the host's cancel did not stop where it
// was meant to: the run finished, failed for another reason, or left its
// row outside the phase the host cancelled in.
var errNotStopped = errors.New("the attempt did not stop where the host cancelled it")

// checkStoppedAt holds a cancelled attempt to the stop it was cancelled
// at, recording the row's phase and watermark in info: Run reports the
// caller's cancel, with the host's cause, as a failed verdict coded
// cancelled-by-caller, and the checkpoint row stands in the phase the host
// stopped it in — for a stop mid-copy, at a watermark short of the key
// space — so the cancel landed inside the phase, not after it.
func checkStoppedAt(ctx context.Context, h host, table testutil.WorkloadTable, stop stopPoint, a attempt, info *attemptInfo) error {
	v := a.verdict
	if a.err == nil {
		return fmt.Errorf("%w: the run finished (%s) before the host's cancel in %s landed; seed more rows (LOAD_PROOF_ROWS)", errNotStopped, v.Outcome, stop.phase())
	}
	// The host's cause reaches the error by two roads. In the copy, the
	// copier wraps the cause into its own error. In the verification pass,
	// the checksum's read reports the bare cancellation, so the cause comes
	// from the live capture's catch-up, which ends with it, and whose stop
	// error the runner joins into the run's (captureOutcome).
	if !errors.Is(a.err, context.Canceled) || !errors.Is(a.err, stop.cause()) {
		return fmt.Errorf("%w: the run stopped for another reason than the host's cancel (verdict %s, %s: %s): %w", errNotStopped, v.Outcome, v.Reason, v.Detail, a.err)
	}
	if v.Outcome != verdict.OutcomeFailed || v.Code != string(executor.CodeCancelledByCaller) {
		return fmt.Errorf("%w: a cancelled run reports outcome %s code %s, not failed cancelled-by-caller", errNotStopped, v.Outcome, v.Code)
	}
	cp, err := h.store.Inspect(ctx, table.Schema, table.Table)
	if err != nil {
		return fmt.Errorf("inspect the checkpoint row after the cancel: %w", err)
	}
	info.RowPhase = cp.Phase.String()
	if cp.Phase != stop.phase() {
		return fmt.Errorf("%w: the row stands in %s, not %s, where the host cancelled", errNotStopped, cp.Phase, stop.phase())
	}
	if info.ShadowOID, err = relationOID(ctx, h.pool, table.Schema, cp.ShadowTable); err != nil {
		return fmt.Errorf("read the shadow the stopped run left: %w", err)
	}
	if stop != stopMidCopy {
		return nil
	}
	if !cp.Watermark.Valid() || cp.Watermark.Complete() {
		return fmt.Errorf("%w: the row in copying carries no watermark short of the key space to resume from", errNotStopped)
	}
	landed := cp.Watermark.Value()
	info.WatermarkAtStop = &landed
	return nil
}

// hostStops are where the host cancels, in order, before the call that
// runs to its verdict.
func hostStops() []stopPoint { return []stopPoint{stopMidCopy, stopMidVerify} }

// errConnsHeld reports a call that left connections of the host's pool
// acquired, which a host that cancels and calls again on one pool runs
// short of.
var errConnsHeld = errors.New("the call left connections of the host's pool acquired")

// connsReturnedDeadline bounds the wait, after a call returns, for the
// host's pool to count none of its connections as acquired. A connection
// the cancel broke is closed by the pool on its own goroutine, under the
// pool's own close timeout, so a connection the call kept is the only one
// still counted after this.
const connsReturnedDeadline = 30 * time.Second

// waitForConnsReturned waits until the host's pool counts no connection
// as acquired.
func waitForConnsReturned(ctx context.Context, h host) error {
	giveUp := time.NewTimer(connsReturnedDeadline)
	defer giveUp.Stop()
	poll := time.NewTicker(commitPollInterval)
	defer poll.Stop()
	for {
		held := h.pool.Stat().AcquiredConns()
		if held == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the host's pool connections to be returned: %w", ctx.Err())
		case <-giveUp.C:
			return fmt.Errorf("%w: %d still acquired after %s", errConnsHeld, held, connsReturnedDeadline)
		case <-poll.C:
		}
	}
}

// stoppedWritesPerWorker is how many writes per load worker the host waits
// for between a cancelled call and the next, so the interval in which no
// call runs is never empty of writes.
const stoppedWritesPerWorker = 3

// runAttempts makes the host's calls to migrate.Run: one cancelled at each
// of hostStops, each followed, once the server has let go of the slot and
// the load has written while no call runs, by a call with the same
// statement that resumes from the checkpoint row, the last of them run to
// its verdict. Each resuming call is held to the run the stop left
// (checkResumed), and every call to returning the host's connections. It
// records every call in res as it returns and returns the last call made,
// with an error when a call did not stop where the host cancelled it, did
// not resume, held connections, or the host itself failed; the caller
// records the run before judging it. An operator's interrupt fails the
// test at once, so its cleanups run.
func runAttempts(t *testing.T, ctx, runCtx context.Context, h host, table testutil.WorkloadTable, load *testutil.LoadGenerator, workers int, started time.Time, res *results) (attempt, error) {
	t.Helper()
	call := func(stop stopPoint, waited time.Duration) (attempt, *attemptInfo, error) {
		a, hostErr := runAttempt(runCtx, h, stop, started)
		require.NoError(t, context.Cause(ctx), "the proof was interrupted: the run was cancelled, and the cleanups remove its replication slot and the seeded table")
		a.info.Attempt = len(res.Attempts) + 1
		a.info.SlotIdleWaitMillis = waited.Milliseconds()
		res.Attempts = append(res.Attempts, a.info)
		info := &res.Attempts[len(res.Attempts)-1]
		if hostErr != nil {
			return a, info, hostErr
		}
		if err := waitForConnsReturned(runCtx, h); err != nil {
			return a, info, fmt.Errorf("attempt %d: %w", info.Attempt, err)
		}
		return a, info, nil
	}
	var (
		waited   time.Duration
		previous *attemptInfo
		prevStop stopPoint
	)
	for _, stop := range hostStops() {
		a, info, err := call(stop, waited)
		if err != nil {
			return a, err
		}
		committedAtReturn := load.Progress().Committed()
		if err := checkStoppedAt(runCtx, h, table, stop, a, info); err != nil {
			return a, fmt.Errorf("attempt %d: %w", info.Attempt, err)
		}
		if previous != nil {
			if err := checkResumed(prevStop, *previous, *info); err != nil {
				return a, err
			}
		}
		t.Logf("attempt %d cancelled in %s after %s; the row stands in %s", info.Attempt, info.CancelledIn,
			time.Duration(info.RunMillis)*time.Millisecond, info.RowPhase)
		if waited, err = waitForSlotIdle(runCtx, h.pool, h.store, table, slotIdleDeadline); err != nil {
			return a, fmt.Errorf("before attempt %d: %w", info.Attempt+1, err)
		}
		if info.CommittedWhileStopped, err = waitForStoppedWrites(runCtx, load, committedAtReturn, stoppedWritesPerWorker*workers); err != nil {
			return a, fmt.Errorf("before attempt %d: %w", info.Attempt+1, err)
		}
		stopped := *info
		previous, prevStop = &stopped, stop
	}
	a, info, err := call(stopNone, waited)
	if err != nil {
		return a, err
	}
	// A last call that did not swap has no new table to compare; the
	// caller judges it by its verdict.
	if a.verdict.Outcome != verdict.OutcomeExecutedViaCopyAndSwap {
		return a, nil
	}
	if info.ShadowOID, err = relationOID(runCtx, h.pool, table.Schema, table.Table); err != nil {
		return a, fmt.Errorf("read the swapped-in table: %w", err)
	}
	return a, checkResumed(prevStop, *previous, *info)
}

// The library proof: the same identity-key retype as TestLoadProof, under
// the same load, driven through migrate.Run in-process as an importer
// drives it. The host cancels its context once mid-copy and once in the
// verification pass, and after each cancel calls Run again with the same
// statement, which resumes from the checkpoint row. The third call runs to
// its verdict, and the result is judged by the binary proof's own oracles.
func TestLoadProofLibrary(t *testing.T) {
	if os.Getenv("LOAD_PROOF_LIBRARY") != "1" {
		t.Skip("set LOAD_PROOF_LIBRARY=1 to run the library load proof; `make load-proof-library` sets everything")
	}
	c := readTarget(t, defaultLibraryResults)
	ctx := interruptible(t)
	res := newResults(c)
	t.Cleanup(func() {
		res.Passed = !t.Failed()
		if err := res.write(c.Results); err != nil {
			t.Errorf("write results: %v", err)
		}
		t.Logf("results written to %s", c.Results)
	})

	pool := newPool(t, c)
	release, err := slotReleaseDeadline(ctx, pool)
	require.NoError(t, err)
	requireTimeForTheRun(t, release)
	require.NoError(t, pool.QueryRow(ctx, `SHOW server_version`).Scan(&res.ServerVersion))
	t.Logf("library load proof server version %s", res.ServerVersion)

	shape := testutil.WorkloadShape{IDType: "integer"}
	table := testutil.NewWorkloadTableOf(t, pool, shape)
	removeLeftoversOnCleanup(t, pool, table, release)
	t.Logf("seeding %d rows into %s", c.Rows, table.Qualified())
	require.NoError(t, table.SeedRows(ctx, c.Rows))
	h := newHost(t, c, fmt.Sprintf(alterTemplate, table.Qualified()))

	load := startProofLoad(t, pool, table, c)

	runCtx, cancelRun := context.WithTimeout(ctx, runBudget)
	defer cancelRun()
	watch := startWatch(t, pool, table, load)
	atStart := load.Progress()
	started := time.Now()
	res.StartedAt = started
	final, attemptsErr := runAttempts(t, ctx, runCtx, h, table, load, c.Workers, started, res)
	exited := time.Now()
	atExit := load.Progress()
	res.RunMillis = exited.Sub(started).Milliseconds()
	w := watch.stop()
	res.recordWatch(w, started)
	v := final.verdict
	require.NoError(t, res.recordVerdict(v))
	// As in the binary proof, the load is recorded before any judgement.
	res.recordLoad(load.Snapshot(), atExit)

	require.NoError(t, attemptsErr)
	require.NoError(t, final.err, "verdict %s: %s", v.Reason, v.Detail)
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	require.NotEmpty(t, v.SwapLSN, "a run that committed its own swap reports the swap's WAL position")
	swap, err := decode.ParseLSN(v.SwapLSN)
	require.NoError(t, err)
	requireDoneWithTheSlotGone(t, ctx, h.pool, h.store, table, w.SlotName, &res.Attempts[len(res.Attempts)-1].RowPhase)
	require.NoError(t, w.Err, "the checkpoint poll failed")
	require.True(t, w.MarkSet, "the poll never saw the row before the cutover, so no write is known to have committed before the swap")
	requireMarkSpansTheStream(t, w, atStart)

	waitForLoad(t, load, func(p testutil.Progress) bool { return p.Inserts > atExit.Inserts },
		afterSwapInsertDeadline, "an insert after the swap")
	summary, err := load.Stop()
	require.NoError(t, err)
	res.recordLoad(summary, atExit)

	assertRowsConverged(t, ctx, pool, table, summary, swap, res)
	assertCatalogMatchesNative(t, pool, table, shape)
	window := swapWindow{opened: w.MarkAt, closed: exited}
	for _, f := range summary.Failures {
		assert.True(t, failureAllowed(f, window), "load failure the swap is not allowed to cause, at %s (mark %s, exit %s): SQLSTATE %q: %v",
			f.At.Format(time.RFC3339Nano), w.MarkAt.Format(time.RFC3339Nano), exited.Format(time.RFC3339Nano), f.Code, f.Err)
	}
}

// requireDoneWithTheSlotGone holds the finished run to its end state: the
// checkpoint row is done, and no replication slot is left under the name
// the engine derives for the table or the one the poll saw the row name.
// The row's phase is recorded in rowPhase before it is judged.
func requireDoneWithTheSlotGone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *checkpoint.Store, table testutil.WorkloadTable, seen string, rowPhase *string) {
	t.Helper()
	cp, err := store.Inspect(ctx, table.Schema, table.Table)
	require.NoError(t, err)
	*rowPhase = cp.Phase.String()
	require.Equal(t, checkpoint.PhaseDone, cp.Phase)
	var database string
	require.NoError(t, pool.QueryRow(ctx, `SELECT current_database()`).Scan(&database))
	for _, slot := range appendNamed([]string{preflight.CopySwapDecodingName(database, table.Schema, table.Table)}, seen) {
		_, found, err := slotHolder(ctx, pool, slot)
		require.NoError(t, err)
		assert.False(t, found, "the finished run dropped its replication slot %s", slot)
	}
}
