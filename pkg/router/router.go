// Package router assigns every classified statement to an execution
// backend. It sits between the planner (which decides what must change and
// how PostgreSQL would treat it) and the executors (which decide how a
// change actually runs), and is the single place migration policy lives:
// which backends exist, which are available, and what happens to a
// statement whose backend is not built yet. Callers branch on the typed
// Backend and Disposition, never on prose.
//
// This is a periphery package (see SAFETY.md): a routed plan is a request.
// Executors enforce their own protections regardless of the route.
package router

import "github.com/block/pg-sprite/pkg/planner"

// Backend identifies an execution strategy.
type Backend string

// The backends the router can assign. A refused statement has no backend.
const (
	// BackendNative runs the change as direct PostgreSQL DDL (the safer
	// online idiom when the planner constructed one) under bounded
	// lock_timeout / statement_timeout.
	BackendNative Backend = "native"
	// BackendCopyAndSwap performs the change as a shadow-table copy with a
	// logical-replication catch-up and a locked cutover swap.
	BackendCopyAndSwap Backend = "copy-and-swap"
)

// available reports whether the backend is implemented in this build. This
// is the routing policy for backends: copy-and-swap is a known strategy the
// planner routes to, but until its executor exists the router marks the
// statement unavailable rather than pretending it could run.
func available(b Backend) bool {
	return b == BackendNative
}

// Disposition is what would happen to one statement if the routed plan were
// executed now.
type Disposition string

// The dispositions a routed statement can carry.
const (
	// DispositionExecute: the assigned backend is available; the statement
	// would run.
	DispositionExecute Disposition = "execute"
	// DispositionUnavailable: the change needs a backend this build does
	// not implement; the statement would be refused at execution.
	DispositionUnavailable Disposition = "unavailable"
	// DispositionRefuse: the planner refused the statement; no backend is
	// assigned.
	DispositionRefuse Disposition = "refuse"
)

// worse orders dispositions for aggregation: refuse > unavailable > execute.
func worse(a, b Disposition) Disposition {
	rank := map[Disposition]int{DispositionExecute: 0, DispositionUnavailable: 1, DispositionRefuse: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Statement is one routed statement: the planner's classification plus the
// backend assignment and the literal SQL the native backend would execute.
type Statement struct {
	planner.Plan
	// Backend is the assigned execution strategy; empty for refusals.
	Backend Backend `json:"backend,omitempty"`
	// Disposition is what execution would do with the statement now.
	Disposition Disposition `json:"disposition"`
	// ExecSQL is the ordered SQL the native backend would run: the
	// planner's safer sequence when it constructed one, otherwise the
	// submitted statement. Empty for non-native routes.
	ExecSQL []string `json:"exec_sql,omitempty"`
}

// Plan is the routed plan for an ordered statement list: one routed
// statement per input plan plus the aggregate disposition (the worst of its
// statements — one unavailable backend makes the whole plan unavailable,
// one refusal refuses it).
type Plan struct {
	// Statements are the routed statements, in input order.
	Statements []Statement `json:"statements"`
	// Disposition is the aggregate disposition.
	Disposition Disposition `json:"disposition"`
}

// Route assigns a backend to every classified statement. It is pure policy:
// no parsing, no database access — classification happens before, execution
// after.
func Route(plans []planner.Plan) Plan {
	routed := Plan{Statements: make([]Statement, 0, len(plans)), Disposition: DispositionExecute}
	for _, p := range plans {
		st := routeStatement(p)
		routed.Disposition = worse(routed.Disposition, st.Disposition)
		routed.Statements = append(routed.Statements, st)
	}
	return routed
}

// routeStatement maps one classified statement to its backend and
// disposition.
func routeStatement(p planner.Plan) Statement {
	st := Statement{Plan: p}
	switch p.Route {
	case planner.RouteNative:
		st.Backend = BackendNative
		st.ExecSQL = nativeExecSQL(p)
	case planner.RouteCopyAndSwap:
		st.Backend = BackendCopyAndSwap
	case planner.RouteRefuse:
		st.Disposition = DispositionRefuse
		return st
	default:
		// An unknown route is a planner/router version skew; refuse it
		// rather than guess a backend.
		st.Disposition = DispositionRefuse
		return st
	}
	if !available(st.Backend) {
		st.Disposition = DispositionUnavailable
		return st
	}
	st.Disposition = DispositionExecute
	return st
}

// nativeExecSQL is the literal SQL the native backend would run for a
// native-routed statement: the planner's safer sequence when it constructed
// one (only single-operation statements carry one), otherwise the submitted
// form.
func nativeExecSQL(p planner.Plan) []string {
	if len(p.Decisions) == 1 && len(p.Decisions[0].SaferSQL) > 0 {
		return p.Decisions[0].SaferSQL
	}
	return []string{p.Statement}
}
