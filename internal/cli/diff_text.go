package cli

import (
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/router"
)

// writeDiffText renders the derived convergence plan in the same
// compiler-diagnostic grammar as the dry-run report: one labeled entry per
// finding, the typed reason as the rule code, doc anchors, and a closing
// plan summary. Display only — the JSON report is the machine contract, and
// --sql prints the plan as an executable script (writePlanText). The
// framing differs from the dry-run report where the semantics differ: diff
// never executes, so execution is routed through the migrate front door,
// and a missing table is the greenfield case — the plan creates the table
// from the full desired schema — not an error.
func writeDiffText(out io.Writer, report plan.Report) error {
	w := &stickyWriter{out: out}
	if tableMissing(report) {
		w.diag("note", "", fmt.Sprintf("the table %s.%s does not exist — the plan creates it from the full desired schema", report.Schema, report.Table))
	}
	if len(report.Statements) == 0 {
		w.entry("plan:")
		w.printf("  %s — no changes; the live table matches the desired schema\n", targetText(report))
		return w.err
	}
	steps, refused := 0, 0
	for i, ps := range report.Statements {
		s, r := writeStatementDiagnostics(w, i+1, ps, "pg-sprite migrate")
		steps += s
		refused += r
	}
	w.entry("plan:")
	w.printf("  %s — %s, %s to run, %d refused\n", targetText(report),
		countNoun(len(report.Statements), "statement"), countNoun(steps, "step"), refused)
	w.entry("diff:")
	w.printf("  nothing was executed\n")
	w.entry("sql:")
	w.printf("  re-run with --sql to print the plan as an executable SQL script\n")
	if refused == 0 && steps > 0 {
		w.entry("apply:")
		w.printf("  run each statement via pg-sprite migrate --alter '…', which refuses\n")
		w.printf("  blocking forms and substitutes safer online sequences\n")
	}
	return w.err
}

// diffRefused reports whether the derived plan contains any statement
// execution would refuse. The diff exits with the refusal code in that
// case — the same contract the dry run uses — so CI can gate on the diff
// without parsing the report. A missing table does not refuse: for diff it
// is the greenfield case, and the plan is the full desired schema.
func diffRefused(report plan.Report) bool {
	return report.Disposition != router.DispositionExecute
}
