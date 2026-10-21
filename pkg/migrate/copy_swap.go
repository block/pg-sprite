package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
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
// been executed when it is returned.
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
	// The route's dedicated sessions are opened from it.
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
// verdict.OutcomeExecutedViaCopyAndSwap.
func copyAndSwap(ctx context.Context, pool *pgxpool.Pool, st statement.Statement,
	opts Options, logger *slog.Logger) (verdict.Verdict, error) {
	if opts.CopySwap.Connection.URL == "" {
		return verdict.Verdict{}, fmt.Errorf("migrate: %w", ErrCopySwapConnectionRequired)
	}
	target, err := preflight.CheckCopySwap(ctx, pool, st.Schema(), st.Table(), opts.CopySwap.environment())
	if err != nil {
		return copySwapPreflightVerdict(st, err)
	}
	logger.Debug("copy-and-swap preflight passed",
		"decodes_wal", target.DecodesWAL(), "keep_old", opts.CopySwap.KeepOld)

	store, err := checkpoint.NewStore(pool, checkpoint.Options{})
	if err != nil {
		return verdict.Verdict{}, err
	}
	runner, err := schemachange.NewRunner(opts.CopySwap.Connection, store, schemachange.RunOptions{
		KeepOld:     opts.CopySwap.KeepOld,
		ReplaceDone: true,
		Restart:     opts.CopySwap.Restart != "",
		AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
			logger.Debug("copy-and-swap checkpoint saved", "phase", cp.Phase.String())
			return nil
		},
	})
	if err != nil {
		return verdict.Verdict{}, err
	}
	result, err := runner.Run(ctx, pool, target, st)
	return copySwapOutcome(st, opts.CopySwap.Quiesced, result, err)
}

// copySwapOutcome folds what the Runner returned into Run's contract. A
// run whose row reached done is executed, whatever error rides alongside:
// the Runner returns its Result with an error only when the change is
// complete and recorded and the release of its lock session failed, and
// a session left behind is a cleanup fault, not a failed change, so the
// verdict says what the engine tables say and names the session in its
// detail for the operator to find. Any other error is folded by
// copySwapRunVerdict.
func copySwapOutcome(st statement.Statement, quiesced bool, result schemachange.Result, err error) (verdict.Verdict, error) {
	if err != nil && result.Checkpoint.Phase != checkpoint.PhaseDone {
		return copySwapRunVerdict(st, quiesced, err)
	}
	detail := copySwapDetail(result, quiesced)
	if err != nil {
		detail += "; the run's lock session was not released: " + err.Error()
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
// cannot change. Every other error is a failure with the error alongside,
// and the detail says what a rerun does: for a run the Runner abandoned on
// a data fault, nothing remains and the rerun starts from the beginning;
// for every other stop the checkpoint row stays at its last save, and what
// the rerun does with it depends on the error and on which hold the run
// keeps on the source while it is stopped.
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
// A data fault from the server — a value the new type will not hold, a
// constraint the rows break — is one the Runner has already acted on: the
// next run would meet the same rows, so the Runner abandoned this one
// (*schemachange.AbandonedError), and nothing remains to resume or to
// start over; the detail says what was discarded and that the corrected
// statement runs from the beginning. Two kinds of stop a rerun cannot
// clear remain for the operator: a divergence, where the row stands in
// verifying and the next run verifies the same copy against the same
// source and diverges again; and a data fault whose abandon did not finish,
// where the row still stands as far as the discard got. Both have to be
// started over, and the detail says how: the restart acknowledgement
// (CopySwapOptions.Restart, `--restart SCHEMA.TABLE` on the CLI) discards
// what the stopped run left and runs the change from the start. Every
// other stop is resumed by running the statement again. In each case the
// detail names the hold the run keeps or kept on the source, since that is
// what the operator is paying for in the meantime: the write fence of a
// quiesced run, or the replication slot of a live run, which pins WAL on
// the server until it is dropped.
func copySwapStoppedRemedy(quiesced bool, err error) string {
	hold := "its replication slot stays and pins WAL on the server"
	release := "drops the replication slot"
	released := "its replication slot is dropped"
	if quiesced {
		hold = "the source stays fenced against writes"
		release = "takes the write fence off the source"
		released = "the write fence is off the source"
	}
	var abandoned *schemachange.AbandonedError
	if errors.As(err, &abandoned) {
		return "the server rejected the rows as the statement would shape them, so the run was abandoned: the shadow is dropped, " + released + ", and the checkpoint row is deleted; correct the data or the statement, then run it again, which starts from the beginning"
	}
	startOver := "correct the data or the statement, then start the run over with the restart acknowledgement (--restart SCHEMA.TABLE), which drops the shadow, " + release + ", deletes the checkpoint row, and runs the change from the start"
	var divergence *checksum.DivergenceError
	if errors.As(err, &divergence) {
		return "the shadow did not match the source, so running the same statement again would verify the same copy and stop here again; " + startOver
	}
	if schemachange.IsDataFault(err) {
		return "the server rejected the rows as the statement would shape them and the run could not be abandoned, so running the same statement again would copy the same rows and stop here again; " + startOver
	}
	return "any checkpoint row it saved remains and " + hold + " until the same statement is run again, which resumes from the row"
}

// copySwapFailureCode names the executor outcome code for an error out of
// the Runner. The Runner's errors are not the executor's, so the
// conditions the codes already name are matched on their own sentinels and
// predicates — a data fault whether or not its abandon finished, since the
// code names what the operator corrects, not what was left behind; the
// rest is an execution failure.
func copySwapFailureCode(err error) executor.Code {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return executor.CodeCancelledByCaller
	case errors.Is(err, schemachange.ErrInvariantViolation):
		return executor.CodeInvariantViolation
	case schemachange.IsDataFault(err):
		return executor.CodeDataFault
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
