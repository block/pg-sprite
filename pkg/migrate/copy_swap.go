package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// ErrCopySwapConnectionRequired is returned by [Run] and [RunDesired] when
// a statement routes to the copy-and-swap backend and
// Options.CopySwap.Connection is unset. The route opens dedicated sessions
// of its own and needs the connection the pool was dialed with, not the
// pool alone; a library caller that builds Options by hand wires it, the
// CLI always does. It is a sentinel so an embedder can tell the missing
// wiring apart from an operational failure with [errors.Is]; nothing has
// been executed when it is returned. A connection that is set but reaches
// another database is the other wiring error,
// [CopySwapConnectionMismatchError].
var ErrCopySwapConnectionRequired = errors.New(
	"Options.CopySwap.Connection must name the database the pool was dialed with; the copy-and-swap route opens dedicated sessions from it")

// CopySwapOptions is the policy for a statement the router assigns to the
// copy-and-swap backend. The zero value is not runnable: the route opens
// dedicated sessions of its own — the table lock, the copy workers, the
// replication stream — so it needs the connection the pool was built from,
// not the pool alone.
//
// A table that already carries the done row of an earlier copy-and-swap is
// run again without ceremony: the route replaces the row inside its own
// lock session (schemachange.RunOptions.ReplaceDone), so a second type
// change on a table — the next planned statement, or next month's — needs
// no step between the two runs. The one done row the route will not
// replace is one whose retained source still stands under its _old name
// (KeepOld): that is an open rollback window, and the run is refused until
// the operator ends it.
type CopySwapOptions struct {
	// Connection is the database the pool handed to Run was dialed with.
	// The route's dedicated sessions are opened from it. Before the
	// preflight the route proves the two reach one database on one
	// cluster, and stops with a CopySwapConnectionMismatchError when they
	// do not.
	Connection dbconn.Config

	// FreeDiskBytes is the free space the operator reports on the volume
	// the table lives on; the preflight refuses the run when the shadow and
	// its indexes would not fit. Zero means no space, and refuses.
	FreeDiskBytes int64

	// KeepOld leaves the retained source under its derived _old name after
	// the swap, for a rollback window the operator ends by hand. False
	// drops it before the run reports done.
	KeepOld bool

	// Quiesced declares that nothing writes the table while the run lasts.
	// The run then takes the write fence instead of a replication slot and
	// needs no logical decoding from the server; a write that reaches the
	// fence fails. False runs the live path: slot, stream, and catch-up.
	Quiesced bool

	// Restart, when set, is the operator's acknowledgement that the run on
	// the table is to start over: whatever an earlier run left — its
	// checkpoint row, its shadow, its write fence or replication slot —
	// is discarded under the run's own lock session before a fresh shadow
	// is built (schemachange.RunOptions.Restart). It must name the
	// resolved target table as SCHEMA.TABLE, the same way Options.Force
	// does, so the acknowledgement cannot be left on a script by accident.
	// A leftover whose swap already committed is not discarded: the
	// change is past the point of starting over, and the run is refused
	// so it can be finished without the acknowledgement. Empty resumes a
	// leftover, as every run does.
	Restart string

	// Copier sizes the copy: its workers, the chunk sizes and the chunk
	// time they are throttled toward, the budgets of each chunk
	// transaction, and the progress tracker the copier reports to while it
	// runs. Zero values take the copier's defaults. A value the copier
	// would refuse is a wiring error refused before the preflight, like a
	// negative CheckpointInterval. The workers set the pool size the route
	// admits: CopySwapMinConns(Copier).
	//
	// Copier.Tracker is the one source of rows copied; OnCheckpoint
	// carries a key position, not a count. The host builds the tracker and
	// calls its Start before Run, since Start and StartStep drop the
	// tracker's work source and so, called while the copy runs, stop the
	// counters. The copier is the work source from the copy's first chunk
	// until just before the copy returns: the tracker reports no work
	// before the copy, after it, or through the verification pass and a
	// live run's catch-up, whose phases reach the host through
	// OnCheckpoint alone; the verdict's detail reports the rows the
	// verification pass covered. Rows copied counts this run's rows only,
	// and rows total is the source's catalog estimate, zero for a table
	// never analyzed, so a host treats a zero total as unknown and a ratio
	// over one as possible. A tracker serves one run: two runs handed one
	// tracker each make it their source, and the first to finish drops the
	// source the other still reports to.
	Copier copier.Options

	// CheckpointInterval is how often a copy in progress saves its landed
	// watermark to the checkpoint row, which is how far a run that stops
	// mid-copy resumes from, and how often OnCheckpoint hears from the
	// copy. Zero means the Runner's default; a negative value is refused
	// before anything is read.
	CheckpointInterval time.Duration

	// OnCheckpoint, when set, is the run's progress signal: it is called
	// after every successful save of the checkpoint row with the row as
	// saved — the phase it entered, the watermark the copy has landed, the
	// position a live run has applied through — so a host can record where
	// a run that may last hours stands. Two saves are not shown: a live
	// run that reconciles a lost slot writes its reconciling marker and
	// the resumed row straight to the store, and the hook is handed the
	// row the reconcile leaves once. The row is the row as written, so its
	// UpdatedAt is zero; a host that wants a save time uses its own clock.
	// Its ShadowProof bytes are shared with the run, so a hook that keeps
	// the row treats it as read-only. The slot, publication, and applied
	// position it carries are PostgreSQL's; an adapter that forwards the
	// row to an engine-neutral record maps them into generic metadata.
	//
	// The hook runs on the run's own goroutines and holds the run up while
	// it runs: the phase saves call it on the goroutine that called Run,
	// with the table's lock session held and, for a quiesced run, the
	// source fenced against writes; the periodic watermark saves call it
	// on the copy's saving goroutine, every CheckpointInterval rather than
	// as the copy lands rows, and a slow call there delays the next save.
	// It must be safe to call from either goroutine and should return
	// promptly. An error it returns stops the run there, as a failed
	// verdict with the row as that save left it, and the next run resumes
	// from the row — except at the done save, after which there is nothing
	// left to stop: the verdict is executed and its detail names the
	// error.
	OnCheckpoint func(ctx context.Context, cp checkpoint.Checkpoint) error
}

// runOptions is the Runner's policy for the route: the operator's two
// choices, the host's sizing and progress hook, and the one setting the
// route fixes — a done row from an earlier run is replaced, since a second
// change on the table is the next statement, not a rollback window.
func (o CopySwapOptions) runOptions(logger *slog.Logger) schemachange.RunOptions {
	return schemachange.RunOptions{
		Copier:             o.Copier,
		KeepOld:            o.KeepOld,
		ReplaceDone:        true,
		Restart:            o.Restart != "",
		CheckpointInterval: o.CheckpointInterval,
		AfterCheckpoint: func(ctx context.Context, cp checkpoint.Checkpoint) error {
			logger.Debug("copy-and-swap checkpoint saved",
				"schema", cp.Schema, "table", cp.Table, "phase", cp.Phase.String())
			if o.OnCheckpoint == nil {
				return nil
			}
			return o.OnCheckpoint(ctx, cp)
		},
	}
}

// environment is what the operator declared about the run's surroundings,
// in the form the preflight reads: a quiesced run decodes no WAL, so it
// needs none of the server's logical-decoding facilities.
func (o CopySwapOptions) environment() preflight.CopySwapEnvironment {
	return preflight.CopySwapEnvironment{
		LogicalDecoding: !o.Quiesced,
		FreeDiskBytes:   o.FreeDiskBytes,
	}
}

// copyAndSwap runs a statement the router assigned to the copy-and-swap
// backend through the schemachange.Runner and folds the result into one
// verdict. The verdict-and-error contract is Run's: a preflight or shadow
// refusal is a refused verdict under verdict.ReasonCopyAndSwapRefused with
// the route's own cause and a nil error; a run that stops is a failed
// verdict together with the error, and the checkpoint row it stopped at
// remains so the next run resumes it; success is
// verdict.OutcomeExecutedViaCopyAndSwap. An option set the Runner refuses
// is a caller wiring error like the missing connection: a plain error, no
// verdict, before the preflight reads anything. A pool smaller than
// CopySwapMinConns is a failed verdict with the pool-too-small code and an
// error wrapping executor.ErrPoolTooSmall, also before the preflight, so
// nothing is created.
func copyAndSwap(ctx context.Context, pool *pgxpool.Pool, st statement.Statement,
	opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	if opts.CopySwap.Connection.URL == "" {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", ErrCopySwapConnectionRequired)
	}
	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return verdict.Verdict{}, err
	}
	runner, err := schemachange.NewRunner(opts.CopySwap.Connection, store, opts.CopySwap.runOptions(logger))
	if err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: copy-and-swap options: %w", err)
	}
	if err := checkCopySwapConnection(ctx, pool, opts.CopySwap.Connection); err != nil {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", err)
	}
	if err := checkCopySwapPool(pool, opts.CopySwap); err != nil {
		return copySwapPoolVerdict(st, err), fmt.Errorf("migrate: %w", err)
	}
	target, err := preflight.CheckCopySwap(ctx, pool, st.Schema(), st.Table(), opts.CopySwap.environment())
	if err != nil {
		return copySwapPreflightVerdict(st, err)
	}
	logger.Debug("copy-and-swap preflight passed",
		"decodes_wal", target.DecodesWAL(), "keep_old", opts.CopySwap.KeepOld)

	result, err := runner.Run(ctx, pool, target, st)
	return copySwapOutcome(st, opts.CopySwap.Quiesced, result, err)
}

// copySwapOutcome folds what the Runner returned into Run's contract. A
// run whose row reached done is executed, whatever error rides alongside:
// the Runner returns its Result with an error only when the change is
// complete and recorded and what came after failed — the host's
// OnCheckpoint handed the done row, or the release of the lock session —
// and neither is a failed change, so the verdict says what the engine
// tables say and names the error in its detail for the operator to find.
// Any other error is folded by copySwapRunVerdict.
func copySwapOutcome(st statement.Statement, quiesced bool, result schemachange.Result, err error) (verdict.Verdict, error) {
	if err != nil && result.Checkpoint.Phase != checkpoint.PhaseDone {
		return copySwapRunVerdict(st, quiesced, err)
	}
	detail := copySwapDetail(result, quiesced)
	if err != nil {
		detail += "; the change is complete and its done row saved, but the run then returned: " + err.Error()
	}
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecutedViaCopyAndSwap,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    detail,
	}, nil
}

// copySwapPreflightVerdict folds a copy-and-swap preflight error into Run's
// contract: a missing privilege and a refused shape or environment are
// refused verdicts; anything else stopped the pipeline before a verdict.
func copySwapPreflightVerdict(st statement.Statement, err error) (verdict.Verdict, error) {
	r, detail, err := classifyCopySwapPreflight(err)
	if err != nil {
		return verdict.Verdict{}, err
	}
	return refusedVerdict(st, r, detail), nil
}

// classifyCopySwapPreflight sorts an error out of preflight.CheckCopySwap
// into the refusal it stands for — with the prose behind it as detail — or
// returns the error when it is not a refusal: the preflight could not
// finish, and nothing has been decided about the table. A missing privilege
// is the same insufficient-privileges refusal the native path produces; a
// refused shape or environment carries the preflight's own cause under the
// class the cause declares. Both front doors fold the same classification,
// one into a verdict and one into a whole-plan refusal.
func classifyCopySwapPreflight(err error) (verdict.Refusal, string, error) {
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		return insufficientPrivilegesRefusal(), privErr.Error(), nil
	}
	cause := preflight.CopySwapRefusalCauseOf(err)
	if cause == "" {
		return verdict.Refusal{}, "", err
	}
	r, ok := copySwapPreflightRefusal(cause)
	if !ok {
		return verdict.Refusal{}, "", fmt.Errorf("%w: copy-and-swap preflight refusal carries unclassified cause %q",
			executor.ErrInvariantViolation, cause)
	}
	return r, err.Error(), nil
}

// copySwapRunVerdict folds an error out of the Runner into Run's contract.
// A shadow refusal is a refused verdict carrying the shadow's own cause: the
// run never started, or stopped at a fact about the table that a retry
// cannot change. Every other error is a failure with the error alongside;
// the checkpoint row stays at its last save, and the detail says what a
// rerun does with it, which depends on the error and on which hold the
// run keeps on the source while it is stopped.
func copySwapRunVerdict(st statement.Statement, quiesced bool, err error) (verdict.Verdict, error) {
	if cause := schemachange.RefusalCauseOf(err); cause != "" {
		r, ok := shadowRefusal(cause)
		if !ok {
			return verdict.Verdict{}, fmt.Errorf("%w: shadow refusal carries unclassified cause %q",
				executor.ErrInvariantViolation, cause)
		}
		return refusedVerdict(st, r, err.Error()), nil
	}
	return verdict.Verdict{
		Outcome:   verdict.OutcomeFailed,
		Code:      string(copySwapFailureCode(err)),
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    "the copy-and-swap run stopped: " + err.Error() + "; " + copySwapStoppedRemedy(quiesced, err),
	}, err
}

// copySwapStoppedRemedy says what the operator does with a stopped run.
// Two kinds of stop a rerun cannot clear: a divergence, where the row
// stands in verifying and the next run verifies the same copy against the
// same source and diverges again; and a data fault from the server — a
// value the new type will not hold, a constraint the copied rows break —
// where the next run copies the same rows and meets the same fault. Both
// have to be started over, and the detail says how: the restart
// acknowledgement (CopySwapOptions.Restart, `--restart SCHEMA.TABLE` on
// the CLI) discards what the stopped run left and runs the change from
// the start. Every other stop is resumed by running the statement again.
// Either way the detail names the hold the stopped run keeps on the
// source, since that is what the operator is paying for in the meantime:
// the write fence of a quiesced run, or the replication slot of a live
// run, which pins WAL on the server until it is dropped.
func copySwapStoppedRemedy(quiesced bool, err error) string {
	hold := "its replication slot stays and pins WAL on the server"
	release := "drops the replication slot"
	if quiesced {
		hold = "the source stays fenced against writes"
		release = "takes the write fence off the source"
	}
	startOver := "correct the data or the statement, then start the run over with the restart acknowledgement (--restart SCHEMA.TABLE), which drops the shadow, " + release + ", deletes the checkpoint row, and runs the change from the start"
	var divergence *checksum.DivergenceError
	if errors.As(err, &divergence) {
		return "the shadow did not match the source, so running the same statement again would verify the same copy and stop here again; " + startOver
	}
	if isDataFault(err) {
		return "the server rejected the rows as the statement would shape them, so running the same statement again would copy the same rows and stop here again; " + startOver
	}
	return "any checkpoint row it saved remains and " + hold + " until the same statement is run again, which resumes from the row"
}

// SQLSTATE classes of the faults the data itself raises when it is copied
// into the shadow's shape: a value the new type will not hold (22, data
// exception: a string too long, a number out of range, a cast that fails)
// and a constraint the copied rows break (23, integrity constraint
// violation: NOT NULL, a check, a unique key). The class is the first two
// characters of the five-character code.
const (
	sqlstateClassDataException       = "22"
	sqlstateClassIntegrityConstraint = "23"
)

// isDataFault reports whether err is the server rejecting the data under
// the statement's new shape — a fault a rerun meets again on the same rows,
// as opposed to a lost connection, a cancelled context, or a lock the run
// could take next time.
func isDataFault(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case sqlstateClassDataException, sqlstateClassIntegrityConstraint:
		return true
	default:
		return false
	}
}

// copySwapFailureCode names the executor outcome code for an error out of
// the Runner. The Runner's errors are not the executor's, so the two
// conditions the codes already name are matched on their own sentinels; the
// rest is an execution failure.
func copySwapFailureCode(err error) executor.Code {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return executor.CodeCancelledByCaller
	case errors.Is(err, schemachange.ErrInvariantViolation):
		return executor.CodeInvariantViolation
	default:
		return executor.OutcomeCode(err)
	}
}

// refusedVerdict is the refused verdict for st under r, with detail as the
// prose behind the cause.
func refusedVerdict(st statement.Statement, r verdict.Refusal, detail string) verdict.Verdict {
	return verdict.Verdict{
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    detail,
	}.WithRefusal(r)
}

// copySwapDetail renders what the run proved, for the human reading the
// verdict: how much the verification pass covered, whether the retained
// source is gone, whether the run had to reconcile a lost slot first, and
// — since the outcome's exit code stands for online-safe execution — that
// a quiesced run fenced the source's writes for its length.
func copySwapDetail(result schemachange.Result, quiesced bool) string {
	s := fmt.Sprintf("verified %d rows in %d chunks", result.Verification.Rows, result.Verification.Chunks)
	if result.Verification.Settled > 0 {
		s += fmt.Sprintf(" (%d settled against the stream)", result.Verification.Settled)
	}
	if result.OldTableDropped {
		s += "; the retained source is dropped"
	} else {
		s += "; the retained source is kept under its _old name"
	}
	if result.Reconciled {
		s += "; the run reconciled a lost replication slot before catching up"
	}
	if quiesced {
		s += "; the table was quiesced: writes were fenced for the length of the run"
	}
	return s
}
