package loadproof_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/internal/testutil"
	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/verdict"
)

// defaultKillResults is where the kill proof writes its record, beside the
// other proofs', so one run does not overwrite another's.
const defaultKillResults = "results-kill.json"

// killPollInterval paces the kill poll's reads of the checkpoint row and
// the two relations' sizes: a smoke-size copy lasts well under a second,
// so the poll reads it several times before it ends.
const killPollInterval = 50 * time.Millisecond

// killRun is one run of the binary in the kill proof's record.
type killRun struct {
	Run int `json:"run"`
	// StartedMillis is when the run started, relative to the first run's
	// start; RunMillis is how long it ran until it exited or was killed.
	StartedMillis int64 `json:"started_ms"`
	RunMillis     int64 `json:"run_ms"`
	// SlotReleaseWaitMillis is how long the harness waited, before the
	// run, for the server to let go of the slot the killed run held.
	SlotReleaseWaitMillis int64 `json:"slot_release_wait_ms"`
	// KilledIn is the phase the poll saw when it sent SIGKILL, empty for
	// the run left to finish; ShadowBytesAtKill and SourceBytesAtKill are
	// the heap sizes it read with it.
	KilledIn          string `json:"killed_in,omitempty"`
	ShadowBytesAtKill int64  `json:"shadow_bytes_at_kill,omitempty"`
	SourceBytesAtKill int64  `json:"source_bytes_at_kill,omitempty"`
	// ExitSignal names the signal that ended the binary, empty when it
	// exited on its own; ExitCode is its exit code, -1 when a signal ended
	// it.
	ExitSignal string `json:"exit_signal,omitempty"`
	ExitCode   int    `json:"exit_code"`
	// RowPhase is the checkpoint row's phase after the run, and
	// WatermarkAtKill the key the row's watermark had landed through when
	// the run was killed: the save the next run resumes from.
	// FirstSavedWatermark is the first watermark the kill poll saw the run
	// save in copying, the complete watermark's key when the copy had
	// ended before the poll saw a partial one, and absent when the poll
	// saw no copying save; for the killed copy's rerun it is what
	// checkResumedFrom judges.
	RowPhase            string `json:"row_phase"`
	WatermarkAtKill     *int64 `json:"watermark_at_kill,omitempty"`
	FirstSavedWatermark *int64 `json:"first_saved_watermark,omitempty"`
	// ShadowOID is the relation the run left the change's new table in:
	// after a kill, the shadow's; after the swap, the live table's. A run
	// that resumed keeps it; one that started over built another.
	ShadowOID uint32 `json:"shadow_oid,omitempty"`
	// CommittedWhileStopped is how many writes the load committed after a
	// killed run died and before the next run started.
	CommittedWhileStopped int `json:"committed_while_stopped,omitempty"`
	// Outcome and Code are the verdict's, for the run that printed one.
	Outcome string `json:"outcome,omitempty"`
	Code    string `json:"code,omitempty"`
}

// readKillConfig skips the test unless the operator asked for the kill
// proof, then reads the configuration the binary proof reads.
func readKillConfig(t *testing.T) config {
	t.Helper()
	if os.Getenv("LOAD_PROOF_KILL") != "1" {
		t.Skip("set LOAD_PROOF_KILL=1 to run the kill load proof; `make load-proof-kill` sets everything")
	}
	c := readTarget(t, defaultKillResults)
	c.Binary = os.Getenv("LOAD_PROOF_BIN")
	require.NotEmpty(t, c.Binary, "set LOAD_PROOF_BIN to a freshly built pg-sprite binary")
	return c
}

// The crash proof: the same identity-key retype as TestLoadProof, under
// the same load, run through the same binary with the same flags. The
// harness kills the binary with SIGKILL once mid-copy and once in the
// verification pass — no cleanup, no last save, its connections dropped
// by the server — and after each kill runs the same command again, as an
// operator does after a crash, once the server has let go of the dead
// run's slot. The run after the last kill is left to finish, and the
// result is judged by the binary proof's own oracles.
func TestLoadProofKill(t *testing.T) {
	c := readKillConfig(t)
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
	t.Logf("kill load proof server version %s", res.ServerVersion)
	res.Binary.SHA256 = fileSHA256(t, c.Binary)

	shape := testutil.WorkloadShape{IDType: "integer"}
	table := testutil.NewWorkloadTableOf(t, pool, shape)
	removeLeftoversOnCleanup(t, pool, table, release)
	t.Logf("seeding %d rows into %s", c.Rows, table.Qualified())
	require.NoError(t, table.SeedRows(ctx, c.Rows))
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)

	load := startProofLoad(t, pool, table, c)

	runCtx, cancelRun := context.WithTimeout(ctx, runBudget)
	defer cancelRun()
	watch := startWatch(t, pool, table, load)
	atStart := load.Progress()
	started := time.Now()
	res.StartedAt = started
	shadow, waited, killsErr := killAndRerun(t, ctx, runCtx, c, pool, store, table, load, release, started, res)
	if killsErr != nil {
		// The phases the poll saw are the failed run's forensics.
		res.recordWatch(watch.stop(), started)
		res.recordLoad(load.Snapshot(), load.Progress())
	}
	require.NoError(t, killsErr)

	finalStart := time.Now()
	output, diagnostic, code := runBinary(t, runCtx, c, migrateArgs(c, table)...)
	exited := time.Now()
	atExit := load.Progress()
	res.RunMillis = exited.Sub(started).Milliseconds()
	w := watch.stop()
	res.recordWatch(w, started)
	res.recordOutput(output)
	// As in the binary proof, the load is recorded before any judgement.
	res.recordLoad(load.Snapshot(), atExit)
	res.Runs = append(res.Runs, killRun{
		Run:                   len(res.Runs) + 1,
		StartedMillis:         finalStart.Sub(started).Milliseconds(),
		RunMillis:             exited.Sub(finalStart).Milliseconds(),
		SlotReleaseWaitMillis: waited.Milliseconds(),
		ExitCode:              code,
	})
	final := &res.Runs[len(res.Runs)-1]

	require.Equal(t, 0, code, "binary stdout: %s; stderr: %s", output, diagnostic)
	var v verdict.Verdict
	require.NoError(t, json.Unmarshal(output, &v), "verdict JSON: %s", output)
	final.Outcome, final.Code = string(v.Outcome), v.Code
	require.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome, "%s: %s", v.Reason, v.Detail)
	require.NotEmpty(t, v.SwapLSN, "a run that committed its own swap reports the swap's WAL position")
	swap, err := decode.ParseLSN(v.SwapLSN)
	require.NoError(t, err)
	requireDoneWithTheSlotGone(t, ctx, pool, store, table, w.SlotName, &final.RowPhase)
	final.ShadowOID, err = relationOID(ctx, pool, table.Schema, table.Table)
	require.NoError(t, err)
	require.Equal(t, shadow, final.ShadowOID, "the live table is the shadow the first killed run built, so every rerun resumed it rather than starting over")
	require.NoError(t, w.Err, "the checkpoint poll failed")
	require.NoError(t, checkNoCopyAfterVerify(w.Sequence), "the run resumed from the verifying row verifies again without copying again")
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

// killAndRerun runs the binary once per kill point, killing it there with
// SIGKILL, and holds each kill to the phase it was aimed at
// (checkKilledAt), each rerun to the shadow the first run built, and the
// rerun of the killed copy to the save it left (checkResumedFrom). Before
// every next run it waits, for at most release, for the server to let go
// of the dead run's slot — a rerun that met it held would be refused as
// resume-slot-held — and for the load to commit a few writes per worker,
// so the stopped interval holds writes only the kept slot can carry into
// the new table. It records every run in res as it ends and returns the
// shadow's OID and the last slot wait, with an error when a kill did not
// land where it was aimed, a rerun built another shadow, or a wait ran
// out. An operator's interrupt fails the test at once, so its cleanups
// run.
func killAndRerun(t *testing.T, ctx, runCtx context.Context, c config, pool *pgxpool.Pool, store *checkpoint.Store, table testutil.WorkloadTable, load *testutil.LoadGenerator, release time.Duration, started time.Time, res *results) (uint32, time.Duration, error) {
	t.Helper()
	var (
		shadow uint32
		waited time.Duration
		// began is the watermark the row carried when the run started, and
		// copyKilledAt the save a killed copy left for the run to resume.
		began        copier.Watermark
		copyKilledAt *int64
	)
	for _, point := range killPoints() {
		k, err := runUntilKilled(t, ctx, runCtx, c, pool, store, table, point, began)
		res.Runs = append(res.Runs, killRun{
			Run:                   len(res.Runs) + 1,
			StartedMillis:         k.started.Sub(started).Milliseconds(),
			RunMillis:             k.exited.Sub(k.started).Milliseconds(),
			SlotReleaseWaitMillis: waited.Milliseconds(),
			ShadowBytesAtKill:     k.fill.ShadowBytes,
			SourceBytesAtKill:     k.fill.SourceBytes,
			ExitCode:              k.code,
		})
		run := &res.Runs[len(res.Runs)-1]
		if k.sent {
			run.KilledIn = point.phase().String()
		}
		if k.signal != 0 {
			run.ExitSignal = k.signal.String()
		}
		if k.firstSaved.Valid() {
			saved := k.firstSaved.Value()
			run.FirstSavedWatermark = &saved
		}
		if err != nil {
			return 0, 0, fmt.Errorf("run %d: %w", run.Run, err)
		}
		if copyKilledAt != nil {
			if err := checkResumedFrom(*copyKilledAt, k.firstSaved); err != nil {
				return 0, 0, fmt.Errorf("run %d: %w", run.Run, err)
			}
			copyKilledAt = nil
		}
		committedAtKill := load.Progress().Committed()
		cp, err := store.Inspect(runCtx, table.Schema, table.Table)
		if err != nil {
			return 0, 0, fmt.Errorf("run %d: inspect the checkpoint row after the kill: %w", run.Run, err)
		}
		run.RowPhase = cp.Phase.String()
		if cp.Watermark.Valid() {
			landed := cp.Watermark.Value()
			run.WatermarkAtKill = &landed
		}
		if err := checkKilledAt(point, cp, k.signal, k.code, k.stdout); err != nil {
			return 0, 0, fmt.Errorf("run %d: %w; stderr: %s", run.Run, err, k.stderr)
		}
		began = cp.Watermark
		if point == killMidCopy {
			copyKilledAt = run.WatermarkAtKill
		}
		if run.ShadowOID, err = relationOID(runCtx, pool, table.Schema, cp.ShadowTable); err != nil {
			return 0, 0, fmt.Errorf("run %d: read the shadow the killed run left: %w", run.Run, err)
		}
		if shadow != 0 && run.ShadowOID != shadow {
			return 0, 0, fmt.Errorf("run %d left its new table in relation %d, not %d, the shadow run 1 built, so it started over rather than resuming",
				run.Run, run.ShadowOID, shadow)
		}
		shadow = run.ShadowOID
		t.Logf("run %d killed in %s after %s; the row stands in %s", run.Run, run.KilledIn,
			time.Duration(run.RunMillis)*time.Millisecond, run.RowPhase)
		if waited, err = waitForSlotIdle(runCtx, pool, store, table, release); err != nil {
			return 0, 0, fmt.Errorf("before run %d: %w", run.Run+1, err)
		}
		if run.CommittedWhileStopped, err = waitForStoppedWrites(runCtx, load, committedAtKill, stoppedWritesPerWorker*c.Workers); err != nil {
			return 0, 0, fmt.Errorf("before run %d: %w", run.Run+1, err)
		}
	}
	return shadow, waited, nil
}

// killed is how a run the harness meant to kill ended.
type killed struct {
	started, exited time.Time
	// sent is whether the poll sent SIGKILL, and fill the heap sizes it
	// read when it did.
	sent bool
	fill copyFill
	// firstSaved is the first watermark the poll saw the row carry in
	// copying other than the one it carried when the run started: the
	// run's first copying save the poll saw, invalid when it saw none.
	firstSaved copier.Watermark
	// signal and code are how the binary ended: SIGKILL and -1 when the
	// kill landed, zero and its exit code when it exited on its own first.
	signal syscall.Signal
	code   int
	// stdout and stderr are the binary's, kept for a run that exited on
	// its own.
	stdout, stderr string
}

// runUntilKilled runs the binary with the proof's command line and kills
// it with SIGKILL from a poll of the checkpoint row once the row shows the
// run at point, noting the first copying save the poll sees move off
// began, the watermark the row carried before the run. It returns how the
// run ended; its error is the harness's: a binary that could not start or
// a poll that failed. When ctx ends — the operator interrupted the proof —
// the binary is killed and waited for, and the test fails so its cleanups
// run.
func runUntilKilled(t *testing.T, ctx, runCtx context.Context, c config, pool *pgxpool.Pool, store *checkpoint.Store, table testutil.WorkloadTable, point killPoint, began copier.Watermark) (killed, error) {
	t.Helper()
	cmd := exec.CommandContext(runCtx, c.Binary, migrateArgs(c, table)...)
	cmd.Env = append(os.Environ(), "PGSPRITE_URL="+c.DSN)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	k := killed{started: time.Now()}
	if err := cmd.Start(); err != nil {
		k.exited = time.Now()
		return k, fmt.Errorf("start the binary: %w", err)
	}
	pollCtx, stopPoll := context.WithCancel(runCtx)
	var (
		wg      sync.WaitGroup
		pollErr error
	)
	// The poll's goroutine alone writes firstSaved until it is waited for.
	seen := func(cp checkpoint.Checkpoint) {
		if k.firstSaved.Valid() || cp.Phase != checkpoint.PhaseCopying {
			return
		}
		if cp.Watermark.Valid() && cp.Watermark != began {
			k.firstSaved = cp.Watermark
		}
	}
	wg.Go(func() {
		k.sent, k.fill, pollErr = killWhenReached(pollCtx, pool, store, table, point, cmd.Process, seen)
	})
	waitErr := cmd.Wait()
	k.exited = time.Now()
	stopPoll()
	wg.Wait()
	require.NoError(t, context.Cause(ctx), "the proof was interrupted: the binary was killed, and the cleanups remove its replication slot and the seeded table")
	k.stdout, k.stderr = stdout.String(), stderr.String()
	k.signal, k.code = exitSignal(cmd.ProcessState), cmd.ProcessState.ExitCode()
	var exit *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exit) {
		return k, fmt.Errorf("wait for the binary: %w", waitErr)
	}
	if pollErr != nil {
		return k, fmt.Errorf("the kill poll failed: %w", pollErr)
	}
	return k, nil
}

// killWhenReached polls the checkpoint row and the two relations' heap
// sizes every killPollInterval until a reading shows the run at point,
// then sends proc SIGKILL, handing seen every row it reads. It reports
// whether it sent the signal to a process still running, and the sizes it
// read then. It ends without sending when ctx ends — the binary exited
// first — and returns any read error other than the row or its table not
// existing yet.
func killWhenReached(ctx context.Context, pool *pgxpool.Pool, store *checkpoint.Store, table testutil.WorkloadTable, point killPoint, proc *os.Process, seen func(checkpoint.Checkpoint)) (bool, copyFill, error) {
	ticker := time.NewTicker(killPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, copyFill{}, nil
		case <-ticker.C:
		}
		cp, err := store.Inspect(ctx, table.Schema, table.Table)
		if errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrTableMissing) {
			continue
		}
		if ctx.Err() != nil {
			return false, copyFill{}, nil
		}
		if err != nil {
			return false, copyFill{}, fmt.Errorf("inspect the checkpoint row: %w", err)
		}
		seen(cp)
		fill, err := readCopyFill(ctx, pool, table.Schema, table.Table, cp.ShadowTable)
		if ctx.Err() != nil {
			return false, copyFill{}, nil
		}
		if err != nil {
			return false, copyFill{}, err
		}
		if !point.reached(cp, fill) {
			continue
		}
		if err := proc.Signal(syscall.SIGKILL); err != nil {
			if errors.Is(err, os.ErrProcessDone) {
				return false, fill, nil
			}
			return false, fill, fmt.Errorf("send the binary SIGKILL: %w", err)
		}
		return true, fill, nil
	}
}

// readCopyFill reads the heap size of the shadow and of the source, zero
// for a relation that does not exist.
func readCopyFill(ctx context.Context, pool *pgxpool.Pool, schema, source, shadow string) (copyFill, error) {
	var fill copyFill
	err := pool.QueryRow(ctx, `
		SELECT
			coalesce(pg_relation_size(to_regclass($1)), 0),
			coalesce(pg_relation_size(to_regclass($2)), 0)`,
		pgx.Identifier{schema, shadow}.Sanitize(),
		pgx.Identifier{schema, source}.Sanitize(),
	).Scan(&fill.ShadowBytes, &fill.SourceBytes)
	if err != nil {
		return copyFill{}, fmt.Errorf("read the heap sizes of %s and %s: %w", shadow, source, err)
	}
	return fill, nil
}
