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
// from the full desired schema — not an error, unless the create path
// refuses a statement's shape, in which case the leading note says so
// instead of promising a create the refusal beneath it withdraws. causes
// is positional with report.Statements: the create path's typed refusal
// for a greenfield statement it refused by shape, nil elsewhere. The plan
// report has no field for it, so the caller recomputes it
// (greenfieldRefusalCauses) and the renderer prints it as a trailing note
// on the refused statement — the executor's explanation, which the typed
// reason alone does not carry.
func writeDiffText(out io.Writer, pal palette, report plan.Report, causes []error) error {
	w := &stickyWriter{out: out, pal: pal}
	if tableMissing(report) {
		if report.Disposition == router.DispositionExecute {
			w.diag("note", "", fmt.Sprintf("the table %s.%s does not exist — the plan creates it from the full desired schema", report.Schema, report.Table))
		} else {
			w.diag("note", "", fmt.Sprintf("the table %s.%s does not exist — the plan is the full desired schema, and a statement in it is refused, so nothing would be created", report.Schema, report.Table))
		}
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
		if i < len(causes) && causes[i] != nil {
			w.diag("note", "", "the create path refuses this statement: "+causes[i].Error())
		}
	}
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

// diffRefused reports whether the derived plan contains any statement
// execution would refuse. The diff exits with the refusal code in that
// case — the same contract the dry run uses — so CI can gate on the diff
// without parsing the report. A missing table does not refuse: for diff it
// is the greenfield case, and the plan is the full desired schema.
func diffRefused(report plan.Report) bool {
	return report.Disposition != router.DispositionExecute
}
