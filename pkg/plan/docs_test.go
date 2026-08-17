package plan_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
)

// planReportDoc is the human-facing contract page these tests keep honest.
const planReportDoc = "../../docs/plan-report.md"

func readDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(planReportDoc)
	require.NoError(t, err)
	return string(raw)
}

// classifyCanonical routes one statement the way both front doors do:
// canonicalize, classify, route.
func classifyCanonical(t *testing.T, sql string) planner.Plan {
	t.Helper()
	canonical, err := statement.Canonical(sql)
	require.NoError(t, err)
	classified, err := planner.Classify(canonical, planner.Facts{})
	require.NoError(t, err)
	return classified
}

// The doc's example reports are generated output, not prose: rebuilding
// them through the real classify-and-route pipeline must reproduce the
// published JSON byte for byte (up to JSON equivalence). If this fails,
// regenerate the examples in docs/plan-report.md.
func TestDocExamplesMatchPipelineOutput(t *testing.T) {
	doc := readDoc(t)
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(doc, -1)
	require.Len(t, blocks, 2, "the doc publishes one example per source")

	alter := plan.NewReport(plan.SourceAlter)
	alter.Schema, alter.Table, alter.ServerVersion = "app", "orders", "16.4"
	alterTableExists := true
	alter.TableExists = &alterTableExists
	routedA := router.Route([]planner.Plan{
		classifyCanonical(t, "ALTER TABLE app.orders DROP COLUMN legacy_status"),
	})
	alter.Disposition = routedA.Disposition
	for _, rs := range routedA.Statements {
		ps, err := plan.FromRouted(rs)
		require.NoError(t, err)
		alter.Statements = append(alter.Statements, ps)
	}
	alter.Fingerprint = plan.Fingerprint(alter.Statements)

	diff := plan.NewReport(plan.SourceDiff)
	diff.Schema, diff.Table, diff.ServerVersion = "app", "orders", "16.4"
	exists := true
	diff.TableExists = &exists
	routedD := router.Route([]planner.Plan{
		classifyCanonical(t, `DROP INDEX "app"."orders_legacy_idx"`),
		classifyCanonical(t, `ALTER TABLE "app"."orders" ADD COLUMN "region" text DEFAULT 'emea'`),
	})
	diff.Disposition = routedD.Disposition
	kinds := []schemadiff.ChangeKind{schemadiff.ChangeDropIndex, schemadiff.ChangeAddColumn}
	for i, rs := range routedD.Statements {
		ps, err := plan.FromRouted(rs)
		require.NoError(t, err)
		ps.Kind = kinds[i]
		diff.Statements = append(diff.Statements, ps)
	}
	diff.Fingerprint = plan.Fingerprint(diff.Statements)

	for i, want := range []plan.Report{alter, diff} {
		raw, err := json.Marshal(want)
		require.NoError(t, err)
		assert.JSONEq(t, blocks[i][1], string(raw),
			"docs/plan-report.md example %d drifted from pipeline output", i+1)
	}
}

// Every vocabulary value the contract closes over must be documented: a
// constant added to the code without a row in docs/plan-report.md (and a
// format_version decision) fails here.
func TestDocListsEveryVocabularyValue(t *testing.T) {
	doc := readDoc(t)
	var values []string
	for _, s := range plan.Sources() {
		values = append(values, string(s))
	}
	for _, r := range planner.Routes() {
		values = append(values, string(r))
	}
	for _, r := range planner.Reasons() {
		values = append(values, string(r))
	}
	for _, e := range planner.Executions() {
		values = append(values, string(e))
	}
	for _, b := range router.Backends() {
		values = append(values, string(b))
	}
	for _, d := range router.Dispositions() {
		values = append(values, string(d))
	}
	for _, k := range schemadiff.ChangeKinds() {
		values = append(values, string(k))
	}
	for _, g := range suggest.Guidances() {
		values = append(values, string(g))
	}
	for _, v := range values {
		assert.Contains(t, doc, fmt.Sprintf("| `%s` |", v),
			"docs/plan-report.md is missing a vocabulary row for %q", v)
	}
}

// The doc's stated current version is the constant, not prose that can
// drift: a FormatVersion bump without the matching doc sentence fails here.
func TestDocStatesCurrentFormatVersion(t *testing.T) {
	doc := readDoc(t)
	assert.Contains(t, doc, fmt.Sprintf("The current version is **%d**", plan.FormatVersion),
		"docs/plan-report.md's stated version drifted from plan.FormatVersion")
}
