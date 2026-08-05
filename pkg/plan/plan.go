// Package plan defines the machine-readable dry-run plan report: the
// stable JSON contract an operator or orchestrator consumes to decide
// whether and how a change would execute. Both front doors emit it — the
// imperative migrate --dry-run path and the declarative diff path — so a
// consumer parses one shape regardless of how the plan was derived.
package plan

import (
	"github.com/block/pg-sprite/pkg/planner"
	"github.com/block/pg-sprite/pkg/router"
)

// FormatVersion identifies the report contract. A consumer must reject a
// report whose version it does not understand instead of guessing at the
// field semantics.
const FormatVersion = 1

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

// Statement is one planned statement: the SQL, its classification, and
// what execution would do with it now.
type Statement struct {
	// SQL is the submitted statement (alter) or the derived convergence
	// statement (diff).
	SQL string `json:"sql"`
	// Destructive marks statements that drop live structure.
	Destructive bool `json:"destructive,omitempty"`
	// Route is the planner's aggregate route for the statement.
	Route planner.Route `json:"route"`
	// Backend is the assigned execution strategy; empty for refusals.
	Backend router.Backend `json:"backend,omitempty"`
	// Disposition is what execution would do with the statement now.
	Disposition router.Disposition `json:"disposition"`
	// Decisions are the planner's per-operation classifications.
	Decisions []planner.Decision `json:"decisions"`
	// ExecSQL is the ordered SQL the native backend would run — the safer
	// sequence when the planner constructed one. Empty for non-native
	// routes.
	ExecSQL []string `json:"exec_sql,omitempty"`
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
	// TableExists reports whether the live table was found. It is set
	// only by sources that introspect for existence (diff); when false,
	// the statements are the full desired schema.
	TableExists *bool `json:"table_exists,omitempty"`
	// Disposition is the aggregate disposition across all statements:
	// what would happen if the engine executed this plan now.
	Disposition router.Disposition `json:"disposition"`
	// Statements is the ordered plan; empty means there is nothing to do.
	Statements []Statement `json:"statements"`
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

// FromRouted converts one routed statement into a plan statement. Fields
// the router does not know (Destructive) stay at their zero value for the
// caller to set.
func FromRouted(rs router.Statement) Statement {
	return Statement{
		SQL:         rs.Statement,
		Route:       rs.Route,
		Backend:     rs.Backend,
		Disposition: rs.Disposition,
		Decisions:   rs.Decisions,
		ExecSQL:     rs.ExecSQL,
	}
}
