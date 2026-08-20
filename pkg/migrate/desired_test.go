package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
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
	assert.Contains(t, err.Error(), "imperative front door",
		"the rejection points the caller at the front door where force applies")
	assert.Equal(t, DesiredResult{}, res)
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
	})

	t.Run("refuses a greenfield plan", func(t *testing.T) {
		report := executable()
		exists := false
		report.TableExists = &exists
		res, ok := admitPlan(DesiredRequest{}, report)
		require.False(t, ok)
		assert.Equal(t, verdict.ReasonUnsupportedStatement, res.Reason)
		assert.Contains(t, res.Detail, "app.t")
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
		}
	})
}
