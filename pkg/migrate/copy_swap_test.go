package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/checkpoint"
	"github.com/block/pg-sprite/pkg/checksum"
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

	t.Run("a divergence is a failure whose remedy is to abandon the run", func(t *testing.T) {
		divergence := &checksum.DivergenceError{Report: checksum.Report{Chunks: 3, Mismatches: []checksum.Mismatch{{}}}}
		runErr := fmt.Errorf("run billing.orders: %w", divergence)
		v, err := copySwapRunVerdict(st, false, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Equal(t, "billing.orders", v.Table)
		assert.Contains(t, v.Detail, "abandon the run")
		assert.Contains(t, v.Detail, "drop the replication slot", "a live run's hold is its slot")
		assert.NotContains(t, v.Detail, "resumes")
	})

	t.Run("a quiesced divergence names the fence as the hold to release", func(t *testing.T) {
		divergence := &checksum.DivergenceError{Report: checksum.Report{Chunks: 1, Mismatches: []checksum.Mismatch{{}}}}
		v, _ := copySwapRunVerdict(st, true, divergence)
		assert.Contains(t, v.Detail, "take the write fence off the source")
		assert.NotContains(t, v.Detail, "replication slot")
	})

	t.Run("a value the new type will not hold is a failure whose remedy is to abandon the run", func(t *testing.T) {
		// 22001 string_data_right_truncation: a varchar(10) retype over a
		// longer value. The copy meets the same row on every rerun.
		tooLong := &pgconn.PgError{Code: "22001", Message: "value too long for type character varying(10)"}
		runErr := fmt.Errorf("copy chunk: %w", tooLong)
		v, err := copySwapRunVerdict(st, true, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Contains(t, v.Detail, "abandon the run")
		assert.Contains(t, v.Detail, "take the write fence off the source", "a quiesced run's hold is its fence")
		assert.NotContains(t, v.Detail, "resumes")
	})

	t.Run("a constraint the copied rows break is a failure whose remedy is to abandon the run", func(t *testing.T) {
		// 23502 not_null_violation: a re-added NOT NULL column with no
		// default fills from NULL on the first chunk.
		notNull := &pgconn.PgError{Code: "23502", Message: "null value in column violates not-null constraint"}
		v, _ := copySwapRunVerdict(st, false, fmt.Errorf("copy chunk: %w", notNull))
		assert.Contains(t, v.Detail, "abandon the run")
		assert.Contains(t, v.Detail, "drop the replication slot", "a live run's hold is its slot")
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
// case: the change is complete and its done row saved, and the release of
// the lock session failed afterwards. That is an executed change with a
// cleanup fault, not a failed run, so the fold reports the outcome and
// names the fault in the detail; an error with a row short of done is the
// failure it looks like.
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
		assert.Contains(t, v.Detail, "lock session was not released: release table lock: connection closed")
	})

	t.Run("a done row without an error carries no release wording", func(t *testing.T) {
		done := schemachange.Result{Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}}
		v, err := copySwapOutcome(st, false, done, nil)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.NotContains(t, v.Detail, "lock session")
	})

	t.Run("an error with the zero result is a failure", func(t *testing.T) {
		v, err := copySwapOutcome(st, false, schemachange.Result{}, releaseErr)
		assert.ErrorIs(t, err, releaseErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
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
		"the table was quiesced: writes were fenced for the length of the run", quiesced)
}
