package cli

import (
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
)

// invalidIndexRunbookURL is where an advisory about an unfinished index
// build sends the operator: the runbook that proves an entry abandoned
// before removing it.
const invalidIndexRunbookURL = "https://github.com/block/pg-sprite/blob/main/docs/invalid-index-recovery.md"

// writeDiffText renders the derived convergence plan in the same
// compiler-diagnostic grammar as the dry-run report: one labeled entry per
// finding, the typed reason as the rule code, doc anchors, and a closing
// plan summary. Display only — the JSON report is the machine contract, and
// --sql prints the plan as an executable script (writePlanText). The
// framing differs from the dry-run report where the semantics differ: diff
// never executes, so execution is routed through the migrate front door,
// and a missing table is the greenfield case — the plan creates the table
// from the full desired schema — not an error, unless the create path
// refuses a statement's shape, in which case the leading note says so
// instead of promising a create the refusal beneath it withdraws. A
// greenfield statement the create path refused by shape carries the typed
// cause on the plan, and the renderer prints its description as a trailing
// note on the refused statement — the executor's explanation, which the
// typed reason alone does not carry. The plan's advisories — the invalid
// indexes the diff observed and declined to drop — render as warnings after
// the statements, with the typed kind as the rule code, so an empty plan
// over a table carrying debris never reads as converged.
func writeDiffText(out io.Writer, pal palette, report plan.Report) error {
	w := &stickyWriter{out: out, pal: pal}
	if tableMissing(report) {
		if report.Disposition == router.DispositionExecute {
			w.diag("note", "", fmt.Sprintf("the table %s.%s does not exist — the plan creates it from the full desired schema", report.Schema, report.Table))
		} else {
			w.diag("note", "", fmt.Sprintf("the table %s.%s does not exist — the plan is the full desired schema, and a statement in it is refused, so nothing would be created", report.Schema, report.Table))
		}
	}
	if len(report.Statements) == 0 {
		writeAdvisoryDiagnostics(w, report.Advisories)
		w.entry("plan:")
		if len(report.Advisories) == 0 {
			w.printf("  %s — no changes; the live table matches the desired schema\n", targetText(report))
		} else {
			w.printf("  %s — no changes to run; the live table carries %s the plan leaves alone\n",
				targetText(report), countNoun(len(report.Advisories), "unfinished index build"))
		}
		return w.err
	}
	steps, refused := 0, 0
	for i, ps := range report.Statements {
		s, r := writeStatementDiagnostics(w, i+1, ps, "pg-sprite migrate")
		steps += s
		refused += r
		if ps.Cause != "" {
			w.diag("note", "", "the create path refuses this statement: "+ps.Cause.Description())
		}
	}
	writeAdvisoryDiagnostics(w, report.Advisories)
	w.entry("plan:")
	w.printf("  %s — %s, %s to run, %d refused\n", targetText(report),
		countNoun(len(report.Statements), "statement"), countNoun(steps, "step"), refused)
	w.entry("diff:")
	w.printf("  nothing was executed\n")
	w.entry("sql:")
	w.printf("  re-run with --sql to print the plan as an executable SQL script\n")
	// No apply pointer for a greenfield plan: migrate changes an existing
	// table and refuses CREATE TABLE, so pointing the reader at it would
	// send them in a circle — the leading note and the --sql pointer are
	// the honest route.
	if refused == 0 && steps > 0 && !tableMissing(report) {
		w.entry("apply:")
		w.printf("  run each statement via pg-sprite migrate --alter '…', which refuses\n")
		w.printf("  blocking forms and substitutes safer online sequences\n")
	}
	return w.err
}

// writeAdvisoryDiagnostics renders each plan advisory as a warning whose
// rule code is the typed kind, followed by one docs pointer at the runbook.
// A warning, not an error: the plan stays executable and the diff's exit
// code is unchanged — the operator is being told what the plan will not do.
func writeAdvisoryDiagnostics(w *stickyWriter, advisories []schemadiff.Advisory) {
	if len(advisories) == 0 {
		return
	}
	for _, a := range advisories {
		w.diag("warning", string(a.Kind), fmt.Sprintf("index %q %s", a.Index, a.Kind.Description()))
	}
	w.entry("docs:")
	w.printf("  %s\n", invalidIndexRunbookURL)
}

// diffRefused reports whether the derived plan contains any statement
// execution would refuse. The diff exits with the refusal code in that
// case — the same contract the dry run uses — so CI can gate on the diff
// without parsing the report. A missing table does not refuse: for diff it
// is the greenfield case, and the plan is the full desired schema.
func diffRefused(report plan.Report) bool {
	return report.Disposition != router.DispositionExecute
}
