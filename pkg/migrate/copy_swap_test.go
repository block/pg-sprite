package migrate

import (
	"context"
	"encoding/json"
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
	"github.com/block/pg-sprite/pkg/decode"
	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/preflight"
	"github.com/block/pg-sprite/pkg/schemachange"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

func parseCopySwapStatement(t *testing.T) statement.Statement {
	t.Helper()
	st, err := statement.ParseOne("ALTER TABLE billing.orders ALTER COLUMN id TYPE bigint")
	require.NoError(t, err)
	return st
}

// The route is in preview and closed until the policy opens it: a
// statement that routes to copy-and-swap under the zero CopySwapOptions is
// refused as a capability boundary, with its own cause, before the route's
// connection is even looked at — the connection here is unset and the pool
// nil, and neither is reached. The refusal is a verdict, not an error: the
// caller wired nothing wrong, the route is simply not open to it.
func TestCopyAndSwapIsClosedUntilEnabled(t *testing.T) {
	st := parseCopySwapStatement(t)
	v, err := copyAndSwap(t.Context(), nil, st, Options{}, 1, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
	assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
	assert.Equal(t, verdict.CauseCopyAndSwapNotEnabled, v.Cause)
	assert.Equal(t, verdict.ClassCapabilityBoundary, v.Class)
	assert.Equal(t, "billing.orders", v.Table)
	assert.Equal(t, st.SQL(), v.Statement)
	assert.Empty(t, v.Code, "a refusal is not a failure")
	assert.Contains(t, v.Detail, "nothing was executed")
}

// The route cannot open its dedicated sessions from a pool alone, so a
// caller that opens the route for a statement without naming the
// connection stops before any database work, with the zero verdict that
// means "nothing was executed".
func TestCopyAndSwapRequiresTheConnection(t *testing.T) {
	opts := Options{CopySwap: CopySwapOptions{Enabled: true}}
	v, err := copyAndSwap(t.Context(), nil, parseCopySwapStatement(t), opts, 1, slog.New(slog.DiscardHandler))
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

	t.Run("a keyless table carries the guidance to add an integer key", func(t *testing.T) {
		shapeErr := &preflight.UnsupportedCopySwapShapeError{
			Cause:        preflight.CopySwapCausePKUnsupported,
			Detail:       "the primary key has 0 columns; exactly one is required",
			NoPrimaryKey: true,
		}
		v, err := copySwapPreflightVerdict(st, fmt.Errorf("preflight: %w", shapeErr))
		require.NoError(t, err)
		assert.Equal(t, verdict.Cause(preflight.CopySwapCausePKUnsupported), v.Cause)
		assert.Equal(t, suggest.GuidanceAddIntegerPrimaryKey, v.Guidance)
		assert.Contains(t, v.Detail, "docs/primary-keys.md#add-a-primary-key-online")
	})

	t.Run("a uuid-keyed table carries no guidance", func(t *testing.T) {
		// A table has one primary key, and no online route replaces it,
		// so there is no add-a-key step to point at.
		shapeErr := &preflight.UnsupportedCopySwapShapeError{
			Cause:  preflight.CopySwapCausePKUnsupported,
			Detail: "primary-key column id has type uuid; smallint, integer, or bigint is required",
		}
		v, err := copySwapPreflightVerdict(st, shapeErr)
		require.NoError(t, err)
		assert.Equal(t, verdict.Cause(preflight.CopySwapCausePKUnsupported), v.Cause)
		assert.Empty(t, v.Guidance)
		assert.Equal(t, shapeErr.Error(), v.Detail)
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
		v, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("run billing.orders: %w", refusal))
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
		v, err := copySwapRunVerdict(st, holdSlot, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Equal(t, "billing.orders", v.Table)
		assert.Contains(t, v.Detail, "start the run over")
		assert.Contains(t, v.Detail, "drops the replication slot", "a live run's hold is its slot")
		assert.NotContains(t, v.Detail, "resumes")
		assert.False(t, v.WritesBlocked, "a live run never took the table offline for writes")
		assert.False(t, v.Abandoned, "a divergence leaves the run standing for --restart")
	})

	t.Run("a quiesced divergence names the fence as the hold to release", func(t *testing.T) {
		divergence := &checksum.DivergenceError{Report: checksum.Report{Chunks: 1, Mismatches: []checksum.Mismatch{{}}}}
		v, _ := copySwapRunVerdict(st, holdFence, divergence)
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
		v, err := copySwapRunVerdict(st, holdFence, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, "the run was abandoned")
		assert.Contains(t, v.Detail, "the write fence is off the source", "a quiesced run's hold was its fence")
		assert.Contains(t, v.Detail, "starts from the beginning")
		assert.NotContains(t, v.Detail, "--restart")
		assert.NotContains(t, v.Detail, "resumes")
		assert.True(t, v.Abandoned, "the verdict says the run was discarded without the consumer reading the detail")
	})

	t.Run("a constraint the copied rows break is an abandoned run that names the slot as released", func(t *testing.T) {
		// 23502 not_null_violation: a re-added NOT NULL column with no
		// default fills from NULL on the first chunk.
		notNull := &pgconn.PgError{Code: "23502", Message: "null value in column violates not-null constraint"}
		runErr := &schemachange.AbandonedError{Schema: "billing", Table: "orders", Fault: fmt.Errorf("copy chunk: %w", notNull)}
		v, _ := copySwapRunVerdict(st, holdSlot, runErr)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, "the run was abandoned")
		assert.Contains(t, v.Detail, "its replication slot is dropped", "a live run's hold was its slot")
		assert.NotContains(t, v.Detail, "--restart")
		assert.True(t, v.Abandoned)
	})

	t.Run("a data fault whose abandon did not finish is coded as a data fault with the restart remedy", func(t *testing.T) {
		// The Runner joins the discard's failure behind the fault; the row
		// still stands as far as the discard got, so the operator finishes
		// the discard with the restart acknowledgement. The code still
		// names the fault, since that is what the operator corrects.
		tooLong := &pgconn.PgError{Code: "22001", Message: "value too long for type character varying(10)"}
		runErr := errors.Join(fmt.Errorf("copy chunk: %w", tooLong), errors.New("abandon run on billing.orders: drop shadow: connection reset"))
		v, err := copySwapRunVerdict(st, holdFence, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, string(executor.CodeDataFault), v.Code)
		assert.Contains(t, v.Detail, "could not be abandoned")
		assert.Contains(t, v.Detail, "start the run over")
		assert.Contains(t, v.Detail, "takes the write fence off the source", "a quiesced run's hold is its fence")
		assert.NotContains(t, v.Detail, "resumes")
		assert.False(t, v.Abandoned, "the same code as an abandoned run, told apart by the typed field")
	})

	t.Run("a server fault outside the data classes is resumed", func(t *testing.T) {
		// 57P01 admin_shutdown: the server went away under the copy; the
		// rows are not at fault and the next run carries on from the row.
		shutdown := &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}
		v, _ := copySwapRunVerdict(st, holdSlot, fmt.Errorf("copy chunk: %w", shutdown))
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.NotContains(t, v.Detail, "abandon")
	})

	t.Run("a stop a rerun clears says the slot stays until it resumes", func(t *testing.T) {
		v, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("copy: %w", context.Canceled))
		require.Error(t, err)
		assert.Contains(t, v.Detail, "replication slot stays")
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.NotContains(t, v.Detail, "abandon")
	})

	t.Run("a quiesced stop a rerun clears says the fence stays until it resumes", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, holdFence, fmt.Errorf("copy: %w", context.Canceled))
		assert.Contains(t, v.Detail, "fenced against writes")
		assert.Contains(t, v.Detail, "resumes from the row")
	})

	t.Run("a blocked-writes run stopped before its fence names no hold and resumes nothing", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, holdNone, fmt.Errorf("acquire table lock: %w", context.Canceled))
		assert.False(t, v.WritesBlocked, "the table kept taking writes")
		assert.Contains(t, v.Detail, "kept taking writes")
		assert.Contains(t, v.Detail, "starts from the beginning")
		assert.NotContains(t, v.Detail, "fenced against writes")
		assert.NotContains(t, v.Detail, "resumes from the row")
	})

	// A slot that retained more WAL than the ceiling with no transaction
	// holding it back stops a resume under the same ceiling again, so the
	// remedy names the setting to raise on both front doors. A stream that
	// fell behind leaves its slot holding more unconfirmed WAL than the
	// ceiling, which refuses a resume under the same ceiling before it
	// starts, so its remedy names the ceiling, the hand drop of
	// the slot, and the restart.
	t.Run("a slot over the ceiling on retained WAL names the ceiling to raise", func(t *testing.T) {
		lag := &decode.SlotLagExceededError{Slot: "pgsprite_x", Cause: decode.SlotLagWALRetained,
			Retained: 2 << 30, Unconfirmed: 1 << 20, Ceiling: 1 << 30}
		v, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("catch up: %w", lag))
		assert.ErrorIs(t, err, decode.ErrSlotLagCeiling)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeSlotLagExceeded), v.Code)
		assert.Equal(t, verdict.CauseSlotLagWALRetained, v.Cause)
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.Contains(t, v.Detail, "above the WAL the slot retained (--slot-lag-ceiling SIZE)")
	})

	// A slot past the hard limit is the same family: a resume under the
	// same ceiling stops there again, so the remedy names the setting too.
	t.Run("a slot past the hard limit names the ceiling to raise", func(t *testing.T) {
		lag := &decode.SlotLagExceededError{Slot: "pgsprite_x", Cause: decode.SlotLagWALRetainedHardLimit,
			Retained: 4 << 30, Unconfirmed: 1 << 20, Ceiling: 1 << 30}
		v, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("catch up: %w", lag))
		assert.ErrorIs(t, err, decode.ErrSlotLagCeiling)
		assert.Equal(t, string(executor.CodeSlotLagExceeded), v.Code)
		assert.Equal(t, verdict.CauseSlotLagWALRetainedHardLimit, v.Cause)
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.Contains(t, v.Detail, "above the WAL the slot retained (--slot-lag-ceiling SIZE)")
	})

	t.Run("a stream behind the ceiling names the ceiling to raise and the restart", func(t *testing.T) {
		lag := &decode.SlotLagExceededError{Slot: "pgsprite_x", Cause: decode.SlotLagStreamBehind,
			Retained: 2 << 30, Unconfirmed: 2 << 30, Ceiling: 1 << 30}
		v, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("catch up: %w", lag))
		assert.ErrorIs(t, err, decode.ErrSlotLagCeiling)
		assert.Equal(t, string(executor.CodeSlotLagExceeded), v.Code)
		assert.Equal(t, verdict.CauseSlotLagStreamBehind, v.Cause)
		js, err := v.JSON()
		require.NoError(t, err)
		assert.Contains(t, js, `"code": "slot-lag-exceeded"`)
		assert.Contains(t, js, `"cause": "stream-behind"`)
		assert.Contains(t, v.Detail, "resumes from the row")
		assert.Contains(t, v.Detail, "raise the slot-lag ceiling (--slot-lag-ceiling SIZE) with margin above the slot's unconfirmed WAL as pg_replication_slots reads it then")
		assert.Contains(t, v.Detail, "the resume counts that ceiling against --free-disk beside the shadow")
		assert.Contains(t, v.Detail, "drop the slot by hand (pg_drop_replication_slot), which keeps the shadow and the checkpoint row")
		assert.Contains(t, v.Detail, "(--restart SCHEMA.TABLE), which drops the shadow, drops the replication slot")
		assert.Contains(t, v.Detail, "checked against --free-disk as a fresh run")
	})

	// A resume refused for the slot's backlog is a refused verdict, not a
	// stop. Its detail is built from the slot's figures the refusal
	// carries and names the flags the CLI operator acts on, each remedy
	// once, without the library's own remedy text.
	t.Run("a resume refused for its slot's backlog names the ceiling to raise and the restart", func(t *testing.T) {
		refused := &schemachange.RefusalError{Cause: schemachange.CauseResumeSlotBacklog, Detail: "the library's account, with a SlotLagCeiling remedy"}
		backlog := &schemachange.SlotBacklogError{Slot: "pgsprite_x", Unconfirmed: 200 << 20, Ceiling: 128 << 20}
		v, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("run: %w: %w", refused, backlog))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ClassEnvironmental, v.Class)
		assert.Equal(t, verdict.Cause(schemachange.CauseResumeSlotBacklog), v.Cause)
		assert.Equal(t, "slot pgsprite_x holds 209715200 bytes of WAL its stream has not confirmed, over the 134217728-byte slot-lag ceiling, "+
			"so the resume was refused before its stream opened: no shadow row was cleared or copied and the slot confirmed nothing; "+
			"the checkpoint row remains and its replication slot stays and pins WAL on the server: "+slotBacklogRemedy, v.Detail)
	})

	// The CLI's account of a backlog refusal needs the slot's figures, so
	// a refusal of that cause that carries none is a broken contract with
	// the library, not a verdict.
	t.Run("a backlog refusal without the slot's figures is an invariant violation", func(t *testing.T) {
		refused := &schemachange.RefusalError{Cause: schemachange.CauseResumeSlotBacklog, Detail: "no figures"}
		_, err := copySwapRunVerdict(st, holdSlot, fmt.Errorf("run: %w", refused))
		assert.ErrorIs(t, err, executor.ErrInvariantViolation)
	})

	t.Run("a cancelled run is coded as cancelled by the caller", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, holdSlot, fmt.Errorf("copy: %w", context.Canceled))
		assert.Equal(t, string(executor.CodeCancelledByCaller), v.Code)
	})

	// The verdict's cause is the decode error's token, so the CLI's --json
	// and a library caller reading the error's Cause route on one string.
	t.Run("a slot lag verdict's cause is the error's own token", func(t *testing.T) {
		for _, cause := range []decode.SlotLagCause{
			decode.SlotLagStreamBehind, decode.SlotLagWALRetained, decode.SlotLagWALRetainedHardLimit,
		} {
			t.Run(string(cause), func(t *testing.T) {
				lag := &decode.SlotLagExceededError{Slot: "pgsprite_x", Cause: cause,
					Retained: 4 << 30, Unconfirmed: 2 << 30, Ceiling: 1 << 30}
				v, _ := copySwapRunVerdict(st, holdSlot, fmt.Errorf("catch up: %w", lag))
				assert.Equal(t, string(cause), string(v.Cause))
			})
		}
	})

	// The Runner joins a capture's stop with the fill's, and the catch-up
	// carries a cycle's failure beside the caller's cancellation, so a slot
	// lag stop can travel with an outcome that outranks it; the verdict
	// then carries that outcome's code and no slot lag cause. A divergence
	// needs a restart, not the resume a slot lag code invites.
	t.Run("a cancellation, a breach, a data fault, or a divergence outranks a slot lag stop", func(t *testing.T) {
		lag := &decode.SlotLagExceededError{Slot: "pgsprite_x", Cause: decode.SlotLagStreamBehind,
			Retained: 2 << 30, Unconfirmed: 2 << 30, Ceiling: 1 << 30}
		for _, tc := range []struct {
			name string
			with error
			want executor.Code
		}{
			{"cancellation", fmt.Errorf("catch up: %w", context.Canceled), executor.CodeCancelledByCaller},
			{"breach", fmt.Errorf("gate: %w", schemachange.ErrInvariantViolation), executor.CodeInvariantViolation},
			{"data fault", fmt.Errorf("copy chunk: %w", &pgconn.PgError{Code: "22001"}), executor.CodeDataFault},
			{"divergence", &checksum.DivergenceError{Report: checksum.Report{Chunks: 3, Mismatches: []checksum.Mismatch{{}}}}, executor.CodeExecutionFailed},
		} {
			t.Run(tc.name, func(t *testing.T) {
				v, _ := copySwapRunVerdict(st, holdSlot, errors.Join(lag, tc.with))
				assert.Equal(t, string(tc.want), v.Code)
				assert.Equal(t, verdict.CauseNone, v.Cause)
			})
		}
	})

	t.Run("a breach of the shadow's invariants is coded as an invariant violation", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, holdSlot, fmt.Errorf("gate: %w", schemachange.ErrInvariantViolation))
		assert.Equal(t, string(executor.CodeInvariantViolation), v.Code)
	})

	t.Run("a rolled-back swap is an execution failure", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, holdSlot, schemachange.ErrCutoverRolledBack)
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
			Verification:    checksum.Report{Rows: 7, Chunks: 1, Unsettled: 1, Carried: 2},
			OldTableDropped: true,
		}
		v, err := copySwapOutcome(st, holdSlot, done, releaseErr)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.Empty(t, v.Code)
		assert.Contains(t, v.Detail, "verified 7 rows in 1 chunks")
		assert.Contains(t, v.Detail, "the run then returned: release table lock: connection closed")
		assert.Equal(t, &verdict.Verification{Rows: 7, Chunks: 1, Unsettled: 1, Carried: 2}, v.Verification,
			"the pass's figures ride the verdict whatever failed after the done row")
	})

	t.Run("a done row without an error carries no release wording", func(t *testing.T) {
		done := schemachange.Result{Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}}
		v, err := copySwapOutcome(st, holdSlot, done, nil)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.NotContains(t, v.Detail, "the run then returned")
		assert.False(t, v.WritesBlocked, "a live run's verdict does not claim the table was offline")
		assert.Empty(t, v.SwapLSN, "a run whose proof carries no swap position reports none")
	})

	t.Run("the swap's WAL position is rendered as pg_lsn text", func(t *testing.T) {
		done := schemachange.Result{
			Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone},
			Swapped:    schemachange.SwapProof{SwapPosition: 0x1_00A2B3C8},
			SwapProven: true,
		}
		v, err := copySwapOutcome(st, holdSlot, done, nil)
		require.NoError(t, err)
		assert.Equal(t, "1/A2B3C8", v.SwapLSN)
	})

	t.Run("a done row without a verification pass carries no verification", func(t *testing.T) {
		done := schemachange.Result{Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}}
		v, err := copySwapOutcome(st, holdSlot, done, nil)
		require.NoError(t, err)
		assert.Nil(t, v.Verification, "a run resumed after its swap made no pass to report")
	})

	t.Run("a pass that compared a chunk of no rows still reports its figures", func(t *testing.T) {
		done := schemachange.Result{
			Checkpoint:   checkpoint.Checkpoint{Phase: checkpoint.PhaseDone},
			Verification: checksum.Report{Chunks: 1},
		}
		v, err := copySwapOutcome(st, holdSlot, done, nil)
		require.NoError(t, err)
		assert.Equal(t, &verdict.Verification{Chunks: 1}, v.Verification,
			"whether a pass ran is keyed on the chunks it compared, not the rows it hashed")
	})

	t.Run("a block-writes done row is marked writes-blocked", func(t *testing.T) {
		done := schemachange.Result{Checkpoint: checkpoint.Checkpoint{Phase: checkpoint.PhaseDone}}
		v, err := copySwapOutcome(st, holdFence, done, nil)
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeExecutedViaCopyAndSwap, v.Outcome)
		assert.True(t, v.WritesBlocked, "the executed verdict says the table was offline for writes while the run held it")
	})

	t.Run("an error with the zero result is a failure", func(t *testing.T) {
		v, err := copySwapOutcome(st, holdSlot, schemachange.Result{}, releaseErr)
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
			KeepOld:             true,
			Copier:              copier.Options{Workers: 3},
			CheckpointInterval:  2 * time.Second,
			SlotLagCeilingBytes: 3 << 30,
		}.runOptions(logger)
		assert.True(t, got.KeepOld)
		assert.True(t, got.ReplaceDone)
		assert.Equal(t, 3, got.Copier.Workers)
		assert.Equal(t, 2*time.Second, got.CheckpointInterval)
		assert.Same(t, logger, got.Catchup.Logger, "the catch-up's requeue warnings reach the call's diagnostics")
		assert.Equal(t, int64(3<<30), got.Catchup.SlotLagCeiling, "the ceiling reaches the catch-up that enforces it")
	})

	t.Run("an unset ceiling leaves the catch-up its default", func(t *testing.T) {
		got := CopySwapOptions{}.runOptions(logger)
		assert.Zero(t, got.Catchup.SlotLagCeiling, "zero is the catch-up's decode.DefaultSlotLagCeiling")
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

// The preflight's disk check counts the ceiling the catch-up will enforce:
// the host's when it set one, decode.DefaultSlotLagCeiling when it left it
// zero, so an unset ceiling is not counted as no WAL at all.
func TestCopySwapEnvironmentCountsTheEffectiveSlotLagCeiling(t *testing.T) {
	t.Run("a set ceiling is counted as given", func(t *testing.T) {
		got := CopySwapOptions{FreeDiskBytes: 8 << 30, SlotLagCeilingBytes: 3 << 30}.environment()
		assert.True(t, got.LogicalDecoding)
		assert.Equal(t, int64(8<<30), got.FreeDiskBytes)
		assert.Equal(t, int64(3<<30), got.SlotLagCeilingBytes)
	})

	t.Run("an unset ceiling is counted as the default", func(t *testing.T) {
		got := CopySwapOptions{FreeDiskBytes: 8 << 30}.environment()
		assert.Equal(t, decode.DefaultSlotLagCeiling, got.SlotLagCeilingBytes)
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

	// A swap that compared the keys its live pass left names how many and
	// how long the compare ran under the lock, so a host reading the
	// verdict sees what the swap paid without --debug.
	carried := copySwapDetail(schemachange.Result{
		Verification:    checksum.Report{Rows: 300, Chunks: 1},
		CarriedCompare:  schemachange.CarriedCompare{Keys: 7, Elapsed: 12 * time.Millisecond},
		OldTableDropped: true,
	}, false)
	assert.Equal(t, "verified 300 rows in 1 chunks; compared 7 still-changing keys under the swap lock in 12ms; "+
		"the retained source is dropped", carried)

	quiesced := copySwapDetail(schemachange.Result{
		Verification:    checksum.Report{Rows: 4, Chunks: 1},
		OldTableDropped: true,
	}, true)
	assert.Equal(t, "verified 4 rows in 1 chunks; the retained source is dropped; "+
		"writes to the table were blocked for the length of the run", quiesced)

	// A run that only finished a swap an earlier run committed says so,
	// rather than reading as a pass over an empty table.
	finished := copySwapDetail(schemachange.Result{
		FinishedEarlierSwap: true,
		OldTableDropped:     true,
	}, false)
	assert.Equal(t, "finished the swap an earlier run committed; nothing was copied or verified by this run; "+
		"the retained source is dropped", finished)
}

// The verification pass's figures reach automation as the verdict's
// verification object, not only as the detail's prose: a live pass that
// settled one chunk against the stream and left three keys of two unsettled
// chunks for the swap serialises those exact counts, the key count and
// never the keys, while the detail stays the prose it always was.
func TestCopySwapVerdictCarriesTheVerificationFigures(t *testing.T) {
	st := parseCopySwapStatement(t)
	done := schemachange.Result{
		Checkpoint:      checkpoint.Checkpoint{Phase: checkpoint.PhaseDone},
		Verification:    checksum.Report{Rows: 52693, Chunks: 26, Settled: 1, Unsettled: 2, Carried: 3},
		OldTableDropped: true,
	}
	v, err := copySwapOutcome(st, holdSlot, done, nil)
	require.NoError(t, err)

	js, err := v.JSON()
	require.NoError(t, err)
	var decoded struct {
		Verification json.RawMessage `json:"verification"`
	}
	require.NoError(t, json.Unmarshal([]byte(js), &decoded))
	assert.JSONEq(t, `{"rows": 52693, "chunks": 26, "settled": 1, "unsettled": 2, "carried": 3}`,
		string(decoded.Verification))
	assert.Equal(t, "verified 52693 rows in 26 chunks (1 settled against the stream); "+
		"the retained source is dropped", v.Detail)
}
