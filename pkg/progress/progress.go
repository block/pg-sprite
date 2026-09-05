// Package progress defines the strategy-wide, machine-readable execution
// progress contract. It deliberately contains copy counters that native
// operations leave empty so copy-and-swap can implement the same contract.
package progress

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/block/pg-sprite/pkg/dbconn"
)

// Clock supplies time to progress state so core executors remain deterministic.
type Clock interface {
	Now() time.Time
}

// WallClock reads the process wall clock.
type WallClock struct{}

// Now returns the current wall-clock time.
func (WallClock) Now() time.Time { return time.Now() }

// Phase is the overall execution phase.
type Phase string

const (
	// PhasePending means execution has not started.
	PhasePending Phase = "pending"
	// PhaseRunning means execution is active.
	PhaseRunning Phase = "running"
	// PhaseFinished means execution completed successfully.
	PhaseFinished Phase = "finished"
	// PhaseFailed means execution reached a terminal failure.
	PhaseFailed Phase = "failed"
)

// FormatVersion identifies the snapshot contract. A consumer must reject a
// snapshot whose format_version it does not recognize rather than guess at
// field semantics. Adding a phase or operation value is a contract change
// and bumps this version, even when no field is added or renamed. Adding a
// field also bumps this version so strict consumers can detect the new shape.
const FormatVersion = 3

// Operation is the current operation's execution class.
type Operation string

const (
	// OperationAdmitting is the pre-execution window in which a sequence's
	// steps are still being validated; no statement has run yet.
	OperationAdmitting Operation = "admitting"
	// OperationOptimistic is one bounded direct native attempt.
	OperationOptimistic Operation = "optimistic"
	// OperationBrief is a brief transactional sequence step.
	OperationBrief Operation = "brief"
	// OperationValidate is a constraint-validation scan.
	OperationValidate Operation = "validate-constraint"
	// OperationConcurrentIndex is a concurrent index build.
	OperationConcurrentIndex Operation = "concurrent-index-build"
)

// Work reports server-observed work. It is present only when the server
// published a progress row, and then every counter marshals explicitly — a
// fresh build reports honest zeros, never an empty object a consumer must
// guess at. Rows and bytes are reserved for copy-and-swap; native operations
// do not fabricate them.
type Work struct {
	RowsCopied   uint64 `json:"rows_copied"`
	RowsTotal    uint64 `json:"rows_total"`
	BytesCopied  uint64 `json:"bytes_copied"`
	BytesTotal   uint64 `json:"bytes_total"`
	BlocksDone   uint64 `json:"blocks_done"`
	BlocksTotal  uint64 `json:"blocks_total"`
	TuplesDone   uint64 `json:"tuples_done"`
	TuplesTotal  uint64 `json:"tuples_total"`
	LockersTotal uint64 `json:"lockers_total"`
	LockersDone  uint64 `json:"lockers_done"`
}

// Detail describes the operation currently executing.
type Detail struct {
	Operation Operation `json:"operation,omitempty"`
	// Statement is the canonical, qualified SQL the executor is running for
	// the current step, never a rendered or prettified form. Terminal snapshots
	// retain it so observers can identify the statement that produced the outcome.
	Statement        string `json:"statement,omitempty"`
	ServerPhase      string `json:"server_phase,omitempty"`
	Active           bool   `json:"active"`
	Attempt          int    `json:"attempt,omitempty"`
	Work             *Work  `json:"work,omitempty"`
	CurrentLockerPID uint32 `json:"current_locker_pid,omitempty"`
}

// Snapshot is one immutable progress observation. For a terminal phase the
// elapsed values are frozen at the instant Finish recorded, so a late poll
// reports the execution's duration, not the observation's age.
type Snapshot struct {
	FormatVersion int           `json:"format_version"`
	Phase         Phase         `json:"phase"`
	Step          int           `json:"step,omitempty"`
	TotalSteps    int           `json:"total_steps,omitempty"`
	Elapsed       time.Duration `json:"elapsed_ns"`
	StepElapsed   time.Duration `json:"step_elapsed_ns"`
	Detail        Detail        `json:"detail"`
}

// Tracker is a concurrency-safe progress source. The caller owns it; it has
// no goroutines. Progress performs the one read needed for an active index
// build, making polling lifetime identical to the caller's context.
//
// Two locks split the tracker's concerns: mu guards the state fields and is
// held only for memory access, so the executor's own updates never wait for
// a database read; pollMu serializes observers, so the reserved session —
// a single pgx connection that is not safe for concurrent use — only ever
// carries one progress query at a time.
type Tracker struct {
	mu        sync.RWMutex
	pollMu    sync.Mutex
	clock     Clock
	session   dbconn.RowQuerier
	phase     Phase
	started   time.Time
	stepStart time.Time
	ended     time.Time
	step      int
	total     int
	detail    Detail
	buildPID  uint32
}

// NewTracker constructs an idle tracker using clock.
func NewTracker(clock Clock) (*Tracker, error) {
	if clock == nil {
		return nil, fmt.Errorf("progress clock is required")
	}
	return &Tracker{clock: clock, phase: PhasePending}, nil
}

// Now returns the tracker's injected time for executor duration accounting.
func (t *Tracker) Now() time.Time { return t.clock.Now() }

// Start records the beginning of an execution. It resets all per-execution
// state, so a reused tracker never leaks a prior run's step, terminal time,
// or session into the new run's snapshots.
func (t *Tracker) Start(total int, operation Operation) {
	now := t.clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase, t.started, t.stepStart, t.ended = PhaseRunning, now, now, time.Time{}
	t.step, t.total, t.detail = 0, total, Detail{Operation: operation, Active: true}
	t.session, t.buildPID = nil, 0
}

// StartStep advances a sequence to a 1-based step, records the exact SQL the
// executor will run, and drops any build session from a prior step, so a later
// step can never poll a stale build.
func (t *Tracker) StartStep(step int, operation Operation, statement string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.step, t.stepStart = step, t.clock.Now()
	t.detail = Detail{Operation: operation, Statement: statement, Active: true}
	t.session, t.buildPID = nil, 0
}

// SetAttempt records the current bounded retry attempt.
func (t *Tracker) SetAttempt(attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.detail.Attempt = attempt
}

// SetConcurrentBuild enables on-demand server progress for pid. The executor
// supplies its reserved verdict session so polling cannot starve behind the
// build session even when the pool has only two connections.
func (t *Tracker) SetConcurrentBuild(session dbconn.RowQuerier, pid uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.session, t.buildPID = session, pid
}

var (
	// ErrNoActiveBuild is returned by CancelBuild when the tracker has no
	// concurrent index build in flight.
	ErrNoActiveBuild = errors.New("no concurrent index build is active")
	// ErrBuildNotRunning is returned by CancelBuild when the tracker has a
	// build but the server shows no statement running on its backend: the
	// build statement has not reached the server yet, or the backend has
	// already gone. PostgreSQL drops a cancel signal delivered to an idle
	// backend, so signalling now would silently do nothing; the caller
	// retries, or waits for the blocking executor call to return.
	ErrBuildNotRunning = errors.New("the concurrent index build's statement is not running on the server")
)

// CancelBuild asks PostgreSQL to cancel the active concurrent build's
// statement, from the tracker's reserved session. The tracker is the only
// party that knows whether the build is still live, so the cancel is issued
// here rather than by handing out the backend PID: it serializes against
// StopConcurrentBuild, which the executor calls before the build's session
// can return to the pool, so the PID it signals still belongs to the build
// — never to an unrelated statement that reused the same pooled backend.
// The signal is sent only to a backend the server reports active, so it
// cannot be dropped on an idle backend and mistaken for a delivered cancel.
// The build itself then returns through the executor's normal failure path
// with its catalog verdict; the caller observes that outcome on the
// blocking executor call, not here.
func (t *Tracker) CancelBuild(ctx context.Context) error {
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	t.mu.RLock()
	session, pid := t.session, t.buildPID
	t.mu.RUnlock()
	if pid == 0 || session == nil {
		return ErrNoActiveBuild
	}
	var cancelled bool
	err := session.QueryRow(ctx,
		`SELECT pg_cancel_backend(pid) FROM pg_catalog.pg_stat_activity WHERE pid = $1 AND state = 'active'`,
		pid).Scan(&cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, ErrBuildNotRunning)
	}
	if err != nil {
		return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, err)
	}
	if !cancelled {
		// The backend left between the activity read and the signal.
		return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, ErrBuildNotRunning)
	}
	return nil
}

// StopConcurrentBuild waits for an in-flight observation and releases the
// reserved session back to the executor before its catalog verdict.
func (t *Tracker) StopConcurrentBuild() {
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.session, t.buildPID = nil, 0
}

// Finish records a terminal execution outcome and the instant it happened;
// elapsed values in later snapshots freeze at that instant.
func (t *Tracker) Finish(err error) {
	now := t.clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.phase = PhaseFinished
	} else {
		t.phase = PhaseFailed
	}
	t.ended = now
	t.detail.Active = false
	t.session, t.buildPID = nil, 0
}

// Progress returns a snapshot and, for an active concurrent index build,
// queries PostgreSQL's progress view by the executor-owned backend PID. On a
// query error the snapshot still carries the last-known tracker state. The
// state lock is released before the query, so concurrent pollers serialize
// only against each other (and StopConcurrentBuild), never against the
// executor's own state updates.
func (t *Tracker) Progress(ctx context.Context) (Snapshot, error) {
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	t.mu.RLock()
	now := t.clock.Now()
	if !t.ended.IsZero() {
		now = t.ended
	}
	s := Snapshot{FormatVersion: FormatVersion, Phase: t.phase, Step: t.step, TotalSteps: t.total, Detail: t.detail}
	if !t.started.IsZero() {
		s.Elapsed = now.Sub(t.started)
		s.StepElapsed = now.Sub(t.stepStart)
	}
	session, pid := t.session, t.buildPID
	t.mu.RUnlock()
	if pid == 0 || session == nil || s.Phase != PhaseRunning {
		return s, nil
	}
	p, active, err := dbconn.ConcurrentIndexProgress(ctx, session, pid)
	if err != nil {
		return s, err
	}
	if !active {
		s.Detail.Active = false
		return s, nil
	}
	work := Work{
		BlocksDone: p.BlocksDone, BlocksTotal: p.BlocksTotal,
		TuplesDone: p.TuplesDone, TuplesTotal: p.TuplesTotal,
		LockersTotal: p.LockersTotal, LockersDone: p.LockersDone,
	}
	s.Detail.Active, s.Detail.ServerPhase, s.Detail.Work = true, p.Phase, &work
	s.Detail.CurrentLockerPID = p.CurrentLockerPID
	return s, nil
}
