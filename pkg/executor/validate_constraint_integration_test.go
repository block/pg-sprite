package executor_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/progress"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// A planned VALIDATE CONSTRAINT names only the constraint, so the step
// checks the constraint against the definition the plan matched before it
// validates it. A constraint replaced under the same name after planning,
// or dropped, refuses with the permanent validated-constraint-drift code
// and nothing is validated; the constraint the plan matched is validated,
// and so is one validated since planning with the same definition.
func TestRunSequenceValidatingRefusesAConstraintReplacedAfterPlanning(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id bigint, qty int, code text)",
		"INSERT INTO %[1]s.t VALUES (1, 5, 'a')",
		"ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID",
		"ALTER TABLE %[1]s.t ADD CONSTRAINT t_code_check CHECK (code <> '') NOT VALID",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	pt := mustPreflight(t, pool, schema, "t")
	validate := func(constraint, def string) error {
		t.Helper()
		_, err := executor.RunSequenceValidating(t.Context(), pool, pt,
			[]string{fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT %s", schema, constraint)},
			runBudget, executor.DefaultRetryPolicy(),
			schemadiff.ConstraintValidation{Constraint: constraint, Def: def})
		return err
	}
	const plannedQty = "CHECK ((qty > 0)) NOT VALID"

	// The constraint is replaced after the plan matched it: same name, a
	// check the plan never reviewed.
	for _, ddl := range []string{
		"ALTER TABLE %[1]s.t DROP CONSTRAINT t_qty_check",
		"ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 3) NOT VALID",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	err := validate("t_qty_check", plannedQty)
	require.ErrorIs(t, err, executor.ErrValidatedConstraintDrift)
	assert.Equal(t, executor.CodeValidatedConstraintDrift, executor.OutcomeCode(err))
	assert.Contains(t, err.Error(), "CHECK ((qty > 3)) NOT VALID")
	exists, validated := constraintState(t, pool, schema, "t", "t_qty_check")
	require.True(t, exists)
	assert.False(t, validated, "the replacement constraint was not validated")

	_, err = pool.Exec(t.Context(), fmt.Sprintf("ALTER TABLE %s.t DROP CONSTRAINT t_qty_check", schema))
	require.NoError(t, err)
	err = validate("t_qty_check", plannedQty)
	require.ErrorIs(t, err, executor.ErrValidatedConstraintDrift)
	assert.Contains(t, err.Error(), "no constraint named t_qty_check")

	const plannedCode = "CHECK ((code <> ''::text)) NOT VALID"
	require.NoError(t, validate("t_code_check", plannedCode))
	_, validated = constraintState(t, pool, schema, "t", "t_code_check")
	assert.True(t, validated)
	require.NoError(t, validate("t_code_check", plannedCode), "validating it again changes nothing")
}

// The check takes the lock VALIDATE CONSTRAINT takes, SHARE UPDATE
// EXCLUSIVE, not the ACCESS EXCLUSIVE an adoption's check takes: the
// validation still runs beside a writer whose transaction is open on the
// table.
func TestRunSequenceValidatingRunsBesideAnOpenWriter(t *testing.T) {
	pool, schema := newPool(t)
	for _, ddl := range []string{
		"CREATE TABLE %[1]s.t (id bigint, qty int)",
		"INSERT INTO %[1]s.t VALUES (1, 5)",
		"ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID",
	} {
		_, err := pool.Exec(t.Context(), fmt.Sprintf(ddl, schema))
		require.NoError(t, err, ddl)
	}
	pt := mustPreflight(t, pool, schema, "t")

	writer, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = writer.Rollback(context.WithoutCancel(t.Context())) }()
	_, err = writer.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s.t VALUES (2, 7)", schema))
	require.NoError(t, err)

	_, err = executor.RunSequenceValidating(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT t_qty_check", schema)},
		runBudget, executor.RetryPolicy{MaxAttempts: 1},
		schemadiff.ConstraintValidation{Constraint: "t_qty_check", Def: "CHECK ((qty > 0)) NOT VALID"})
	require.NoError(t, err)
	_, validated := constraintState(t, pool, schema, "t", "t_qty_check")
	assert.True(t, validated)
}

// A planned validation no step of the sequence performs is a breach of the
// plan's contract: it refuses before anything runs.
func TestRunSequenceValidatingRefusesASequenceThatValidatesNothing(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf("CREATE TABLE %s.t (id bigint, note text)", schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")

	_, err = executor.RunSequenceValidating(t.Context(), pool, pt,
		[]string{fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)},
		runBudget, executor.DefaultRetryPolicy(),
		schemadiff.ConstraintValidation{Constraint: "t_note_check", Def: "CHECK ((note <> ''::text)) NOT VALID"})
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Equal(t, "text", columnType(t, pool, schema, "t", "note"), "nothing ran")
}

// A caller that tracks progress runs a planned validation as one step: the
// constraint the plan matched is validated, and the tracker reports the
// step as a constraint-validation scan, as the sequence executor does,
// with the exact statement, the one attempt, and a finished phase.
func TestExecuteNativeValidatingWithProgressValidatesThePlannedConstraint(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (
			id  bigint,
			qty int
		);
		INSERT INTO %[1]s.t VALUES (1, 5);
		ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	sql := fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT t_qty_check", schema)
	require.NoError(t, executor.ExecuteNativeValidatingWithProgress(t.Context(), pool, pt, mustParse(t, sql),
		budget, executor.DefaultRetryPolicy(),
		schemadiff.ConstraintValidation{Constraint: "t_qty_check", Def: "CHECK ((qty > 0)) NOT VALID"},
		tracker))

	_, validated := constraintState(t, pool, schema, "t", "t_qty_check")
	assert.True(t, validated, "the planned constraint was validated")
	requireOneStepSnapshot(t, tracker, progress.PhaseFinished, progress.OperationValidate, sql, 1)
}

// The progress-tracked validation reads the constraint again under the
// validation's lock, exactly as the sequence's validating step does: a
// constraint replaced under the same name after planning refuses with the
// permanent validated-constraint-drift code, nothing is validated, and the
// tracker records the failure against the statement.
func TestExecuteNativeValidatingWithProgressRefusesAConstraintReplacedAfterPlanning(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (
			id  bigint,
			qty int
		);
		INSERT INTO %[1]s.t VALUES (1, 5);
		ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 0) NOT VALID`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	// The plan matched CHECK (qty > 0); it is replaced under the same name
	// by a check the plan never reviewed before the statement runs.
	_, err = pool.Exec(t.Context(), fmt.Sprintf(`
		ALTER TABLE %[1]s.t DROP CONSTRAINT t_qty_check;
		ALTER TABLE %[1]s.t ADD CONSTRAINT t_qty_check CHECK (qty > 3) NOT VALID`, schema))
	require.NoError(t, err)

	sql := fmt.Sprintf("ALTER TABLE %s.t VALIDATE CONSTRAINT t_qty_check", schema)
	err = executor.ExecuteNativeValidatingWithProgress(t.Context(), pool, pt, mustParse(t, sql),
		budget, executor.DefaultRetryPolicy(),
		schemadiff.ConstraintValidation{Constraint: "t_qty_check", Def: "CHECK ((qty > 0)) NOT VALID"},
		tracker)
	require.ErrorIs(t, err, executor.ErrValidatedConstraintDrift)
	assert.Equal(t, executor.CodeValidatedConstraintDrift, executor.OutcomeCode(err))
	exists, validated := constraintState(t, pool, schema, "t", "t_qty_check")
	require.True(t, exists)
	assert.False(t, validated, "the replacement constraint was not validated")
	requireOneStepSnapshot(t, tracker, progress.PhaseFailed, progress.OperationValidate, sql, 1)
}

// A planned validation the statement does not perform is a breach of the
// plan's contract: it refuses as an invariant violation before any attempt
// runs, so the column stays and the tracker observed no attempt.
func TestExecuteNativeValidatingWithProgressRefusesAStatementThatValidatesNothing(t *testing.T) {
	pool, schema := newPool(t)
	_, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE TABLE %[1]s.t (
			id   bigint,
			note text
		)`, schema))
	require.NoError(t, err)
	pt := mustPreflight(t, pool, schema, "t")
	tracker, err := progress.NewTracker(progress.WallClock{})
	require.NoError(t, err)

	sql := fmt.Sprintf("ALTER TABLE %s.t DROP COLUMN note", schema)
	err = executor.ExecuteNativeValidatingWithProgress(t.Context(), pool, pt, mustParse(t, sql),
		budget, executor.DefaultRetryPolicy(),
		schemadiff.ConstraintValidation{Constraint: "t_note_check", Def: "CHECK ((note <> ''::text)) NOT VALID"},
		tracker)
	require.ErrorIs(t, err, executor.ErrInvariantViolation)
	assert.Equal(t, "text", columnType(t, pool, schema, "t", "note"), "nothing ran")
	requireOneStepSnapshot(t, tracker, progress.PhaseFailed, progress.OperationValidate, sql, 0)
}
