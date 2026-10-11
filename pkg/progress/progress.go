// Package progress defines the strategy-wide, machine-readable execution
// progress contract. One snapshot shape serves every operation: a concurrent
// index build's counters are read from the server's progress view, the
// copy-and-swap row copy's and checksum pass's counters come from the
// engine's own WorkSource, and each operation leaves the others' counters
// at zero.
package progress

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
const FormatVersion = 6

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
	// OperationCopy is the copy-and-swap row copy from the source table into
	// its shadow.
	OperationCopy Operation = "copy"
	// OperationChecksum is the copy-and-swap checksum pass comparing the
	// source table with its shadow chunk by chunk, and repairing differing
	// chunks when its policy says so.
	OperationChecksum Operation = "checksum"
	// OperationCatchUp is the copy-and-swap catch-up applying the changes
	// decoded from the source table's WAL to its shadow.
	OperationCatchUp Operation = "catch-up"
)

// Work reports observed work. It is present only when something measured
// it — the server published a progress row for a concurrent build, or the
// engine's own WorkSource reported the step's counters — and then every
// counter marshals explicitly — a fresh build reports honest zeros, never an
// empty object a consumer must guess at. Rows and bytes belong to the
// copy-and-swap row copy; chunks compared, rows hashed, chunks mismatched,
// chunks repaired and chunks reread to the checksum pass; changes applied,
// changes buffered and lag bytes to the catch-up; blocks, tuples and
// lockers to a concurrent index build. No operation fabricates another's
// counters.
type Work struct {
	RowsCopied       uint64 `json:"rows_copied"`
	RowsTotal        uint64 `json:"rows_total"`
	BytesCopied      uint64 `json:"bytes_copied"`
	BytesTotal       uint64 `json:"bytes_total"`
	ChunksCompared   uint64 `json:"chunks_compared"`
	RowsHashed       uint64 `json:"rows_hashed"`
	ChunksMismatched uint64 `json:"chunks_mismatched"`
	ChunksRepaired   uint64 `json:"chunks_repaired"`
	ChunksReread     uint64 `json:"chunks_reread"`
	ChangesApplied   uint64 `json:"changes_applied"`
	ChangesBuffered  uint64 `json:"changes_buffered"`
	LagBytes         uint64 `json:"lag_bytes"`
	BlocksDone       uint64 `json:"blocks_done"`
	BlocksTotal      uint64 `json:"blocks_total"`
	TuplesDone       uint64 `json:"tuples_done"`
	TuplesTotal      uint64 `json:"tuples_total"`
	LockersTotal     uint64 `json:"lockers_total"`
	LockersDone      uint64 `json:"lockers_done"`
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
// build, or the one WorkSource call for an engine-measured step, making
// polling lifetime identical to the caller's context.
//
// Two guards split the tracker's concerns: mu guards the state fields and
// is held only for memory access, so the executor's own updates never wait
// for a database read; the poll gate serializes observers, so the reserved
// session — a single pgx connection that is not safe for concurrent use —
// only ever carries one progress query at a time, and a WorkSource sees one
// poll at a time. The gate is a one-slot channel rather than a mutex so
// that CancelBuild, the one taker with a caller waiting on an answer, can
// stop waiting when that caller's context ends.
//
// Four state changes take the poll gate, because each ends a poll target's
// ownership of the step's work and must not do so under a poll still in
// flight: StopConcurrentBuild and SetWorkSource release the build's
// session, which the executor calls before that session can return to the
// pool, so an observation or cancel signal in flight completes against a
// backend the build still owns; StopWorkSource and SetConcurrentBuild
// release the source, which the engine calls before the state the source
// reads goes away. Each takes the gate before mu, the one order every taker
// uses. Start, StartStep and Finish also clear those fields, but as resets
// under mu alone — by the time they run the step has already passed through
// its fence, and a reset that waited behind an observation would make
// polling a gate on execution.
//
// A Tracker comes from NewTracker, which creates the gate; the zero value
// has no gate and must not be used.
type Tracker struct {
	mu        sync.RWMutex
	pollGate  chan struct{}
	clock     Clock
	session   dbconn.RowQuerier
	source    WorkSource
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
	return &Tracker{clock: clock, phase: PhasePending, pollGate: make(chan struct{}, 1)}, nil
}

// Now returns the tracker's injected time for executor duration accounting.
func (t *Tracker) Now() time.Time { return t.clock.Now() }

// takePollGate waits, without limit, for the poll gate. The observers and
// the handoff fences take it this way: a poll is bounded by its own
// context once it holds the gate, and a fence waits only for that poll.
func (t *Tracker) takePollGate() {
	t.pollGate <- struct{}{}
}

// takePollGateOrGiveUp waits for the poll gate until ctx ends, and reports
// ctx's error — holding nothing — when the caller gives up first. When the
// gate frees and ctx ends in the same instant either outcome may be
// reported; both are honest, because nothing has been done yet.
func (t *Tracker) takePollGateOrGiveUp(ctx context.Context) error {
	select {
	case t.pollGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releasePollGate hands the gate to the next waiter.
func (t *Tracker) releasePollGate() {
	<-t.pollGate
}

// Start records the beginning of an execution. It resets all per-execution
// state, so a reused tracker never leaks a prior run's step, terminal time,
// or session into the new run's snapshots.
func (t *Tracker) Start(total int, operation Operation) {
	now := t.clock.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase, t.started, t.stepStart, t.ended = PhaseRunning, now, now, time.Time{}
	t.step, t.total, t.detail = 0, total, Detail{Operation: operation, Active: true}
	t.session, t.buildPID, t.source = nil, 0, nil
}

// StartStep advances a sequence to a 1-based step, records the exact SQL the
// executor will run, and drops any build session or work source from a prior
// step, so a later step can never poll a stale build or a finished source.
func (t *Tracker) StartStep(step int, operation Operation, statement string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.step, t.stepStart = step, t.clock.Now()
	t.detail = Detail{Operation: operation, Statement: statement, Active: true}
	t.session, t.buildPID, t.source = nil, 0, nil
}

// SetAttempt records the current bounded retry attempt.
func (t *Tracker) SetAttempt(attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.detail.Attempt = attempt
}

// SetConcurrentBuild enables on-demand server progress for pid. The executor
// supplies its reserved verdict session so polling cannot starve behind the
// build session even when the pool has only two connections. A step's work
// comes from one place, so any WorkSource the step had is dropped — after
// waiting, as StopWorkSource does, for a poll still reading it, so the
// source's owner never finds its state observed after the handoff.
func (t *Tracker) SetConcurrentBuild(session dbconn.RowQuerier, pid uint32) {
	t.takePollGate()
	defer t.releasePollGate()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.session, t.buildPID = session, pid
	t.source = nil
}

var (
	// ErrNoActiveBuild is returned by CancelBuild when the tracker has no
	// concurrent index build in flight.
	ErrNoActiveBuild = errors.New("no concurrent index build is active")
	// ErrBuildNotRunning is returned by CancelBuild when the tracker has a
	// build but the server positively shows no statement running on its
	// backend: the build statement has not reached the server yet, or the
	// backend has already gone. PostgreSQL drops a cancel signal delivered
	// to an idle backend, so signalling now would silently do nothing; the
	// caller retries, or waits for the blocking executor call to return.
	ErrBuildNotRunning = errors.New("the concurrent index build's statement is not running on the server")
	// ErrBuildUnobservable is returned by CancelBuild when the server does
	// not expose the build backend's state — activity tracking is off, or
	// the backend is hidden from the tracker's role — so the tracker cannot
	// tell a running build from an idle backend. A signal sent blind could
	// be dropped on an idle backend while the caller reads nil as a cancel
	// that reached the build, so none is sent. The build is still
	// stoppable: an operator whose role can see the backend cancels it
	// directly, or the caller ends the context the build runs under.
	ErrBuildUnobservable = errors.New("the server does not expose the concurrent index build backend's state")
	// ErrCancelNotDispatched is returned by CancelBuild when the caller's
	// context ended before the cancel signal was sent — while CancelBuild
	// waited for an in-flight progress poll or another cancel to release
	// the reserved session, or in the instant it took the session. It is
	// returned wrapped together with the caller's context error, so
	// errors.Is matches both; test for this sentinel before testing for
	// the context error. A context error without it came back after the
	// signal was sent, and the signal may have reached the build. The
	// other sentinels CancelBuild returns also mean nothing was delivered;
	// see CancelBuild for the full outcome table.
	ErrCancelNotDispatched = errors.New("the cancel signal was not sent")
)

// cancelSignalTimeout bounds the cancel signal's round trip. The signal
// runs on the executor's reserved verdict session, never under the
// caller's own context: a caller deadline expiring mid-query would tear
// down that session and leave the build's failure verdict indeterminate —
// the outcome the reserved session exists to prevent.
const cancelSignalTimeout = 5 * time.Second

// cancelBuildSQL reads the build backend's state and signals it only when
// the server reports it active, in one statement so the read and the
// signal cannot straddle a state change the tracker then misreports.
// Every catalog name is pg_catalog-qualified, operators included (CO-9):
// a user schema ahead of pg_catalog on search_path could otherwise
// substitute a pg_cancel_backend that returns true and signals nothing.
const cancelBuildSQL = `SELECT state,
       CASE WHEN state OPERATOR(pg_catalog.=) 'active' THEN pg_catalog.pg_cancel_backend(pid) ELSE false END
  FROM pg_catalog.pg_stat_activity
 WHERE pid OPERATOR(pg_catalog.=) $1`

// CancelBuild asks PostgreSQL to cancel the active concurrent build's
// statement, from the tracker's reserved session. The tracker is the only
// party that knows whether the build is still live, so the cancel is issued
// here rather than by handing out the backend PID: it serializes against
// StopConcurrentBuild, which the executor calls before the build's session
// can return to the pool, so the PID it signals still belongs to the build
// — never to an unrelated statement that reused the same pooled backend.
// A step whose work a WorkSource owns has no build to signal and reports
// ErrNoActiveBuild; the snapshot's operation tells a caller which case it
// is in. The signal is sent only to a backend the server reports active in the
// same statement as the read, which rules out the common way a cancel is
// lost — a signal landing on an idle backend — without making the signal
// itself observable.
//
// A nil return means pg_cancel_backend accepted the signal for a backend
// the same statement had just read as active. It does not mean the build
// has stopped, and it cannot rule out the build finishing in the instant
// between that read and the signal, in which case the signal lands on an
// idle backend and is dropped: the server acts on a cancel at its next
// interrupt check, and the build returns through the executor's normal
// path — failed with its catalog verdict, or finished — either way on the
// blocking executor call, not here. The caller's ctx gates whether the
// signal is attempted; the signal itself runs bounded and detached from it,
// because the session it rides on is the one the build's verdict needs
// intact.
//
// CancelBuild waits for an in-flight progress poll or another cancel to
// release the reserved session, for as long as the caller's ctx lasts. A
// caller whose ctx ends first — during that wait, or by the time the
// session is held — gets ErrCancelNotDispatched wrapped with its context
// error, and no signal was sent. The outcomes sort into four classes:
//
//   - nil: the signal was sent and accepted.
//   - ErrCancelNotDispatched, with the context error: nothing was sent;
//     the caller may retry.
//   - ErrNoActiveBuild, ErrBuildNotRunning, ErrBuildUnobservable: nothing
//     was delivered to the build, for the reason each sentinel names.
//   - any other error, a context error without ErrCancelNotDispatched
//     included: it came back after the signal query was issued, and the
//     signal may have reached the build.
//
// Because the second class matches both the sentinel and the context
// error, a caller tests for ErrCancelNotDispatched before it tests for a
// context error; the other order reads "nothing sent" as "may have been
// sent".
//
// The reserved session's role must be allowed to signal the build's
// backend: the same role, or a member of pg_signal_backend. Otherwise
// pg_cancel_backend raises an error rather than returning false, and that
// error is returned wrapped; it is a permanent condition of the role, not
// one a retry clears.
func (t *Tracker) CancelBuild(ctx context.Context) error {
	if err := t.takePollGateOrGiveUp(ctx); err != nil {
		// The caller gave up while a poll or another cancel held the
		// session; nothing was sent. A build that ended meanwhile is
		// reported as such, not as a cancel left unsent.
		_, pid, ok := t.activeBuild()
		if !ok {
			return ErrNoActiveBuild
		}
		return fmt.Errorf("cancel concurrent index build backend %d: %w: %w", pid, ErrCancelNotDispatched, err)
	}
	defer t.releasePollGate()
	session, pid, ok := t.activeBuild()
	if !ok {
		return ErrNoActiveBuild
	}
	if err := ctx.Err(); err != nil {
		// The caller gave up in the instant the session came free; the
		// gate is held, so this is the last point at which nothing has
		// been sent.
		return fmt.Errorf("cancel concurrent index build backend %d: %w: %w", pid, ErrCancelNotDispatched, err)
	}
	signalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelSignalTimeout)
	defer cancel()
	var state *string
	var cancelled bool
	err := session.QueryRow(signalCtx, cancelBuildSQL, pid).Scan(&state, &cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		// The backend is gone; a disconnected backend runs nothing.
		return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, ErrBuildNotRunning)
	}
	if err != nil {
		return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, err)
	}
	switch classifyBuildBackend(state) {
	case buildBackendActive:
		if !cancelled {
			// The backend left between the activity read and the signal.
			return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, ErrBuildNotRunning)
		}
		return nil
	case buildBackendIdle:
		return fmt.Errorf("cancel concurrent index build backend %d: %w", pid, ErrBuildNotRunning)
	default:
		return fmt.Errorf("cancel concurrent index build backend %d reports state %s: %w",
			pid, describeState(state), ErrBuildUnobservable)
	}
}

// activeBuild reads the concurrent build the tracker is polling, if any:
// its reserved session and backend PID, and whether there is one.
func (t *Tracker) activeBuild() (dbconn.RowQuerier, uint32, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.buildPID == 0 || t.session == nil {
		return nil, 0, false
	}
	return t.session, t.buildPID, true
}

// buildBackendVerdict is the classification of one pg_stat_activity state
// observation for the purpose of signalling the build's backend.
type buildBackendVerdict int

const (
	// buildBackendActive means the backend is executing a statement, so a
	// cancel signal reaches it.
	buildBackendActive buildBackendVerdict = iota
	// buildBackendIdle means the backend positively runs nothing; a cancel
	// signal would be dropped.
	buildBackendIdle
	// buildBackendUnobservable means the server does not expose the
	// state: NULL for a backend hidden from this role, "disabled" when
	// activity tracking is off, or a state outside the vocabulary.
	buildBackendUnobservable
)

// classifyBuildBackend maps one observed pg_stat_activity state to its
// cancel verdict. The cases are the documented pg_stat_activity.state
// vocabulary (PostgreSQL 14–18). Only "active" is signalled — the build
// statement is an ordinary query, never a fastpath function call — and
// only the idle states count as positively not running; everything else
// is unobservable rather than assumed either way.
func classifyBuildBackend(state *string) buildBackendVerdict {
	if state == nil {
		return buildBackendUnobservable
	}
	switch *state {
	case "active":
		return buildBackendActive
	case "idle", "idle in transaction", "idle in transaction (aborted)":
		return buildBackendIdle
	default:
		return buildBackendUnobservable
	}
}

// describeState renders an observed state for an error message.
func describeState(state *string) string {
	if state == nil {
		return "NULL"
	}
	return strconv.Quote(*state)
}

// StopConcurrentBuild waits for an in-flight observation or cancel signal
// and releases the reserved session back to the executor before its catalog
// verdict. It is the fence that keeps CancelBuild's target honest: the
// executor calls it before the build's own session can return to the pool,
// so no signal that read the build's PID completes after that backend could
// be running someone else's statement.
func (t *Tracker) StopConcurrentBuild() {
	t.takePollGate()
	defer t.releasePollGate()
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
	t.session, t.buildPID, t.source = nil, 0, nil
}

// Progress returns a snapshot and, for an active concurrent index build,
// queries PostgreSQL's progress view by the executor-owned backend PID; for
// a step with a WorkSource it asks the source for the step's counters. On a
// query or source error the snapshot still carries the last-known tracker
// state. The state lock is released before the query, so concurrent pollers
// serialize only against each other (and the four handoff fences), never
// against the executor's own state updates.
func (t *Tracker) Progress(ctx context.Context) (Snapshot, error) {
	t.takePollGate()
	defer t.releasePollGate()
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
	session, pid, source := t.session, t.buildPID, t.source
	t.mu.RUnlock()
	if s.Phase != PhaseRunning {
		return s, nil
	}
	if source != nil {
		return observeEngineWork(ctx, s, source)
	}
	if pid == 0 || session == nil {
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
