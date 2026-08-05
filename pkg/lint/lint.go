// Package lint checks DDL offline for patterns the engine would refuse,
// rewrite, or gate. It runs the same parse-and-classify pipeline as the
// front doors but with zero live facts, so it needs no database and is
// strictly conservative: a change lint passes without findings can still
// sharpen at execution time, but a change lint flags will never quietly
// get worse. Findings carry typed codes automation branches on — never
// prose.
package lint

import (
	"fmt"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/statement"
)

// FormatVersion identifies the report contract. A consumer must reject a
// report whose version it does not understand instead of guessing at the
// field semantics.
const FormatVersion = 1

// Severity ranks a finding by what the engine would do with it.
type Severity string

// The severities a finding can carry.
const (
	// SeverityError: the engine would refuse the statement — it cannot
	// execute as written.
	SeverityError Severity = "error"
	// SeverityWarning: the engine would execute the statement, but it has
	// a safer form, needs a heavier path, or discards live structure.
	SeverityWarning Severity = "warning"
)

// Code is the typed finding kind; automation branches on it, never on
// prose.
type Code string

// The finding codes.
const (
	// CodeUnsupportedOperation: no known safe path — the engine refuses it.
	CodeUnsupportedOperation Code = "unsupported-operation"
	// CodeBlockingIdiom: the submitted form blocks readers or writers and
	// a safer native form exists; Suggestion carries it when the linter
	// can construct one.
	CodeBlockingIdiom Code = "blocking-idiom"
	// CodeTableRewrite: the operation needs a full table rewrite — only
	// the engine's copy-and-swap path can run it online. Reason carries
	// the specific cause.
	CodeTableRewrite Code = "table-rewrite"
	// CodeDestructive: the operation discards live structure (a column or
	// constraint drop) and cannot be undone by re-running the schema.
	CodeDestructive Code = "destructive"
)

// Finding is one lint result: the statement it is about, the typed code,
// and the severity.
type Finding struct {
	// Statement is the 1-based index of the statement in the script.
	Statement int `json:"statement"`
	// SQL is the canonical text of that statement.
	SQL string `json:"sql"`
	// Operation is the operator-facing label of the flagged operation
	// (display only).
	Operation string `json:"operation"`
	// Code is the typed finding kind.
	Code Code `json:"code"`
	// Severity is what the engine would do about it.
	Severity Severity `json:"severity"`
	// Reason is the classifier's typed cause, present for findings the
	// classifier produced (blocking-idiom, table-rewrite, unsupported).
	Reason planner.Reason `json:"reason,omitempty"`
	// Suggestion is the ordered safer SQL to run instead, present only
	// for blocking-idiom findings where the linter could construct it.
	Suggestion []string `json:"suggestion,omitempty"`
}

// Report is the lint result for one script.
type Report struct {
	// FormatVersion is the report contract version; always FormatVersion.
	FormatVersion int `json:"format_version"`
	// Findings are the results in statement order; empty means the script
	// is clean.
	Findings []Finding `json:"findings"`
	// Errors counts error-severity findings.
	Errors int `json:"errors"`
	// Warnings counts warning-severity findings.
	Warnings int `json:"warnings"`
}

// Check lints a DDL script: every statement is parsed with the PostgreSQL
// grammar and classified with zero live facts. A parse failure is an
// error; an unsupported operation is not — it is an error-severity
// finding, so one bad statement never hides the rest of the report.
func Check(sql string) (Report, error) {
	stmts, err := statement.Split(sql)
	if err != nil {
		return Report{}, err
	}
	report := Report{FormatVersion: FormatVersion, Findings: []Finding{}}
	for i, stmt := range stmts {
		findings, err := checkStatement(i+1, stmt)
		if err != nil {
			return Report{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		report.Findings = append(report.Findings, findings...)
	}
	for _, f := range report.Findings {
		if f.Severity == SeverityError {
			report.Errors++
		} else {
			report.Warnings++
		}
	}
	return report, nil
}

// checkStatement produces the findings for one statement: classifier
// findings from its routing decisions plus destructive findings from its
// parsed operations.
func checkStatement(index int, sql string) ([]Finding, error) {
	classified, err := planner.Classify(sql, planner.Facts{})
	if err != nil {
		return nil, err
	}
	ops, err := statement.ParseOps(sql)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, d := range classified.Decisions {
		if f, flagged := decisionFinding(d); flagged {
			f.Statement, f.SQL = index, sql
			findings = append(findings, f)
		}
	}
	for _, op := range ops {
		if isDestructive(op) {
			findings = append(findings, Finding{
				Statement: index,
				SQL:       sql,
				Operation: op.Describe(),
				Code:      CodeDestructive,
				Severity:  SeverityWarning,
			})
		}
	}
	return findings, nil
}

// decisionFinding maps one routing decision to its finding. Decisions that
// are already the safe native form produce no finding.
func decisionFinding(d planner.Decision) (Finding, bool) {
	f := Finding{Operation: d.Operation, Reason: d.Reason}
	switch d.Route {
	case planner.RouteRefuse:
		f.Code, f.Severity = CodeUnsupportedOperation, SeverityError
		return f, true
	case planner.RouteCopyAndSwap:
		f.Code, f.Severity = CodeTableRewrite, SeverityWarning
		return f, true
	case planner.RouteNative:
		if d.Reason == planner.ReasonSaferIdiom {
			f.Code, f.Severity = CodeBlockingIdiom, SeverityWarning
			f.Suggestion = d.SaferSQL
			return f, true
		}
		return Finding{}, false
	default:
		// An unknown route is a planner contract violation; fail closed
		// as a refusal rather than passing it silently.
		f.Code, f.Severity = CodeUnsupportedOperation, SeverityError
		return f, true
	}
}

// isDestructive reports whether an operation discards live structure. The
// definition matches the declarative differ's: column and constraint
// drops. An index drop is not destructive — the index is recreatable from
// the schema.
func isDestructive(op statement.Op) bool {
	return op.Kind == statement.OpDropColumn || op.Kind == statement.OpDropConstraint
}
