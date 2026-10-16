package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.Error(t, err)
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
		v, err := copySwapRunVerdict(st, fmt.Errorf("run billing.orders: %w", refusal))
		require.NoError(t, err)
		assert.Equal(t, verdict.OutcomeRefused, v.Outcome)
		assert.Equal(t, verdict.ReasonCopyAndSwapRefused, v.Reason)
		assert.Equal(t, verdict.ClassEnvironmental, v.Class)
		assert.Equal(t, verdict.Cause(schemachange.CauseNameTaken), v.Cause)
		assert.Empty(t, v.Code, "a refusal is not a failure")
	})

	t.Run("a divergence is a failure returned with its error", func(t *testing.T) {
		divergence := &checksum.DivergenceError{Report: checksum.Report{Chunks: 3, Mismatches: []checksum.Mismatch{{}}}}
		runErr := fmt.Errorf("run billing.orders: %w", divergence)
		v, err := copySwapRunVerdict(st, runErr)
		assert.ErrorIs(t, err, runErr)
		assert.Equal(t, verdict.OutcomeFailed, v.Outcome)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
		assert.Equal(t, "billing.orders", v.Table)
		assert.Contains(t, v.Detail, "checkpoint row remains")
	})

	t.Run("a cancelled run is coded as cancelled by the caller", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, fmt.Errorf("copy: %w", context.Canceled))
		assert.Equal(t, string(executor.CodeCancelledByCaller), v.Code)
	})

	t.Run("a breach of the shadow's invariants is coded as an invariant violation", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, fmt.Errorf("gate: %w", schemachange.ErrInvariantViolation))
		assert.Equal(t, string(executor.CodeInvariantViolation), v.Code)
	})

	t.Run("a rolled-back swap is an execution failure", func(t *testing.T) {
		v, _ := copySwapRunVerdict(st, schemachange.ErrCutoverRolledBack)
		assert.Equal(t, string(executor.CodeExecutionFailed), v.Code)
	})
}

// The detail names what the run proved; the kept-old wording and the
// dropped wording are never both present.
func TestCopySwapDetail(t *testing.T) {
	dropped := copySwapDetail(schemachange.Result{
		Verification:    checksum.Report{Rows: 2500, Chunks: 3},
		OldTableDropped: true,
	})
	assert.Equal(t, "verified 2500 rows in 3 chunks; the retained source is dropped", dropped)

	kept := copySwapDetail(schemachange.Result{
		Verification: checksum.Report{Rows: 10, Chunks: 1, Settled: 1},
		Reconciled:   true,
	})
	assert.Equal(t, "verified 10 rows in 1 chunks (1 settled against the stream); "+
		"the retained source is kept under its _old name; "+
		"the run reconciled a lost replication slot before catching up", kept)
}
