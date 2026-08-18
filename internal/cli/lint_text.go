package cli

import (
	"fmt"
	"io"

	"github.com/block/pg-sprite/pkg/lint"
)

// writeLintText renders the findings in the same compiler-diagnostic
// grammar as the dry-run and diff reports: the flagged statement leads
// each group under the conventional name:line:column: label so a reader
// can jump to the source, each finding is a labeled
// severity[code]: entry beneath it, and the report closes with a lint
// summary. Display only — the JSON report is the machine contract; the
// codes are the same typed values the JSON report carries. A clean report
// prints nothing.
func writeLintText(out io.Writer, pal palette, name string, report lint.Report) error {
	if len(report.Findings) == 0 {
		return nil
	}
	w := &stickyWriter{out: out, pal: pal}
	statement := 0
	var docs []string
	for _, f := range report.Findings {
		if f.Statement != statement {
			writeDocs(w, docs)
			docs = nil
			w.entry(fmt.Sprintf("%s:%d:%d:", name, f.Line, f.Column))
			w.printf("  %s;\n", f.SQL)
			statement = f.Statement
		}
		w.diag(string(f.Severity), string(f.Code), lintMessage(f))
		if f.Code == lint.CodePossibleTableRewrite {
			w.diag("note", "", "the table was not introspected, so this is the conservative classification; with live facts the same change may classify as cheaper")
		}
		if len(f.Suggestion) > 0 {
			w.diag("help", "", "a safer online form exists — not a semantic equivalent, and running it by hand forgoes the engine's execution-time guards:")
			for n, sql := range f.Suggestion {
				w.printf("  %d. %s;\n", n+1, sql)
			}
			w.diag("note", "", "run each statement in its own transaction, never one block; after a failed CONCURRENTLY build, check pg_index.indisvalid and rebuild")
		}
		docs = appendCode(docs, onlineDDLReferenceURL+"#"+lintDocCode(f))
	}
	writeDocs(w, docs)
	w.entry("lint:")
	w.printf("  %s — %s, %s, %s\n", name,
		countNoun(len(report.Findings), "finding"),
		countNoun(report.Errors, "error"), countNoun(report.Warnings, "warning"))
	return w.err
}

// lintMessage renders one finding's diagnostic message: the flagged
// operation and what running it as written does to the table. Classifier
// findings carry the planner's typed reason, so the impact prose is the
// same one the dry-run report renders for that reason; a destructive
// finding has no reason and states the loss directly.
func lintMessage(f lint.Finding) string {
	if f.Code == lint.CodeDestructive {
		return f.Operation + " — discards live data or structure"
	}
	if f.Reason != "" {
		return fmt.Sprintf("%s — %s", f.Operation, impactText(f.Reason))
	}
	return f.Operation
}

// lintDocCode picks the reference-doc anchor for one finding: the
// planner's reason code when the finding carries one (the anchors in
// docs/postgres-online-ddl-reference.md are keyed by reason, matching the
// dry-run report), and the finding's own code otherwise.
func lintDocCode(f lint.Finding) string {
	if f.Reason != "" {
		return string(f.Reason)
	}
	return string(f.Code)
}
