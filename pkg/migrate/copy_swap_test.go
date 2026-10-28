package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
	"github.com/block/pg-sprite/pkg/copier"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/verdict"
)

func parseCopySwapStatement(t *testing.T) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne("ALTER TABLE billing.orders ALTER COLUMN id TYPE bigint")
	require.NoError(t, err)
	return st
}

// The route cannot open its dedicated sessions from a pool alone, so a
// caller that routes a statement to copy-and-swap without naming the
// connection stops before any database work, with the zero verdict that
// means "nothing was executed".
func TestCopyAndSwapRequiresTheConnection(t *testing.T) {
	v, err := copyAndSwap(t.Context(), nil, parseCopySwapStatement(t), Options{}, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, ErrCopySwapConnectionRequired)
	assert.Zero(t, v)
}

// A preflight refusal is a refused verdict under the one reason, carrying
// the preflight's own cause and the class the registry gives it; a missing
// privilege is the same insufficient-privileges refusal the native path
// produces; anything else stopped the pipeline before a verdict.
func TestCopySwapPreflightVerdict(t *testing.T) {
	st := parseCopySwapStatement(t)

	t.Run("a refused shape is a capability boundary with its cause", func(t *testing.T) {
		shapeErr := &preflight.UnsupportedCopySwapShapeError{Cause: preflight.CopySwapCauseUnlogged, Detail: "the table is UNLOGGED"}
		v, err := copySwapPreflightVerdict(st, fmt.Errorf("preflight: %w", shapeErr))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
		assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
		assert.Equal(t, verdict.Cause(preflight.CopySwapCauseUnlogged), v.Cause)
		assert.Equal(t, "billing.orders", v.Table)
		assert.Contains(t, v.Detail, "UNLOGGED")
	})

	t.Run("a refused environment is environmental with its cause", func(t *testing.T) {
		envErr := &preflight.CopySwapEnvironmentError{Cause: preflight.CopySwapCauseDiskHeadroom, Detail: "0 bytes free"}
		v, err := copySwapPreflightVerdict(st, envErr)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ClassEnvironmental, v.Class)
		assert.Equal(t, verdict.Cause(preflight.CopySwapCauseDiskHeadroom), v.Cause)
	})

	t.Run("a missing privilege is the native path's privilege refusal", func(t *testing.T) {
		privErr := &preflight.PrivilegeError{Tier: preflight.TierCopyAndSwap, Check: "rolreplication", Grant: `ALTER ROLE "app" REPLICATION`}
		v, err := copySwapPreflightVerdict(st, privErr)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonInsufficientPrivileges, v.Reason)
		assert.Contains(t, v.Detail, privErr.Grant)
	})

	t.Run("any other error stops before a verdict", func(t *testing.T) {
		v, err := copySwapPreflightVerdict(st, errors.New("connection reset"))
		require.Error(t, err)
		assert.Zero(t, v)
	})
}

// An error out of the Runner is either a shadow refusal — a refused verdict
// carrying the shadow's cause under the class the cause declares — or a
// failure returned with its error, coded so automation can tell a stopped
// run from a cancelled one from a breach of the engine's own contract.
func TestCopySwapRunVerdict(t *testing.T) {
	st := parseCopySwapStatement(t)

	t.Run("a shadow refusal is refused with its cause under the class it declares", func(t *testing.T) {
		refusal := &schemachange.RefusalError{Cause: schemachange.CauseNameTaken, Detail: "a relation wears the shadow's name"}
		v, err := copySwapRunVerdict(st, false, fmt.Errorf("run billing.orders: %w", refusal))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
		assert.Equal(t, verdict.ClassEnvironmental, v.Class)
		assert.Equal(t, verdict.Cause(schemachange.CauseNameTaken), v.Cause)
		assert.Empty(t, v.Code, "a refusal is not a failure")
	})

	t.Run("a divergence is a failure whose remedy is to start the run over", func(t *testing.T) {
		divergence := &checksum.DivergenceError{Report: checksum.Report{Chunks: 3, Mismatches: []checksum.Mismatch{{}}}}
		runErr := fmt.Errorf("run billing.orders: %w", divergence)
		v, err := copySwapRunVerdict(st, false, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Equal(t, "billing.orders", v.Table)
		assert.Contains(t, v.Detail, "start the run over")
		assert.Contains(t, v.Detail, "drops the replication slot", "a live run's hold is its slot")
		assert.NotContains(t, v.Detail, "resumes")
		assert.False(t, v.WritesBlocked, "a live run never took the table offline for writes")
	})

	t.Run("a quiesced divergence names the fence as the hold to release", func(t *testing.T) {
		divergence := &checksum.DivergenceError{Report: checksum.Report{Chunks: 1, Mismatches: []checksum.Mismatch{{}}}}
		v, _ := copySwapRunVerdict(st, true, divergence)
		assert.Contains(t, v.Detail, "takes the write fence off the source")
		assert.NotContains(t, v.Detail, "replication slot")
		assert.True(t, v.WritesBlocked, "the failed verdict says the run took the table offline for writes")
	})

	t.Run("a value the new type will not hold is an abandoned run coded as a data fault", func(t *testing.T) {
		// 22001 string_data_right_truncation: a varchar(10) retype over a
		// longer value. The copy would meet the same row on every rerun,
		// so the Runner discarded the run before returning; the detail
		// says what is gone and that the corrected statement starts from
		// the beginning — there is no row to resume and nothing for
		// --restart to discard.
		tooLong := &pgconn.PgError{Code: "22001", Message: "value too long for type character varying(10)"}
		runErr := &schemachange.AbandonedError{Schema: "billing", Table: "orders", Fault: fmt.Errorf("copy chunk: %w", tooLong)}
		v, err := copySwapRunVerdict(st, true, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, "the run was abandoned")
		assert.Contains(t, v.Detail, "the write fence is off the source", "a quiesced run's hold was its fence")
		assert.Contains(t, v.Detail, "starts from the beginning")
		assert.NotContains(t, v.Detail, "--restart")
		assert.NotContains(t, v.Detail, "resumes")
	})

	t.Run("a constraint the copied rows break is an abandoned run that names the slot as released", func(t *testing.T) {
		// 23502 not_null_violation: a re-added NOT NULL column with no
		// default fills from NULL on the first chunk.
		notNull := &pgconn.PgError{Code: "23502", Message: "null value in column violates not-null constraint"}
		runErr := &schemachange.AbandonedError{Schema: "billing", Table: "orders", Fault: fmt.Errorf("copy chunk: %w", notNull)}
		v, _ := copySwapRunVerdict(st, false, runErr)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, "the run was abandoned")
		assert.Contains(t, v.Detail, "its replication slot is dropped", "a live run's hold was its slot")
		assert.NotContains(t, v.Detail, "--restart")
	})

	t.Run("a data fault whose abandon did not finish is coded as a data fault with the restart remedy", func(t *testing.T) {
		// The Runner joins the discard's failure behind the fault; the row
		// still stands as far as the discard got, so the operator finishes
		// the discard with the restart acknowledgement. The code still
		// names the fault, since that is what the operator corrects.
		tooLong := &pgconn.PgError{Code: "22001", Message: "value too long for type character varying(10)"}
		runErr := errors.Join(fmt.Errorf("copy chunk: %w", tooLong), errors.New("abandon run on billing.orders: drop shadow: connection reset"))
		v, err := copySwapRunVerdict(st, true, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, "could not be abandoned")
		assert.Contains(t, v.Detail, "start the run over")
		assert.Contains(t, v.Detail, "takes the write fence off the source", "a quiesced run's hold is its fence")
		assert.NotContains(t, v.Detail, "resumes")
	})

	t.Run("a server fault outside the data classes is resumed", func(t *testing.T) {
		// 57P01 admin_shutdown: the server went away under the copy; the
		// rows are not at fault and the next run carries on from the row.
		shutdown := &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}
		v, _ := copySwapRunVerdict(st, false, fmt.Errorf("copy chunk: %w", shutdown))
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.NotContains(t, v.Detail, "abandon")
	})

	t.Run("a stop a rerun clears says the slot stays until it resumes", func(t *testing.T) {
		v, err := copySwapRunVerdict(st, false, fmt.Errorf("copy: %w", context.Canceled))
		require.Error(t, err)
		assert.Contains(t, v.Detail, "replication slot stays")
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.NotContains(t, v.Detail, "abandon")
	})

	t.Run("a quiesced stop a rerun clears says the fence stays until it resumes", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, true, fmt.Errorf("copy: %w", context.Canceled))
		assert.Contains(t, v.Detail, "fenced against writes")
		assert.Contains(t, v.Detail, "resumes from the row")
	})

	t.Run("a cancelled run is coded as cancelled by the caller", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, false, fmt.Errorf("copy: %w", context.Canceled))
		assert.Equal(t, string(executor.CodeCancelledByCaller), v.Code)
	})

	t.Run("a breach of the shadow's invariants is coded as an invariant violation", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, false, fmt.Errorf("gate: %w", schemachange.ErrInvariantViolation))
		assert.Equal(t, string(executor.CodeInvariantViolation), v.Code)
	})

	t.Run("a rolled-back swap is an execution failure", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, false, schemachange.ErrCutoverRolledBack)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
	})
}

// The Runner returns its Result together with an error in exactly one
// case: the change is complete and its done row saved, and what came
// after failed — the host's hook handed the done row, or the release of
// the lock session. That is an executed change with a fault after it, not
// a failed run, so the fold reports the outcome and names the fault in
// the detail; an error with a row short of done is the failure it looks
// like.
func TestCopySwapOutcomeTreatsADoneRowAsExecuted(t *testing.T) {
	st := parseCopySwapStatement(t)
	releaseErr := errors.New("release table lock: connection closed")

	t.Run("a done row with a release error is executed", func(t *testing.T) {
		done := schemachange.Result{
			Checkpoint:      checkpoint.Checkpoint{Phase: checkpoint.PhaseDone},
			Verification:    checksum.Report{Rows: 7, Chunks: 1},
			OldTableDropped: true,
		}
		v, err := copySwapOutcome(st, false, done, releaseErr)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.Empty(t, v.Code)
		assert.Contains(t, v.Detail, "verified 7 rows in 1 chunks")
		assert.Contains(t, v.Detail, "the run then returned: release table lock: connection closed")
	})

	t.Run("a done row without an error carries no release wording", func(t *testing.T) {
		done := schemachange.Result{Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}}
		v, err := copySwapOutcome(st, false, done, nil)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.NotContains(t, v.Detail, "the run then returned")
		assert.False(t, v.WritesBlocked, "a live run's verdict does not claim the table was offline")
	})

	t.Run("a block-writes done row is marked writes-blocked", func(t *testing.T) {
		done := schemachange.Result{Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}}
		v, err := copySwapOutcome(st, true, done, nil)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.True(t, v.WritesBlocked, "the executed verdict says the table was offline for writes while the run held it")
	})

	t.Run("an error with the zero result is a failure", func(t *testing.T) {
		v, err := copySwapOutcome(st, false, schemachange.Result{}, releaseErr)
		assert.ErrorIs(t, err, releaseErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
	})
}

// The host's sizing, interval, and progress hook reach the Runner as given,
// and the route fixes ReplaceDone on. The Runner's hook is the route's own:
// it tolerates a host that set none, hands the host's the row as saved,
// and returns what the host's returned, so a host's error stops the run as
// the Runner's contract says.
func TestCopySwapRunOptionsCarryTheHostsPolicy(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	row := checkpoint.Checkpoint{Schema: "billing", Table: "orders", Phase: checkpoint.PhaseVerifying}

	t.Run("passes sizing and choices through", func(t *testing.T) {
		got := CopySwapOptions{
			KeepOld:            true,
			Copier:             copier.Options{Workers: 3},
			CheckpointInterval: 2 * time.Second,
		}.runOptions(logger)
		assert.True(t, got.KeepOld)
		assert.True(t, got.ReplaceDone)
		assert.Equal(t, 3, got.Copier.Workers)
		assert.Equal(t, 2*time.Second, got.CheckpointInterval)
	})

	t.Run("a run without a hook saves without complaint", func(t *testing.T) {
		got := CopySwapOptions{}.runOptions(logger)
		require.NotNil(t, got.AfterCheckpoint)
		assert.NoError(t, got.AfterCheckpoint(t.Context(), row))
	})

	t.Run("the hook sees the row as saved and its error stops the run", func(t *testing.T) {
		hookErr := errors.New("host is draining")
		var seen checkpoint.Checkpoint
		got := CopySwapOptions{OnCheckpoint: func(_ context.Context, cp checkpoint.Checkpoint) error {
			seen = cp
			return hookErr
		}}.runOptions(logger)
		assert.ErrorIs(t, got.AfterCheckpoint(t.Context(), row), hookErr)
		assert.Equal(t, row, seen)
	})
}

// The detail names what the run proved; the kept-old wording and the
// dropped wording are never both present, and the quiesced wording appears
// exactly when the run fenced the source's writes, since the outcome's
// exit code otherwise reads as online-safe.
func TestCopySwapDetail(t *testing.T) {
	dropped := copySwapDetail(schemachange.Result{
		Verification:    checksum.Report{Rows: 2500, Chunks: 3},
		OldTableDropped: true,
	}, false)
	assert.Equal(t, "verified 2500 rows in 3 chunks; the retained source is dropped", dropped)

	kept := copySwapDetail(schemachange.Result{
		Verification: checksum.Report{Rows: 10, Chunks: 1, Settled: 1},
		Reconciled:   true,
	}, false)
	assert.Equal(t, "verified 10 rows in 1 chunks (1 settled against the stream); "+
		"the retained source is kept under its _old name; "+
		"the run reconciled a lost replication slot before catching up", kept)

	quiesced := copySwapDetail(schemachange.Result{
		Verification:    checksum.Report{Rows: 4, Chunks: 1},
		OldTableDropped: true,
	}, true)
	assert.Equal(t, "verified 4 rows in 1 chunks; the retained source is dropped; "+
		"writes to the table were blocked for the length of the run", quiesced)
}
