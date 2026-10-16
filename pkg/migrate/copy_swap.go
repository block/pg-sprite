package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/dbconn"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

// CopySwapOptions is the policy for a statement the router assigns to the
// copy-and-swap backend. The zero value is not runnable: the route opens
// dedicated sessions of its own — the table lock, the copy workers, the
// replication stream — so it needs the connection the pool was built from,
// not the pool alone.
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
		return verdict.Verdict{}, errors.New("migrate: Options.CopySwap.Connection must name the database the pool was dialed with; the copy-and-swap route opens dedicated sessions from it")
	}
	env := preflight.CopySwapEnvironment{
		LogicalDecoding: !opts.CopySwap.Quiesced,
		FreeDiskBytes:   opts.CopySwap.FreeDiskBytes,
	}
	target, err := preflight.CheckCopySwap(ctx, pool, st.Schema(), st.Table(), env)
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
		KeepOld: opts.CopySwap.KeepOld,
		AfterCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
			logger.Debug("copy-and-swap checkpoint saved", "phase", cp.Phase.String())
			return nil
		},
	})
	if err != nil {
		return verdict.Verdict{}, err
	}
	result, err := runner.Run(ctx, pool, target, st)
	if err != nil {
		return copySwapRunVerdict(st, err)
	}
	return verdict.Verdict{
		Outcome:   verdict.OutcomeExecutedViaCopyAndSwap,
		Statement: st.SQL(),
		Table:     qualified(st),
		Detail:    copySwapDetail(result),
	}, nil
}

// copySwapPreflightVerdict folds a copy-and-swap preflight error into Run's
// contract: a missing privilege and a refused shape or environment are
// refused verdicts; anything else stopped the pipeline before a verdict.
func copySwapPreflightVerdict(st statement.Statement, err error) (verdict.Verdict, error) {
	var privErr *preflight.PrivilegeError
	if errors.As(err, &privErr) {
		return privilegeVerdict(st, privErr, false), nil
	}
	cause := preflight.CopySwapRefusalCauseOf(err)
	if cause == "" {
		return verdict.Verdict{}, err
	}
	r, ok := copySwapPreflightRefusal(cause)
	if !ok {
		return verdict.Verdict{}, fmt.Errorf("%w: copy-and-swap preflight refusal carries unclassified cause %q",
			executor.ErrInvariantViolation, cause)
	}
	return refusedVerdict(st, r, err.Error()), nil
}

// copySwapRunVerdict folds an error out of the Runner into Run's contract.
// A shadow refusal is a refused verdict carrying the shadow's own cause: the
// run never started, or stopped at a fact about the table that a retry
// cannot change. Every other error is a failure with the error alongside;
// the checkpoint row stays at its last save, and the next run over the same
// statement resumes it.
func copySwapRunVerdict(st statement.Statement, err error) (verdict.Verdict, error) {
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
		Detail: "the copy-and-swap run stopped: " + err.Error() +
			"; its checkpoint row remains, and running the same statement again resumes from it",
	}, err
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
// source is gone, and whether the run had to reconcile a lost slot first.
func copySwapDetail(result schemachange.Result) string {
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
	return s
}
