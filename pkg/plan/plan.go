// Package plan defines the machine-readable dry-run plan report: the
// stable JSON contract an operator or orchestrator consumes to decide
// whether and how a change would execute. Both front doors emit it — the
// imperative migrate --dry-run path and the declarative diff path — so a
// consumer parses one shape regardless of how the plan was derived.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"

	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
	"github.com/block/pg-sprite/pkg/schemadiff"
	"github.com/block/pg-sprite/pkg/statement"
	"github.com/block/pg-sprite/pkg/suggest"
	"github.com/block/pg-sprite/pkg/verdict"
)

// FormatVersion identifies the report contract. A consumer must reject a
// report whose version it does not understand instead of guessing at the
// field semantics. Version 2 added the guidance field on rewrite-required
// statements.
const FormatVersion = 2

// Source identifies which front door derived the plan.
type Source string

const (
	// SourceAlter marks a plan derived from a submitted DDL statement
	// (migrate --alter --dry-run): the classify-and-route pipeline with
	// the diff step skipped.
	SourceAlter Source = "alter"
	// SourceDiff marks a plan derived from a desired-state schema diff
	// (diff --desired): the ordered statements that converge the live
	// table on the desired schema.
	SourceDiff Source = "diff"
)

// Sources returns the closed set of Source values. It is part of the
// plan-report contract (docs/plan-report.md): the set changes only with a
// format_version bump, and a consumer that meets an unrecognized value must
// treat the report as unknown and refuse it.
func Sources() []Source {
	return []Source{SourceAlter, SourceDiff}
}

// Statement is one planned statement: the SQL, its classification, and
// what execution would do with it now.
type Statement struct {
	// SQL is the statement in the engine's canonical rendering: parsed and
	// reprinted through the PostgreSQL deparser, whichever front door
	// derived it. It is never a verbatim echo of the submitted text, so
	// the same change carries the same string through either door.
	SQL string `json:"sql"`
	// Kind classifies a diff-derived statement so a consumer can gate
	// whole classes of change (see schemadiff.ChangeKind). Empty for the
	// alter source: a submitted statement may carry several operations and
	// has no single kind.
	Kind schemadiff.ChangeKind `json:"kind,omitempty"`
	// Destructive marks statements that discard live structure — a dropped
	// column, constraint, index, or NOT NULL. It is derived from the classifier's
	// decisions, so both sources report it identically; it is always
	// emitted, never omitted, because a safety flag a consumer gates on
	// must be explicit even when false.
	Destructive bool `json:"destructive"`
	// Route is the planner's aggregate route for the statement.
	Route planner.Route `json:"route"`
	// Backend is the assigned execution strategy; empty for refusals.
	Backend router.Backend `json:"backend,omitempty"`
	// Disposition is what execution would do with the statement now.
	Disposition router.Disposition `json:"disposition"`
	// Reason is the typed cause when target facts refuse this statement.
	Reason verdict.Reason `json:"reason,omitempty"`
	// Decisions are the planner's per-operation classifications.
	Decisions []planner.Decision `json:"decisions"`
	// ExecSQL is the ordered SQL the native backend would run — the safer
	// sequence when the planner constructed one. Empty for non-native
	// routes.
	ExecSQL []string `json:"exec_sql,omitempty"`
	// Execution is the typed execution contract for ExecSQL
	// (planner.Execution), present exactly when ExecSQL is. A consumer
	// that runs the statements itself branches on it — it is what says
	// each step runs in its own implicit transaction, never inside an
	// enclosing transaction block. It is derived from ExecSQL's presence,
	// so it is excluded from the fingerprint like the other explanatory
	// fields.
	Execution planner.Execution `json:"execution,omitempty"`
	// Guidance is the typed manual path for a rewrite-required refusal,
	// drawn from the suggest contract's Guidance vocabulary
	// (docs/suggest-report.md): the engine will not run the statement, and
	// this names what to do instead. Present exactly when Disposition is
	// rewrite-required. Explanatory, so it is excluded from the
	// fingerprint.
	Guidance suggest.Guidance `json:"guidance,omitempty"`
}

// Report is the dry-run plan for one change against one table.
type Report struct {
	// FormatVersion is the report contract version; always FormatVersion.
	FormatVersion int `json:"format_version"`
	// Source is the front door that derived the plan.
	Source Source `json:"source"`
	// Schema is the target schema; empty when the submitted statement did
	// not qualify one.
	Schema string `json:"schema,omitempty"`
	// Table is the target table; empty when the statement has no single
	// table target (index maintenance).
	Table string `json:"table,omitempty"`
	// ServerVersion is the PostgreSQL server_version the plan was derived
	// against. Classification is version-sensitive, so a stored or
	// forwarded report names the server whose rules produced it; empty
	// only for sources that never connected.
	ServerVersion string `json:"server_version,omitempty"`
	// TableExists reports whether the live table was found. Set by every
	// source that introspects the target (diff, and the alter dry run);
	// nil means the plan has no single table target to introspect. For
	// diff, false means the statements are the full desired schema; for
	// an alter dry run, false means the plan was classified from zero
	// facts and executing it would fail.
	TableExists *bool `json:"table_exists,omitempty"`
	// Disposition is the aggregate disposition across all statements:
	// what would happen if the engine executed this plan now.
	Disposition router.Disposition `json:"disposition"`
	// Reason is the typed refusal cause when target facts make an otherwise
	// executable routed plan unsafe.
	Reason verdict.Reason `json:"reason,omitempty"`
	// Fingerprint is the plan's stable identity (see Fingerprint). An
	// approver pins it when the plan is reviewed; an executor recomputes it
	// at apply time and refuses on mismatch — that is how "the plan a
	// reviewer approves is the plan that executes" is enforced across
	// storage and forwarding.
	Fingerprint string `json:"fingerprint"`
	// Statements is the ordered plan; empty means there is nothing to do.
	Statements []Statement `json:"statements"`
}

// RefuseUnsupportedPartitionedParent marks executable statements as refused
// when partition-aware admission rejects their execution steps.
func RefuseUnsupportedPartitionedParent(report *Report, refused []bool) {
	any := false
	for i := range report.Statements {
		if i >= len(refused) || !refused[i] {
			continue
		}
		any = true
		report.Statements[i].Backend = ""
		report.Statements[i].Disposition = router.DispositionRefuse
		report.Statements[i].Reason = verdict.ReasonUnsupportedPartitionedParent
		report.Statements[i].ExecSQL = nil
		report.Statements[i].Execution = ""
		for j := range report.Statements[i].Decisions {
			report.Statements[i].Decisions[j].SaferSQL = nil
			report.Statements[i].Decisions[j].SaferSQLExecution = ""
		}
	}
	if any {
		report.Disposition = router.DispositionRefuse
		report.Reason = verdict.ReasonUnsupportedPartitionedParent
	}
}

// NewReport returns an empty report for source with the contract version
// stamped and Statements non-nil, so an empty plan serializes as [] rather
// than null.
func NewReport(source Source) Report {
	return Report{
		FormatVersion: FormatVersion,
		Source:        source,
		Statements:    []Statement{},
	}
}

// FromRouted converts one routed statement into a plan statement.
// Destructive is derived from the classifier's decisions — one destructive
// operation makes the statement destructive — so every source that routes
// through the planner reports it identically by construction. A
// rewrite-required statement additionally carries the typed manual path
// (Guidance), derived through the same mapping the suggest report uses; a
// rewrite-required decision with no known guidance is a contract violation
// and fails closed.
func FromRouted(rs router.Statement) (Statement, error) {
	st := Statement{
		SQL:         rs.Statement,
		Route:       rs.Route,
		Backend:     rs.Backend,
		Disposition: rs.Disposition,
		Decisions:   rs.Decisions,
		ExecSQL:     rs.ExecSQL,
	}
	if len(st.ExecSQL) > 0 {
		st.Execution = planner.ExecutionAutocommit
	}
	if st.Disposition == router.DispositionRefuse {
		// A planner-level refusal carries no target-fact reason of its
		// own; stamp the same typed reason the run path's refusal verdict
		// reports, so a dry-run report and a run receipt for the same
		// statement match on the typed field alone.
		st.Reason = verdict.ReasonUnsupportedStatement
	}
	for _, d := range rs.Decisions {
		if d.Destructive {
			st.Destructive = true
			break
		}
	}
	if rs.Disposition == router.DispositionRewriteRequired {
		guidance, err := rewriteRequiredGuidance(rs)
		if err != nil {
			return Statement{}, err
		}
		st.Guidance = guidance
	}
	return st, nil
}

// rewriteRequiredGuidance derives the typed manual path for a statement
// the router marked rewrite-required: the first safer-idiom decision names
// the operation the manual path is for. A multi-operation statement always
// advises splitting first (ManualGuidance's contract), which covers the
// case where the rewrite exists but could not be applied to a compound
// statement. The operation list and decision list are index-aligned by the
// planner's contract (one decision per operation, in order); a mismatch,
// or a rewrite-required statement with no safer-idiom decision, is a
// contract violation and fails closed.
func rewriteRequiredGuidance(rs router.Statement) (suggest.Guidance, error) {
	ops, err := statement.ParseOps(rs.Statement)
	if err != nil {
		return "", fmt.Errorf("parse rewrite-required statement: %w", err)
	}
	if len(ops) != len(rs.Decisions) {
		return "", fmt.Errorf("planner produced %d decisions for %d operations", len(rs.Decisions), len(ops))
	}
	for i, d := range rs.Decisions {
		if d.Reason == planner.ReasonSaferIdiom {
			return suggest.ManualGuidance(ops[i], len(ops) > 1)
		}
	}
	return "", fmt.Errorf("rewrite-required statement %q has no safer-idiom decision", rs.Statement)
}

// Fingerprint computes the plan's stable identity: "sha256:" plus the hex
// digest over what would execute — each statement's canonical SQL, route,
// backend, disposition, and exec_sql, in plan order. Explanatory fields
// (decisions, kind, destructive) are excluded, so a reworded reason does
// not change identity but a rerouted or resequenced plan does. The exact
// serialization is part of the contract (docs/plan-report.md) and changes
// only with a format_version bump. This is a plan identity, not a schema
// fingerprint: it never participates in schema-state comparison.
func Fingerprint(statements []Statement) string {
	h := sha256.New()
	for _, st := range statements {
		writeField(h, st.SQL)
		writeField(h, string(st.Route))
		writeField(h, string(st.Backend))
		writeField(h, string(st.Disposition))
		for _, sql := range st.ExecSQL {
			writeField(h, sql)
		}
		h.Write([]byte{0x1e}) // record separator: one per statement
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// writeField hashes one field with a trailing unit separator, so adjacent
// fields can never collide by concatenation.
func writeField(h hash.Hash, field string) {
	h.Write([]byte(field))
	h.Write([]byte{0x1f})
}
