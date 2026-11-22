package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/migrate"
	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// desiredReport is a desired-state plan that adds a column and, when
// destructive, drops another, as the planner derives it.
func desiredReport(destructive bool) plan.Report {
	report := plan.NewReport(plan.SourceDiff)
	report.Schema, report.Table = "app", "orders"
	report.Disposition = router.DispositionExecute
	report.Statements = []plan.Statement{
		{SQL: "ALTER TABLE app.orders ADD COLUMN note text", Disposition: router.DispositionExecute},
	}
	if destructive {
		report.Statements = append(report.Statements, plan.Statement{
			SQL: "ALTER TABLE app.orders DROP COLUMN legacy", Destructive: true, Disposition: router.DispositionExecute,
		})
	}
	report.Fingerprint = report.ComputeFingerprint()
	return report
}

// The diff text prints the plan's fingerprint, the pin a desired-state run
// takes, after the plan summary and before the pointers.
func TestDiffTextPrintsTheFingerprint(t *testing.T) {
	report := desiredReport(true)
	var out strings.Builder
	require.NoError(t, writeDiffText(&out, palette{}, report))
	assert.Contains(t, out.String(), "plan:\n  app.orders — 2 statements, 0 steps to run, 0 refused\n\nfingerprint:\n  "+report.Fingerprint+"\n\ndiff:\n")
}

// A dry run previews a destructive plan as refused unless it carries both
// the pin of the plan the diff printed and the table in the destructive
// acknowledgement. A refused preview hands back the fingerprint to pin,
// which is not the refused preview's own.
func TestPreviewDesiredPlan(t *testing.T) {
	report := desiredReport(true)
	cases := []struct {
		name    string
		cmd     MigrateCmd
		refused bool
	}{
		{"no flags", MigrateCmd{}, true},
		{"the table without the pin", MigrateCmd{AllowDestructive: "app.orders"}, true},
		{"the pin without the table", MigrateCmd{ExpectFingerprint: report.Fingerprint}, true},
		{"another table", MigrateCmd{AllowDestructive: "app.other", ExpectFingerprint: report.Fingerprint}, true},
		{"a pin for another plan", MigrateCmd{AllowDestructive: "app.orders", ExpectFingerprint: "sha256:older"}, true},
		{"the pin and the table", MigrateCmd{AllowDestructive: "app.orders", ExpectFingerprint: report.Fingerprint}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview, pinnable := tc.cmd.previewDesiredPlan(desiredReport(true))
			if !tc.refused {
				assert.Empty(t, pinnable)
				assert.Equal(t, report, preview, "the plan the run executes")
				return
			}
			assert.Equal(t, report.Fingerprint, pinnable)
			assert.Equal(t, router.DispositionRefuse, preview.Disposition)
			assert.Equal(t, verdict.ReasonDestructiveChange, preview.Reason)
			assert.NotEqual(t, report.Fingerprint, preview.Fingerprint, "the refused preview is a different plan")
		})
	}

	additive := desiredReport(false)
	preview, pinnable := (&MigrateCmd{}).previewDesiredPlan(additive)
	assert.Empty(t, pinnable)
	assert.Equal(t, additive, preview, "a plan that drops nothing is previewed as it is")
}

// A refused destructive preview in text ends with the way through: the
// fingerprint to pin and the table to acknowledge. JSON stays one document
// and carries the pin as pinnable_fingerprint.
func TestWriteDesiredPreviewNamesTheWayThrough(t *testing.T) {
	report := desiredReport(true)

	var text strings.Builder
	require.ErrorIs(t, (&MigrateCmd{}).writeDesiredPreview(&text, report), verdict.ErrRefused)
	assert.Contains(t, text.String(), "to run this plan, add to the command:\n  --expect-fingerprint "+report.Fingerprint+" --allow-destructive app.orders\n")

	var js strings.Builder
	require.ErrorIs(t, (&MigrateCmd{JSON: true}).writeDesiredPreview(&js, report), verdict.ErrRefused)
	assert.NotContains(t, js.String(), "--expect-fingerprint")
	var got plan.Report
	require.NoError(t, json.Unmarshal([]byte(js.String()), &got))
	assert.Equal(t, report.Fingerprint, got.PinnableFingerprint)
	assert.Equal(t, got.ComputeFingerprint(), got.Fingerprint, "the refused preview's fingerprint is its own")

	pinned := &MigrateCmd{AllowDestructive: "app.orders", ExpectFingerprint: report.Fingerprint}
	for _, asJSON := range []bool{false, true} {
		var out strings.Builder
		pinned.JSON = asJSON
		require.NoError(t, pinned.writeDesiredPreview(&out, report))
		assert.NotContains(t, out.String(), "a run refuses a plan")
		assert.NotContains(t, out.String(), "pinnable_fingerprint")
		assert.Contains(t, out.String(), report.Fingerprint)
	}
}

// A dry run compares the pin and the acknowledged table as the run does,
// before it renders anything: a pin for another plan is the run's own
// refusal, and a table other than the plan's is a usage error, whatever the
// plan, in text and in JSON.
func TestWriteDesiredPreviewChecksThePinAndTheTable(t *testing.T) {
	converged := desiredReport(false)
	converged.Statements = nil
	converged.Fingerprint = converged.ComputeFingerprint()
	reports := map[string]plan.Report{
		"a destructive plan":        desiredReport(true),
		"a plan that drops nothing": desiredReport(false),
		"a converged table":         converged,
	}

	for name, report := range reports {
		for _, asJSON := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				var out strings.Builder
				c := &MigrateCmd{AllowDestructive: "app.other", ExpectFingerprint: report.Fingerprint, JSON: asJSON}
				err := c.writeDesiredPreview(&out, report)
				require.ErrorIs(t, err, migrate.ErrAllowDestructiveMismatch)
				assert.NotErrorIs(t, err, verdict.ErrRefused, "a usage error, not a refusal")
				assert.Empty(t, out.String(), "nothing is rendered")
			})
		}
	}

	for _, name := range []string{"a destructive plan", "a plan that drops nothing"} {
		report := reports[name]
		t.Run("stale pin on "+name, func(t *testing.T) {
			var text strings.Builder
			c := &MigrateCmd{ExpectFingerprint: "sha256:older"}
			require.ErrorIs(t, c.writeDesiredPreview(&text, report), verdict.ErrRefused)
			assert.Contains(t, text.String(), "refused ("+string(verdict.ReasonPlanFingerprintMismatch)+")")
			assert.Contains(t, text.String(), "is not the pinned plan (fingerprint sha256:older)")
			assert.Contains(t, text.String(), report.Fingerprint)
			assert.NotContains(t, text.String(), "diff:", "a stale pin previews no plan")

			var js strings.Builder
			c.JSON = true
			require.ErrorIs(t, c.writeDesiredPreview(&js, report), verdict.ErrRefused)
			assert.Contains(t, js.String(), `"reason": "`+string(verdict.ReasonPlanFingerprintMismatch)+`"`)
		})
	}

	var out strings.Builder
	additive := reports["a plan that drops nothing"]
	require.NoError(t, (&MigrateCmd{ExpectFingerprint: additive.Fingerprint}).writeDesiredPreview(&out, additive))
	assert.Contains(t, out.String(), "fingerprint:\n  "+additive.Fingerprint, "a pin for the plan previews it")
}
