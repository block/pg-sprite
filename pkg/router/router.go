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
	// DispositionRewriteRequired: the planner says the submitted form
	// blocks and must run as a safer idiom, but no executable rewrite was
	// constructed (a multi-operation statement, or a pattern the planner
	// cannot build). The statement will be refused at execution rather
	// than run in its blocking form.
	DispositionRewriteRequired Disposition = "rewrite-required"
	// DispositionUnavailable: the change needs a backend this build does
	// not implement; the statement would be refused at execution.
	DispositionUnavailable Disposition = "unavailable"
	// DispositionRefuse: the planner refused the statement; no backend is
	// assigned.
	DispositionRefuse Disposition = "refuse"
)

// worse orders dispositions for aggregation:
// refuse > unavailable > rewrite-required > execute.
func worse(a, b Disposition) Disposition {
	rank := map[Disposition]int{
		DispositionExecute:         0,
		DispositionRewriteRequired: 1,
		DispositionUnavailable:     2,
		DispositionRefuse:          3,
	}
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
	// submitted statement. Execution contract: the steps run one at a
	// time, in order, each in its own implicit transaction — never wrapped
	// in an enclosing transaction block, which the CONCURRENTLY forms
	// refuse. Empty for non-native routes and for statements the engine
	// will not run (DispositionRewriteRequired).
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
	if st.Backend == BackendNative {
		sql, ok := nativeExecSQL(p)
		if !ok {
			st.Disposition = DispositionRewriteRequired
			return st
		}
		st.ExecSQL = sql
	}
	st.Disposition = DispositionExecute
	return st
}

// nativeExecSQL is the literal SQL the native backend would run for a
// native-routed statement: the planner's safer sequence when it constructed
// one (only single-operation statements carry one), otherwise the submitted
// form — but only when every decision is safe to run as submitted. A
// safer-idiom decision without a constructed rewrite yields no executable
// SQL: running the submitted form would falsify the plan's own reason.
func nativeExecSQL(p planner.Plan) ([]string, bool) {
	if len(p.Decisions) == 1 && len(p.Decisions[0].SaferSQL) > 0 {
		return p.Decisions[0].SaferSQL, true
	}
	for _, d := range p.Decisions {
		if !d.ExecutableAsSubmitted() {
			return nil, false
		}
	}
	return []string{p.Statement}, true
}
