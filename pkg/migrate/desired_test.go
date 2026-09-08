package migrate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/executor"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/verdict"
)

func TestRunDesiredRejectsUnrunnableOptions(t *testing.T) {
	// Options validation happens before any database work, so no pool is
	// needed: desired-state execution enforces the same "the zero value is
	// not a runnable policy" contract Run does.
	res, err := RunDesired(t.Context(), nil, DesiredRequest{}, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MaxTableSizeBytes",
		"the rejection must name the Options field")
	assert.Equal(t, DesiredResult{}, res, "an error before a plan carries a zero result")
}

func TestRunDesiredRejectsForce(t *testing.T) {
	opts := DefaultOptions()
	opts.Force = "public.t"
	res, err := RunDesired(t.Context(), nil, DesiredRequest{}, opts)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrForceNotSupported,
		"the rejection is the typed sentinel an embedder can branch on")
	assert.Contains(t, err.Error(), "imperative front door",
		"the rejection points the caller at the front door where force applies")
	assert.Equal(t, DesiredResult{}, res)
}

// A greenfield create-shape refusal explains itself through the typed
// cause the executor stamped on the statement; a refusal without one
// falls back to the generic wording rather than inventing a cause.
func TestPlanRefusalRendersCreateShapeCause(t *testing.T) {
	missing := false
	refused := plan.Report{
		TableExists: &missing,
		Disposition: router.DispositionRefuse,
		Statements: []plan.Statement{
			{
				SQL:         "CREATE TABLE child PARTITION OF parent FOR VALUES IN (1)",
				Disposition: router.DispositionRefuse,
				Reason:      verdict.ReasonUnsupportedStatement,
				Cause:       executor.CreateShapePartitionOf,
			},
		},
	}
	reason, detail := planRefusal(refused)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, reason)
	assert.Contains(t, detail, "planned statement 1 (")
	assert.Contains(t, detail, "is refused by the create path: "+executor.CreateShapePartitionOf.Description())
	assert.Contains(t, detail, "nothing was executed")

	uncaused := refused
	uncaused.Statements = []plan.Statement{
		{SQL: "ALTER TABLE t NO SUCH THING", Disposition: router.DispositionRefuse},
	}
	reason, detail = planRefusal(uncaused)
	assert.Equal(t, verdict.ReasonUnsupportedStatement, reason,
		"a refused statement without a reason still reports the unsupported-statement reason")
	assert.Contains(t, detail, "has no safe path")
	assert.NotContains(t, detail, "create path")
}

func TestAdmitPlan(t *testing.T) {
	executable := func() plan.Report {
		exists := true
		return plan.Report{
			Fingerprint: "fp-live",
			Schema:      "app",
			Table:       "t",
			TableExists: &exists,
			Disposition: router.DispositionExecute,
			Statements: []plan.Statement{
				{SQL: "ALTER TABLE app.t ADD COLUMN v text", Disposition: router.DispositionExecute},
			},
		}
	}

	t.Run("admits an executable plan", func(t *testing.T) {
		_, ok := admitPlan(DesiredRequest{}, executable())
		assert.True(t, ok)
	})

	t.Run("admits a matching pinned fingerprint", func(t *testing.T) {
		_, ok := admitPlan(DesiredRequest{ExpectedFingerprint: "fp-live"}, executable())
		assert.True(t, ok)
	})

	t.Run("refuses a pinned fingerprint mismatch before every other check", func(t *testing.T) {
		// The plan is also greenfield and destructive: the mismatch must
		// win, because the caller's approval is void whatever else holds.
		report := executable()
		exists := false
		report.TableExists = &exists
		report.Statements[0].Destructive = true
		res, ok := admitPlan(DesiredRequest{ExpectedFingerprint: "fp-reviewed"}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.OutcomeRefused, res.Outcome)
		assert.Equal(t, verdict.ReasonPlanFingerprintMismatch, res.Reason)
		assert.Contains(t, res.Detail, "fp-reviewed")
		assert.Contains(t, res.Detail, "fp-live")
		assert.Equal(t, report, res.Plan, "a refusal carries the plan it refused")
	})

	t.Run("admits a greenfield plan", func(t *testing.T) {
		// A table that does not exist takes the create path after
		// admission; admission itself only vets the plan's content — the
		// pin, the destructive guard, and the routed dispositions.
		report := executable()
		exists := false
		report.TableExists = &exists
		_, ok := admitPlan(DesiredRequest{}, report)
		assert.True(t, ok)
	})

	t.Run("refuses a destructive statement anywhere in the plan", func(t *testing.T) {
		report := executable()
		report.Statements = append(report.Statements, plan.Statement{
			SQL:         "ALTER TABLE app.t DROP COLUMN old",
			Destructive: true,
			Disposition: router.DispositionExecute,
		})
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Contains(t, res.Detail, "statement 2")
		assert.Contains(t, res.Detail, "DROP COLUMN old")
		assert.Contains(t, res.Detail, "imperative front door",
			"an ALTER TABLE drop is pointed at the door that runs it deliberately")
		assert.Contains(t, res.Detail, "the plan's other statement, even if non-destructive, was not run",
			"a multi-statement refusal discloses that the rest of the plan was skipped too")
		assert.Equal(t, report, res.Plan, "a refusal carries the plan it refused")
	})

	t.Run("counts the skipped statements when more than one is blocked", func(t *testing.T) {
		report := executable()
		report.Statements = append(report.Statements,
			plan.Statement{
				SQL:         "ALTER TABLE app.t DROP COLUMN old",
				Destructive: true,
				Disposition: router.DispositionExecute,
			},
			plan.Statement{
				SQL:         "CREATE INDEX t_v_idx ON app.t (v)",
				Disposition: router.DispositionExecute,
			})
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Contains(t, res.Detail, "the plan's 2 other statements, non-destructive ones included, were not run",
			"the disclosure counts every skipped statement, before and after the destructive one")
	})

	t.Run("a single-statement destructive refusal claims no skipped statements", func(t *testing.T) {
		report := executable()
		report.Statements = []plan.Statement{{
			SQL:         "ALTER TABLE app.t DROP COLUMN old",
			Destructive: true,
			Disposition: router.DispositionExecute,
		}}
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.NotContains(t, res.Detail, "all-or-nothing",
			"there is nothing else in the plan to disclose as skipped")
	})

	t.Run("points a destructive index drop at its concurrent idiom", func(t *testing.T) {
		// The imperative front door refuses a plain DROP INDEX, so the
		// refusal must not send an index drop there — it names the
		// concurrent idiom the operator can run directly.
		report := executable()
		report.Statements = append(report.Statements, plan.Statement{
			SQL:         `DROP INDEX "app"."t_v_idx"`,
			Kind:        schemadiff.ChangeDropIndex,
			Destructive: true,
			Disposition: router.DispositionExecute,
		})
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonDestructiveChange, res.Reason)
		assert.Contains(t, res.Detail, "DROP INDEX CONCURRENTLY")
		assert.NotContains(t, res.Detail, "imperative front door",
			"the front door would refuse the drop; the detail must not point there")
	})

	t.Run("maps the first non-executable disposition to its refusal", func(t *testing.T) {
		cases := []struct {
			disposition router.Disposition
			stReason    verdict.Reason
			want        verdict.Reason
		}{
			{router.DispositionRewriteRequired, verdict.ReasonNone, verdict.ReasonRewriteRequired},
			{router.DispositionUnavailable, verdict.ReasonNone, verdict.ReasonBackendUnavailable},
			{router.DispositionRefuse, verdict.ReasonUnsupportedPartitionedParent, verdict.ReasonUnsupportedPartitionedParent},
			{router.DispositionRefuse, verdict.ReasonNone, verdict.ReasonUnsupportedStatement},
		}
		for _, tc := range cases {
			report := executable()
			report.Disposition = tc.disposition
			report.Statements = append(report.Statements, plan.Statement{
				SQL:         "ALTER TABLE app.t ALTER COLUMN v TYPE bigint",
				Disposition: tc.disposition,
				Reason:      tc.stReason,
			})
			res, ok := admitPlan(DesiredRequest{}, report)
			require.False(t, ok, "disposition %s", tc.disposition)
			assert.Equal(t, tc.want, res.Reason, "disposition %s", tc.disposition)
			assert.Contains(t, res.Detail, "statement 2", "the detail names the non-executable statement")
			assert.Contains(t, res.Detail, "nothing was executed")
			assert.Equal(t, report, res.Plan, "a refusal carries the plan it refused")
		}
	})
}

// committedPrefixDetail is the disclosure of how far convergence got; its
// arithmetic must not drift — the stopping statement is 1-based, the
// committed prefix count is the 0-based index. As a renderer helper its
// exact wording is pinned here, in its own unit test.
func TestCommittedPrefixDetail(t *testing.T) {
	assert.Equal(t,
		"planned statement 3 of 5 failed; the 2 preceding statements committed and remain in effect",
		committedPrefixDetail(2, 5, "failed"))
	assert.Equal(t,
		"planned statement 1 of 2 stopped before a verdict; nothing about it was executed; "+
			"nothing was committed before it",
		committedPrefixDetail(0, 2, stoppedBeforeVerdict))
}

// The failed-create verdict's detail must state what the failed step left:
// a rolled-back bounded attempt for most causes, but a standing table when
// the CREATE TABLE committed and its owned-name verification then failed
// or could not complete.
func TestCreateFailureDetailNamesWhatTheStepLeft(t *testing.T) {
	rolledBack := createFailureDetail(errors.New("lock_timeout"))
	assert.Contains(t, rolledBack, "rolled back")
	assert.NotContains(t, rolledBack, "committed")

	mismatch := createFailureDetail(&executor.SequenceStepError{Step: 1, Total: 2, Err: &executor.CreateNameMismatchError{
		Schema: "app", Table: "t", Missing: []string{"t_pkey"}, Unclaimed: []string{"t_pkey1"},
	}})
	assert.Contains(t, mismatch, "committed and was not rolled back")
	assert.Contains(t, mismatch, `"t_pkey1"`)
	assert.NotContains(t, mismatch, "the step's bounded attempt failed and rolled back")

	unverified := createFailureDetail(&executor.SequenceStepError{Step: 1, Total: 2,
		Err: fmt.Errorf("%w: app.t: %w", executor.ErrCreateNamesUnverified, context.Canceled)})
	assert.Contains(t, unverified, "committed and was not rolled back")
	assert.Contains(t, unverified, "unproven")
	assert.NotContains(t, unverified, "the step's bounded attempt failed and rolled back")
}
