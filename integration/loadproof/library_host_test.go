package loadproof_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// libraryCheckpointInterval is the CheckpointInterval the library proof
// hands migrate.Run: a host that cancels a long copy resumes from the last
// periodic save, so it may ask for saves more often than the engine's
// default, which the CLI has no flag for. It also puts a periodic save of
// the copy in reach of a table small enough for a smoke run, whose copy
// lasts well under a second.
const libraryCheckpointInterval = 100 * time.Millisecond

// trackerPollInterval paces the host's reads of the progress tracker, the
// way a host that shows a run's progress reads it.
const trackerPollInterval = 50 * time.Millisecond

// The causes the host cancels its context with, one per stop, so each
// cancelled attempt is matched to the stop that ended it.
var (
	errCancelledMidCopy   = errors.New("the host cancelled the run mid-copy")
	errCancelledMidVerify = errors.New("the host cancelled the run mid-verify")
)

// stopPoint is where the host cancels an attempt; stopNone runs it to its
// verdict.
type stopPoint int

const (
	stopNone stopPoint = iota
	stopMidCopy
	stopMidVerify
)

// cause is the cancel cause of the stop, nil for stopNone.
func (s stopPoint) cause() error {
	switch s {
	case stopMidCopy:
		return errCancelledMidCopy
	case stopMidVerify:
		return errCancelledMidVerify
	default:
		return nil
	}
}

// phase is the checkpoint phase the row must stand in after the stop.
func (s stopPoint) phase() checkpoint.Phase {
	switch s {
	case stopMidCopy:
		return checkpoint.PhaseCopying
	case stopMidVerify:
		return checkpoint.PhaseVerifying
	default:
		return checkpoint.PhaseDone
	}
}

// attemptInfo is one call to migrate.Run in the library proof's record.
type attemptInfo struct {
	Attempt int `json:"attempt"`
	// StartedMillis is when the call started, relative to the first
	// call's start; RunMillis is how long it took to return.
	StartedMillis int64 `json:"started_ms"`
	RunMillis     int64 `json:"run_ms"`
	// SlotIdleWaitMillis is how long the host waited, before the call, for
	// the server to let go of the slot the previous call held.
	SlotIdleWaitMillis int64 `json:"slot_idle_wait_ms"`
	// CancelledIn is the phase the host cancelled the call in, empty for
	// the call that ran to its verdict.
	CancelledIn string `json:"cancelled_in,omitempty"`
	// Saves are the phases of the checkpoint saves OnCheckpoint was handed,
	// in order, a run of saves in one phase recorded once.
	Saves []saveInfo `json:"saves"`
	// RowPhase is the checkpoint row's phase after the call returned, and
	// WatermarkAtStop the key a stopped copy had landed through.
	RowPhase        string `json:"row_phase"`
	WatermarkAtStop *int64 `json:"watermark_at_stop,omitempty"`
	// ShadowOID is the relation the call left the change's new table in:
	// after a stop, the shadow's; after the swap, the live table's, which
	// the swap made of the shadow. A call that resumed keeps it; one that
	// started over built a new shadow under a new OID.
	ShadowOID uint32 `json:"shadow_oid,omitempty"`
	// HeldConns is how many of the host's pool connections were still
	// counted as acquired when the call returned: a connection the cancel
	// broke stays counted until the pool has finished closing it, which it
	// does on a goroutine of its own.
	HeldConns int32 `json:"held_conns"`
	// CommittedWhileStopped is how many writes the load committed after a
	// cancelled call returned and before the host's next call started.
	CommittedWhileStopped int `json:"committed_while_stopped,omitempty"`
	// LastWork is the last of the tracker's counters the host read before
	// the call returned.
	LastWork *progress.Work `json:"last_work,omitempty"`
	Outcome  string         `json:"outcome"`
	Code     string         `json:"code,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// saveInfo is a phase OnCheckpoint first saw a save in, relative to the
// attempt's start, with the watermark that first save carried, when it
// carried one.
type saveInfo struct {
	Phase     string `json:"phase"`
	AtMillis  int64  `json:"at_ms"`
	Watermark *int64 `json:"watermark,omitempty"`
}

// host is what an importer holds to drive migrate.Run: its own engine
// pool, the connection it was dialed with, the statement, and the logger
// it hands the engine's diagnostics to.
type host struct {
	pool   *pgxpool.Pool
	conn   dbconn.Config
	st     statement.Statement
	free   int64
	store  *checkpoint.Store
	logger *slog.Logger
}

// newHost dials the engine's pool the way the CLI does: the operator's
// connection, with the pool floor the copy-and-swap route needs at its
// default sizing.
func newHost(t *testing.T, c config, alter string) host {
	t.Helper()
	conn := dbconn.Config{URL: c.DSN, MaxConnsFloor: migrate.CopySwapMinConns(copier.Options{})}
	pool, err := dbconn.NewPool(t.Context(), conn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	st, err := statement.ParseOne(alter)
	require.NoError(t, err)
	free, err := parseFreeDisk(c.FreeDisk)
	require.NoError(t, err, "LOAD_PROOF_FREE_DISK")
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	require.NoError(t, err)
	// The engine's decisions and warnings land in the test's output beside
	// the attempt log, as the binary's stderr does in the binary proof; the
	// per-save debug lines are left out, since the record lists the saves.
	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	return host{pool: pool, conn: conn, st: st, free: free, store: store, logger: logger}
}

// options is what the host hands migrate.Run. It starts from what the CLI
// sets for `migrate --copy-and-swap --keep-old --free-disk`: the library's
// DefaultOptions, which the CLI's flag defaults equal, and the same
// CopySwap fields, with the host's diagnostics logger. Beside them it sets
// three options only the library exposes — Progress, a tracker the host
// reads from its own goroutine (the CLI wires one only for its terminal
// display, never with --json); CopySwap.OnCheckpoint, the host's hook on
// every checkpoint save; and CopySwap.CheckpointInterval, how often the
// copy saves its watermark.
func (h host) options(tracker *progress.Tracker, onCheckpoint func(context.Context, checkpoint.Checkpoint) error) migrate.Options {
	opts := migrate.DefaultOptions()
	opts.CopySwap = migrate.CopySwapOptions{
		Enabled:            true,
		Connection:         h.conn,
		FreeDiskBytes:      h.free,
		KeepOld:            true,
		CheckpointInterval: libraryCheckpointInterval,
		OnCheckpoint:       onCheckpoint,
	}
	opts.Progress = tracker
	opts.Logger = h.logger
	return opts
}

// saveLog is what OnCheckpoint was handed during one attempt.
type saveLog struct {
	mu      sync.Mutex
	started time.Time
	saves   []saveInfo
}

func (l *saveLog) record(cp checkpoint.Checkpoint) {
	l.mu.Lock()
	defer l.mu.Unlock()
	phase := cp.Phase.String()
	if n := len(l.saves); n > 0 && l.saves[n-1].Phase == phase {
		return
	}
	save := saveInfo{Phase: phase, AtMillis: time.Since(l.started).Milliseconds()}
	if cp.Watermark.Valid() {
		landed := cp.Watermark.Value()
		save.Watermark = &landed
	}
	l.saves = append(l.saves, save)
}

func (l *saveLog) list() []saveInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]saveInfo(nil), l.saves...)
}

// midCopySave reports whether cp is a periodic save of a copy still
// running: in copying, with a landed watermark short of the key space. The
// fresh row has nothing landed and the copy's last save carries the
// complete watermark, so neither is one.
func midCopySave(cp checkpoint.Checkpoint) bool {
	if cp.Phase != checkpoint.PhaseCopying {
		return false
	}
	return cp.Watermark.Valid() && !cp.Watermark.Complete()
}

// midVerify reports whether the tracker shows the verification pass
// running with at least one chunk compared.
func midVerify(s progress.Snapshot) bool {
	if s.Phase != progress.PhaseRunning {
		return false
	}
	if s.Detail.Operation != progress.OperationChecksum {
		return false
	}
	return s.Detail.Work != nil && s.Detail.Work.ChunksCompared > 0
}

// trackerLog is the last tracker snapshot the host read during an attempt.
type trackerLog struct {
	mu   sync.Mutex
	last *progress.Work
}

func (l *trackerLog) record(s progress.Snapshot) {
	if s.Detail.Work == nil {
		return
	}
	work := *s.Detail.Work
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = &work
}

func (l *trackerLog) lastWork() *progress.Work {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// pollTracker reads the tracker every trackerPollInterval until ctx ends,
// handing each snapshot to seen. A read the ending ctx cut short is not a
// failure; any other is returned.
func pollTracker(ctx context.Context, tracker *progress.Tracker, seen func(progress.Snapshot)) error {
	ticker := time.NewTicker(trackerPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		s, err := tracker.Progress(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		seen(s)
	}
}

// attempt is what one call to migrate.Run returned, and its record.
type attempt struct {
	verdict verdict.Verdict
	err     error
	info    attemptInfo
}

// runAttempt is one call to migrate.Run as the host makes it, cancelled at
// stop: mid-copy from OnCheckpoint, on a periodic save of a copy still
// running, and mid-verify from the host's tracker poll, once the pass has
// compared a chunk. It returns what the call returned and the attempt's
// record without its row reading, which the caller adds; its own error is
// the host's: a tracker it could not build or read.
func runAttempt(ctx context.Context, h host, stop stopPoint, started time.Time) (attempt, error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tracker, err := progress.NewTracker(progress.WallClock{})
	if err != nil {
		return attempt{}, fmt.Errorf("progress tracker: %w", err)
	}
	attemptStart := time.Now()
	saves := &saveLog{started: attemptStart}
	work := &trackerLog{}
	opts := h.options(tracker, func(_ context.Context, cp checkpoint.Checkpoint) error {
		saves.record(cp)
		if stop == stopMidCopy && midCopySave(cp) {
			cancel(errCancelledMidCopy)
		}
		return nil
	})

	pollCtx, stopPoll := context.WithCancel(runCtx)
	var (
		wg      sync.WaitGroup
		pollErr error
	)
	wg.Go(func() {
		pollErr = pollTracker(pollCtx, tracker, func(s progress.Snapshot) {
			work.record(s)
			if stop == stopMidVerify && midVerify(s) {
				cancel(errCancelledMidVerify)
			}
		})
	})
	v, runErr := migrate.Run(runCtx, h.pool, h.st, opts)
	returned := time.Now()
	stopPoll()
	wg.Wait()

	info := attemptInfo{
		StartedMillis: attemptStart.Sub(started).Milliseconds(),
		RunMillis:     returned.Sub(attemptStart).Milliseconds(),
		Saves:         saves.list(),
		HeldConns:     h.pool.Stat().AcquiredConns(),
		LastWork:      work.lastWork(),
		Outcome:       string(v.Outcome),
		Code:          v.Code,
	}
	if runErr != nil {
		info.Error = runErr.Error()
	}
	if stop != stopNone && errors.Is(runErr, stop.cause()) {
		info.CancelledIn = stop.phase().String()
	}
	if pollErr != nil {
		pollErr = fmt.Errorf("read the progress tracker: %w", pollErr)
	}
	return attempt{verdict: v, err: runErr, info: info}, pollErr
}
