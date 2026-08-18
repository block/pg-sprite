package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/block/pg-sprite/pkg/plan"
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/verdict"
)

// suggestReportURL is where a rewrite-required refusal's typed guidance
// codes are documented; the docs section links it alongside the diagnostic
// code anchors.
const suggestReportURL = "https://github.com/block/pg-sprite/blob/main/docs/suggest-report.md"

// dryRunTextWidth is the column diagnostic prose wraps at, indent included.
// SQL, URLs, and the plan summary are never wrapped: a statement must stay
// greppable as one line.
const dryRunTextWidth = 74

// writeDryRunText renders the routed dry-run plan in the compiler-diagnostic
// grammar, one labeled entry per finding: the label (severity[code]:) on its
// own line, the content indented beneath it, a blank line between entries.
// The typed reason is the rule code, each code gets a doc anchor, and the
// report closes with a plan summary. Display only — the JSON report is the
// machine contract. The codes are the same typed values the JSON report
// carries, so the prose cross-references automation and
// docs/postgres-online-ddl-reference.md#dry-run-diagnostic-codes.
func writeDryRunText(out io.Writer, report plan.Report) error {
	w := &stickyWriter{out: out}
	steps, refused := 0, 0
	for i, ps := range report.Statements {
		s, r := writeStatementDiagnostics(w, i+1, ps, "pg-sprite")
		steps += s
		refused += r
	}
	if tableMissing(report) {
		w.diag("error", "table-not-found",
			fmt.Sprintf("the target table %s.%s does not exist — classification fell back to zero facts (the conservative worst case), and running without --dry-run would fail", report.Schema, report.Table))
		w.entry("docs:")
		w.printf("  %s#table-not-found\n", onlineDDLReferenceURL)
	}
	w.entry("plan:")
	w.printf("  %s — %s, %s to run, %d refused\n", targetText(report),
		countNoun(len(report.Statements), "statement"), countNoun(steps, "step"), refused)
	w.entry("dry-run:")
	w.printf("  nothing was executed\n")
	if refused == 0 && steps > 0 && !tableMissing(report) {
		w.entry("apply:")
		w.printf("  re-run without --dry-run\n")
	}
	return w.err
}

// writeStatementDiagnostics renders one plan statement's labeled entries —
// the statement itself, its refusal, its classifications, guidance, the
// substitution notes, and the docs anchors — and returns the execution
// steps it contributes and how many refusal codes it raised (at most one).
// runner names what executes the plan in the substitution note: the dry-run
// report speaks as the invocation itself ("pg-sprite" — re-run without
// --dry-run), while the diff report never executes and routes execution
// through the migrate front door ("pg-sprite migrate").
func writeStatementDiagnostics(w *stickyWriter, n int, ps plan.Statement, runner string) (steps, refused int) {
	w.entry(fmt.Sprintf("statement %d:", n))
	w.printf("  %s;\n", ps.SQL)
	codes := writeRefusal(w, ps)
	refused = len(codes) // at most one refusal code per statement
	for _, d := range ps.Decisions {
		w.diag(decisionSeverity(ps, d), string(d.Reason),
			fmt.Sprintf("%s — %s", d.Operation, impactText(d.Reason)))
		codes = appendCode(codes, string(d.Reason))
	}
	if ps.Destructive {
		w.diag("warning", "destructive", "this change discards live data or structure")
		codes = appendCode(codes, "destructive")
	}
	if unverifiedDecision(ps) {
		w.diag("note", "", "the table was not introspected, so this is the conservative classification; with live facts the same change may classify as cheaper")
	}
	if ps.Guidance != "" {
		// The manual path trails the findings that explain it —
		// compiler style, where help: follows the diagnosis. help is
		// reserved for steps the user runs; a sequence pg-sprite runs
		// itself is a note.
		w.diag("help", string(ps.Guidance), guidanceText(ps.Guidance))
	}
	if ps.Disposition == router.DispositionExecute {
		if substituted(ps) {
			w.diag("note", "", runner+" will run a safer online sequence instead:")
			for n, sql := range ps.ExecSQL {
				w.printf("  %d. %s;\n", n+1, sql)
			}
			if len(ps.ExecSQL) > 1 {
				w.diag("note", "", "each step commits on its own — not transactionally equivalent, and the sequence must not run inside a transaction block")
			} else {
				w.diag("note", "", "the substituted statement commits on its own and must not run inside a transaction block")
			}
		} else {
			w.diag("note", "", "runs as written")
		}
		steps = len(ps.ExecSQL)
	}
	if len(codes) > 0 {
		w.entry("docs:")
		for _, c := range codes {
			w.printf("  %s#%s\n", onlineDDLReferenceURL, c)
		}
		if ps.Guidance != "" {
			w.printf("  %s#%s\n", suggestReportURL, ps.Guidance)
		}
	}
	return steps, refused
}

// tableMissing reports whether the plan's target table was introspected
// and found absent. Unset means not introspected (no single table target),
// which is not a missing table.
func tableMissing(report plan.Report) bool {
	return report.TableExists != nil && !*report.TableExists
}

// writeRefusal emits the leading error diagnostic for a statement execution
// would not run, and returns its rule code; an executable statement emits
// nothing. The refusal leads the statement's diagnostics — compiler style:
// the blocking finding first, its context as notes after.
func writeRefusal(w *stickyWriter, ps plan.Statement) []string {
	switch ps.Disposition {
	case router.DispositionExecute:
		return nil
	case router.DispositionRewriteRequired:
		w.diag("error", "rewrite-required", "refused — blocks as written and no online replacement could be constructed; rewrite the change as separate online steps and dry-run each one")
		return []string{"rewrite-required"}
	case router.DispositionUnavailable:
		w.diag("error", "backend-unavailable", fmt.Sprintf("refused — needs the %s backend (an online shadow-table copy with a cutover), which this build does not implement yet", ps.Backend))
		return []string{"backend-unavailable"}
	case router.DispositionRefuse:
		reason := ps.Reason
		if reason == verdict.ReasonNone {
			// A planner-level refusal carries no target-fact reason;
			// report the same typed token the run path's refusal verdict
			// uses so both front doors name the refusal identically.
			reason = verdict.ReasonUnsupportedStatement
		}
		w.diag("error", string(reason), "refused — "+refuseText(ps, reason))
		return []string{string(reason)}
	}
	// An unrecognized disposition is outside this build's contract: refuse
	// loudly rather than guessing what execution would do. No docs entry —
	// a code this build does not know has no anchor to link.
	w.diag("error", "unknown-disposition", fmt.Sprintf("refused — unrecognized disposition %q; treat the statement as not executable", ps.Disposition))
	return nil
}

// decisionSeverity maps one operation's classification to its diagnostic
// severity: note when the submitted form is safe to run, warning when it is
// unsafe or app-breaking as written, and note on a refused statement, where
// the leading error diagnostic already carries the severity.
func decisionSeverity(ps plan.Statement, d planner.Decision) string {
	if ps.Disposition != router.DispositionExecute {
		return "note"
	}
	if d.Reason == planner.ReasonSaferIdiom || d.Reason == planner.ReasonAppBreakingRename {
		return "warning"
	}
	return "note"
}

// appendCode appends a rule code if it is not already present, preserving
// first-seen order so docs lines match diagnostic order.
func appendCode(codes []string, code string) []string {
	if slices.Contains(codes, code) {
		return codes
	}
	return append(codes, code)
}

// writeDocs emits the docs: entry linking each collected reference URL,
// one per line in first-seen order; no entry when nothing was collected.
func writeDocs(w *stickyWriter, urls []string) {
	if len(urls) == 0 {
		return
	}
	w.entry("docs:")
	for _, u := range urls {
		w.printf("  %s\n", u)
	}
}

// unverifiedDecision reports whether any decision was taken without the
// live facts needed to prove a cheaper route.
func unverifiedDecision(ps plan.Statement) bool {
	for _, d := range ps.Decisions {
		if d.Unverified {
			return true
		}
	}
	return false
}

// countNoun renders a count with its singular or plural noun.
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// targetText names what the plan was derived against: the resolved table
// when the statement has one, and the server version the rules ran under.
func targetText(report plan.Report) string {
	var b strings.Builder
	if report.Table != "" {
		fmt.Fprintf(&b, "%s.%s", report.Schema, report.Table)
	} else {
		b.WriteString("no single table target")
	}
	if report.ServerVersion != "" {
		fmt.Fprintf(&b, " (PostgreSQL %s)", displayVersion(report.ServerVersion))
	}
	return b.String()
}

// displayVersion trims a server_version banner ("16.14 (Debian …)") to the
// bare version number for the summary line; the JSON report carries the
// full banner.
func displayVersion(v string) string {
	bare, _, _ := strings.Cut(v, " ")
	return bare
}

// substituted reports whether execution would run something other than the
// submitted statement itself.
func substituted(ps plan.Statement) bool {
	return len(ps.ExecSQL) > 0 && (len(ps.ExecSQL) != 1 || ps.ExecSQL[0] != ps.SQL)
}

// impactText renders what running an operation's submitted form does to
// the table, keyed by the planner's typed reason. An unknown reason renders
// as itself: the typed value is the contract, the prose is display only.
// The wording claims only what holds for every operation the reason covers;
// per-operation lock detail lives in docs/postgres-online-ddl-reference.md.
func impactText(r planner.Reason) string {
	switch r {
	case planner.ReasonMetadataOnly:
		return "a brief catalog-only change; takes a short exclusive lock but does not scan or rewrite the table"
	case planner.ReasonOnlineIdiom:
		return "already the safe online form; does not block the table while it runs"
	case planner.ReasonFastDefault:
		return "catalog-only (fast default); no table scan or rewrite"
	case planner.ReasonBinaryCoercible:
		return "PostgreSQL relabels the type in place; no table rewrite"
	case planner.ReasonSaferIdiom:
		return "holds a blocking lock on the table for the whole operation — writes (and for some forms reads) wait until it finishes"
	case planner.ReasonAppBreakingRename:
		return "the rename itself is a brief catalog change, but every query still using the old name fails the moment it commits"
	case planner.ReasonVolatileDefault:
		return "the default must be computed per row: a full table rewrite under an exclusive lock that blocks reads and writes"
	case planner.ReasonGeneratedStored:
		return "computing the stored column forces a full table rewrite under an exclusive lock that blocks reads and writes"
	case planner.ReasonTypeRewrite:
		return "the type conversion forces a full table rewrite under an exclusive lock that blocks reads and writes"
	case planner.ReasonRelocation:
		return "physically moves the table's storage: rewrite-scale I/O under an exclusive lock that blocks reads and writes"
	case planner.ReasonPartitionParentLock:
		return "takes a brief exclusive lock on the partitioned parent, briefly blocking access to every partition"
	case planner.ReasonUnsupportedOperation:
		return "pg-sprite does not recognize this operation and will not run it"
	}
	return string(r)
}

// verdictText renders the target-fact refusal cause a dry run can carry.
// An unknown reason renders as itself: the typed value is the contract,
// the prose is display only.
func verdictText(r verdict.Reason) string {
	if r == verdict.ReasonUnsupportedPartitionedParent {
		return "the target is a partitioned table and this operation is not supported on partitioned parents"
	}
	return string(r)
}

// refuseText renders a refusal's cause. A planner-level refusal
// (unsupported-statement) names the refused operation when the decisions
// carry one, mirroring the run path's refusal verdict detail; target-fact
// refusals render their verdictText.
func refuseText(ps plan.Statement, r verdict.Reason) string {
	if r != verdict.ReasonUnsupportedStatement {
		return verdictText(r)
	}
	if op := refusedOperation(ps); op != "" {
		return "the planner knows no safe path for " + op
	}
	return "the planner knows no safe path for this statement"
}

// refusedOperation names the first operation the planner refused — the
// same operation the run path's refusal verdict names in its detail.
func refusedOperation(ps plan.Statement) string {
	for _, d := range ps.Decisions {
		if d.Route == planner.RouteRefuse {
			return d.Operation
		}
	}
	return ""
}

// stickyWriter accumulates the first write error so the renderer reads as
// layout, not error plumbing.
type stickyWriter struct {
	out     io.Writer
	err     error
	started bool
}

func (w *stickyWriter) printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	if _, err := fmt.Fprintf(w.out, format, args...); err != nil {
		w.err = fmt.Errorf("write plan: %w", err)
	}
}

// entry starts a labeled entry: the label alone on its own line, a blank
// line separating it from the previous entry. The caller writes the entry's
// content indented beneath it.
func (w *stickyWriter) entry(label string) {
	if w.started {
		w.printf("\n")
	}
	w.started = true
	w.printf("%s\n", label)
}

// diag writes one diagnostic entry: the "severity[code]:" label (or
// "severity:" when there is no code) on its own line, the message wrapped
// at dryRunTextWidth and indented two spaces beneath it.
func (w *stickyWriter) diag(severity, code, message string) {
	label := severity + ":"
	if code != "" {
		label = fmt.Sprintf("%s[%s]:", severity, code)
	}
	w.entry(label)
	for _, line := range wrapWords(message, dryRunTextWidth-2) {
		w.printf("  %s\n", line)
	}
}

// wrapWords greedily wraps text into lines of at most width columns,
// breaking only between words. A word longer than width gets its own
// overlong line rather than being split.
func wrapWords(text string, width int) []string {
	var lines []string
	var line strings.Builder
	lineLen := 0
	for word := range strings.FieldsSeq(text) {
		wordLen := utf8.RuneCountInString(word)
		if lineLen > 0 && lineLen+1+wordLen > width {
			lines = append(lines, line.String())
			line.Reset()
			lineLen = 0
		}
		if lineLen > 0 {
			line.WriteByte(' ')
			lineLen++
		}
		line.WriteString(word)
		lineLen += wordLen
	}
	if lineLen > 0 {
		lines = append(lines, line.String())
	}
	return lines
}
