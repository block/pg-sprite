package migrate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
)

// An adoption is diff-planned SQL, which is never forced, so a forced
// attempt that carries one is a breach of the plan's contract. It refuses
// before the table is even read: the pool is never touched.
func TestExecuteRefusesAForcedAttemptThatAdoptsAnIndex(t *testing.T) {
	st, err := statement.ParseOne(`ALTER TABLE s.t ADD CONSTRAINT t_email_key UNIQUE USING INDEX t_email_key`)
	require.NoError(t, err)
	adopts := &schemadiff.IndexAdoption{Index: "t_email_key", Def: "CREATE UNIQUE INDEX t_email_key ON t USING btree (email)"}

	_, err = execute(t.Context(), nil, st, []string{st.SQL()}, planner.Plan{}, false, true, Options{}, 1, adopts, nil, nil, nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	require.ErrorContains(t, err, "a forced attempt cannot carry the adoption of index t_email_key")
}

// A validation is diff-planned SQL as well, so a forced attempt that
// carries one refuses the same way, before the pool is touched.
func TestExecuteRefusesAForcedAttemptThatValidatesAConstraint(t *testing.T) {
	st, err := statement.ParseOne(`ALTER TABLE s.t VALIDATE CONSTRAINT t_qty_check`)
	require.NoError(t, err)
	validates := &schemadiff.ConstraintValidation{Constraint: "t_qty_check", Def: "CHECK ((qty > 0)) NOT VALID"}

	_, err = execute(t.Context(), nil, st, []string{st.SQL()}, planner.Plan{}, false, true, Options{}, 1, nil, validates, nil, nil)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	require.ErrorContains(t, err, "a forced attempt cannot carry the validation of constraint t_qty_check")
}

// A reviewed drop runs only on a route that reads the object again right
// before it drops it, so a statement routed anywhere but native that carries
// a drop refuses before the statement runs.
func TestCheckDropRouteRefusesANonNativeRoute(t *testing.T) {
	dropped := &schemadiff.DroppedObject{Kind: schemadiff.DroppedColumn, Name: "note", Definition: "text"}
	native := router.Statement{Disposition: router.DispositionExecute, Backend: router.BackendNative}
	copySwap := router.Statement{Disposition: router.DispositionExecute, Backend: router.BackendCopyAndSwap}

	require.NoError(t, checkDropRoute(native, dropped))
	require.NoError(t, checkDropRoute(copySwap, nil), "a statement with no drop is not this check's")
	err := checkDropRoute(copySwap, dropped)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	require.ErrorContains(t, err, "a copy-and-swap route cannot carry the drop of column note")
}
